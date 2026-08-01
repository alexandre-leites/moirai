from __future__ import annotations

import os
import unittest
from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import UUID, uuid4

import asyncpg

from moirai.domain.control_plane import AuthenticationError
from moirai.persistence.authentication import AsyncpgAuthentication
from moirai.persistence.control_plane import AsyncpgControlPlane
from moirai.persistence.migrations import MigrationRunner
from moirai.workflows.persistence import AsyncpgWorkflowPersistence

_DATABASE_URL_ENV = "LOOP_TEST_DATABASE_URL"
_NOW = datetime(2026, 1, 1, tzinfo=UTC)


class PostgreSQLPersistenceIntegrationTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        database_url = os.environ.get(_DATABASE_URL_ENV)
        if not database_url:
            self.skipTest(f"{_DATABASE_URL_ENV} is not configured")
        self.pool = await asyncpg.create_pool(database_url)
        self.addAsyncCleanup(self.pool.close)
        self.migrations = MigrationRunner(self.pool)
        await self.migrations.run()

    async def test_migrations_are_recorded_and_idempotent(self) -> None:
        expected = [path.stem for path in sorted(MigrationRunner.MIGRATIONS_DIR.glob("*.sql"))]
        recorded = await self.pool.fetch("SELECT version, name FROM app.schema_version ORDER BY version")

        self.assertEqual(
            [f"{int(row['version']):03d}_{row['name']}" for row in recorded],
            expected,
        )
        self.assertEqual(await self.migrations.run(), [])

    async def test_control_plane_persists_project_runner_issue_and_lease_lifecycle(self) -> None:
        control_plane = AsyncpgControlPlane(self.pool)
        suffix = uuid4().hex
        # schedule() picks one online runner ordered by id, so a runner this
        # suite left online in an earlier run would be assigned the job instead
        # of the one registered below and the assertion would fail against every
        # database but a virgin one.
        await self._retire_leftover_runners()
        project = await control_plane.create_project(
            f"integration-{suffix}",
            "managed_clone",
            "https://example.test/integration.git",
            None,
            "main",
            {"linux"},
            _NOW,
        )
        project_id = project["id"]

        self.assertIn(project, await control_plane.list_projects())
        enabled_projects = await control_plane.list_enabled_projects()
        self.assertEqual(enabled_projects[0].id, project_id)
        self.assertEqual(enabled_projects[0].required_runner_labels, frozenset({"linux"}))

        await control_plane.upsert_issue(
            project_id=project_id,
            external_id="42",
            title="Exercise PostgreSQL persistence",
            body="",
            state="open",
            labels=["agent:ready"],
            priority=100,
            eligible=True,
            human_approval_required=False,
            external_created_at=_NOW,
            external_updated_at=_NOW,
            now=_NOW,
        )
        issue = await self.pool.fetchrow(
            "SELECT id FROM app.issues WHERE project_id = $1 AND external_id = $2",
            project_id,
            "42",
        )
        assert issue is not None
        issue_id = str(issue["id"])
        await control_plane.set_issue_labels(issue_id, ["agent:ready", "agent:priority:100"])
        self.assertEqual(
            await control_plane.get_issue_labels(issue_id),
            ["agent:priority:100", "agent:ready"],
        )

        token = await control_plane.create_registration_token(
            {"linux"}, _NOW + timedelta(minutes=30), now=_NOW
        )
        runner, credential = await control_plane.register_runner(
            token, f"runner-{suffix}", {"linux"}, _NOW, capacity=2
        )
        connected = await control_plane.heartbeat(runner.id, credential, _NOW)
        self.assertTrue(connected.connected)
        self.assertEqual(connected.capacity, 2)

        scheduled = await control_plane.schedule(_NOW, timedelta(minutes=5))
        self.assertIsNotNone(scheduled)
        assert scheduled is not None
        self.assertEqual(scheduled.assignment.issue.id, issue_id)
        self.assertEqual(scheduled.assignment.runner.id, runner.id)

        lease = await control_plane.accept_offer(scheduled.offer.job_id, runner.id, _NOW)
        renewed = await control_plane.renew_lease(
            lease.job_id,
            runner.id,
            lease.generation,
            _NOW + timedelta(minutes=10),
            _NOW,
        )
        self.assertEqual(renewed.generation, lease.generation)
        job = await self.pool.fetchrow(
            "SELECT status, lease_expires_at FROM app.jobs WHERE id = $1",
            lease.job_id,
        )
        assert job is not None
        self.assertEqual(job["status"], "preparing")
        self.assertEqual(job["lease_expires_at"], _NOW + timedelta(minutes=10))

    async def test_label_reconciliation_reads_only_the_newest_workflow_run_per_issue(self) -> None:
        control_plane = AsyncpgControlPlane(self.pool)
        suffix = uuid4().hex
        project = await control_plane.create_project(
            f"labels-{suffix}",
            "managed_clone",
            "https://example.test/labels.git",
            None,
            "main",
            {"linux"},
            _NOW,
        )
        project_id = project["id"]
        self.addAsyncCleanup(self._delete_project, project_id)
        delivered_issue = await self._create_issue(control_plane, project_id, "77")
        blocked_issue = await self._create_issue(control_plane, project_id, "78")
        await self._create_workflow_run(project_id, delivered_issue, "blocked", _NOW - timedelta(hours=2))
        await self._create_workflow_run(project_id, delivered_issue, "implementing", _NOW - timedelta(hours=1))
        await self._create_workflow_run(project_id, delivered_issue, "completed", _NOW)
        await self._create_workflow_run(project_id, blocked_issue, "blocked", _NOW)

        runs = await control_plane.list_latest_workflow_runs_for_project(project_id)

        self.assertEqual(
            {str(run["issue_id"]): run["status"] for run in runs},
            {delivered_issue: "completed", blocked_issue: "blocked"},
        )
        self.assertEqual([run["issue_id"] for run in runs], sorted(str(run["issue_id"]) for run in runs))
        self.assertEqual(runs, await control_plane.list_latest_workflow_runs_for_project(project_id))

    async def _retire_leftover_runners(self) -> None:
        """Takes every runner this database still reports online offline.

        Runners outlive the tests that register them — they belong to no
        project, so the per-project cleanups below never touch them. Marking
        them offline is enough to keep them out of `schedule()`, and is safer
        than deleting rows that jobs and leases still reference.
        """
        await self.pool.execute("UPDATE app.runners SET status = 'offline' WHERE status = 'online'")

    async def _delete_project(self, project_id: str) -> None:
        for table in ("workflow_runs", "issues"):
            await self.pool.execute(f"DELETE FROM app.{table} WHERE project_id = $1", UUID(project_id))
        await self.pool.execute("DELETE FROM app.projects WHERE id = $1", UUID(project_id))

    async def _create_issue(self, control_plane: AsyncpgControlPlane, project_id: str, external_id: str) -> str:
        await control_plane.upsert_issue(
            project_id=project_id,
            external_id=external_id,
            title=f"Issue {external_id}",
            body="",
            state="open",
            labels=["agent:ready", "bug"],
            priority=10,
            eligible=True,
            human_approval_required=False,
            external_created_at=_NOW,
            external_updated_at=_NOW,
            now=_NOW,
        )
        record = await self.pool.fetchrow(
            "SELECT id FROM app.issues WHERE project_id = $1 AND external_id = $2",
            project_id,
            external_id,
        )
        assert record is not None
        return str(record["id"])

    async def _create_workflow_run(
        self, project_id: str, issue_id: str, status: str, created_at: datetime
    ) -> None:
        run_id = uuid4()
        await self.pool.execute(
            """
            INSERT INTO app.workflow_runs
                (id, project_id, issue_id, thread_id, status, current_phase, created_at, updated_at)
            VALUES ($1, $2, $3, $4, $5, $5, $6, $6)
            """,
            run_id,
            UUID(project_id),
            UUID(issue_id),
            str(run_id),
            status,
            created_at,
        )


class AccountManagementIntegrationTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        database_url = os.environ.get(_DATABASE_URL_ENV)
        if not database_url:
            self.skipTest(f"{_DATABASE_URL_ENV} is not configured")
        self.pool = await asyncpg.create_pool(database_url)
        self.addAsyncCleanup(self.pool.close)
        await MigrationRunner(self.pool).run()
        self.authentication = AsyncpgAuthentication(self.pool, session_ttl=timedelta(hours=1))
        suffix = uuid4().hex
        self.username = f"account-{suffix}"
        await self.authentication.create_user(
            username=self.username,
            password="Correct horse battery staple 9!",
            role="viewer",
            now=_NOW,
        )
        self.credentials = await self.authentication.login(self.username, "Correct horse battery staple 9!", _NOW)
        self.session_id = await self._session_id(self.credentials.session_token)

    async def _session_id(self, session_token: str) -> str:
        from moirai.persistence.authentication import _hash_token

        row = await self.pool.fetchrow(
            "SELECT id FROM app.user_sessions WHERE token_hash = $1",
            _hash_token(session_token),
        )
        return str(row["id"])

    async def test_update_account_changes_password_revokes_other_sessions_and_keeps_profile(self) -> None:
        from uuid import UUID

        from moirai.persistence.authentication import _hash_token

        other_session_id = uuid4()
        # app.user_sessions.token_hash is globally unique, so a literal token
        # here would collide with the row a previous run of this test left
        # behind and the suite could only ever pass against a virgin database.
        other_session_token = f"other-session-token-{other_session_id.hex}"
        other_csrf_token = f"other-csrf-token-{other_session_id.hex}"
        await self.pool.execute(
            """
            INSERT INTO app.user_sessions
                (id, user_id, token_hash, csrf_token_hash, created_at, expires_at, last_seen_at)
            VALUES ($1, $2, $3, $4, $5, $6, $5)
            """,
            other_session_id,
            UUID(self.credentials.user_id),
            _hash_token(other_session_token),
            _hash_token(other_csrf_token),
            _NOW,
            _NOW + timedelta(hours=1),
        )
        profile = await self.authentication.update_account(
            user_id=self.credentials.user_id,
            keep_session_id=self.session_id,
            current_password="Correct horse battery staple 9!",
            new_password="Correct horse battery staple 10!",
            new_email="admin@example.com",
            display_name="Admin User",
            now=_NOW + timedelta(minutes=1),
        )
        self.assertEqual(profile.email, "admin@example.com")
        self.assertEqual(profile.display_name, "Admin User")
        session = await self.authentication.validate_session(
            self.credentials.session_token, self.credentials.csrf_token, _NOW + timedelta(minutes=2), True
        )
        self.assertEqual(session.email, "admin@example.com")
        with self.assertRaises(AuthenticationError):
            await self.authentication.validate_session(
                other_session_token, other_csrf_token, _NOW + timedelta(minutes=2), True
            )

    async def test_update_account_rejects_a_wrong_current_password(self) -> None:
        with self.assertRaisesRegex(AuthenticationError, "current password is incorrect"):
            await self.authentication.update_account(
                user_id=self.credentials.user_id,
                keep_session_id=self.session_id,
                current_password="wrong horse battery staple",
                new_password="Correct horse battery staple 10!",
                new_email="",
                display_name="",
                now=_NOW,
            )

    async def test_update_account_enforces_the_password_policy(self) -> None:
        with self.assertRaisesRegex(ValueError, "between 8 and 1024"):
            await self.authentication.update_account(
                user_id=self.credentials.user_id,
                keep_session_id=self.session_id,
                current_password="Correct horse battery staple 9!",
                new_password="short",
                new_email="",
                display_name="",
                now=_NOW,
            )


class OfferExpiryIntegrationTests(unittest.IsolatedAsyncioTestCase):
    """Offer expiry and rejection must not destroy an in-flight workflow run.
    See GitHub issue #91: an unanswered offer used to cancel the whole run,
    dropping the project lock and leaking the dispatched execution request,
    even when the run already had a pushed branch and an open pull request.
    """

    async def asyncSetUp(self) -> None:
        database_url = os.environ.get(_DATABASE_URL_ENV)
        if not database_url:
            self.skipTest(f"{_DATABASE_URL_ENV} is not configured")
        self.pool = await asyncpg.create_pool(database_url)
        self.addAsyncCleanup(self.pool.close)
        self.addAsyncCleanup(self._disable_seeded_projects)
        await MigrationRunner(self.pool).run()
        self.control_plane = AsyncpgControlPlane(self.pool)

    async def _disable_seeded_projects(self) -> None:
        """Leaves the shared database schedulable-neutral for other tests."""
        await self.pool.execute("UPDATE app.projects SET enabled = false")

    async def _seed(self) -> tuple[str, str, str]:
        """Creates an isolated project, eligible issue, and online runner.

        Scheduling queries are global, so every project seeded by an earlier
        test is disabled first and the runner label is unique per test: the
        only candidate `schedule()` can pick is the one this test created.
        """
        await self.pool.execute("UPDATE app.projects SET enabled = false")
        suffix = uuid4().hex[:12]
        label = f"offer-{suffix}"
        project = await self.control_plane.create_project(
            f"offer-expiry-{suffix}",
            "managed_clone",
            "https://example.test/offer-expiry.git",
            None,
            "main",
            {label},
            _NOW,
        )
        project_id = str(project["id"])
        await self.control_plane.upsert_issue(
            project_id=project_id,
            external_id=f"issue-{suffix}",
            title="Offer expiry must not cancel in-flight workflows",
            body="",
            state="open",
            labels=["agent:ready"],
            priority=100,
            eligible=True,
            human_approval_required=False,
            external_created_at=_NOW,
            external_updated_at=_NOW,
            now=_NOW,
        )
        issue = await self.pool.fetchrow(
            "SELECT id FROM app.issues WHERE project_id = $1",
            UUID(project_id),
        )
        assert issue is not None
        token = await self.control_plane.create_registration_token(
            {label}, _NOW + timedelta(minutes=30), now=_NOW
        )
        runner, credential = await self.control_plane.register_runner(
            token, f"runner-{suffix}", {label}, _NOW
        )
        await self.control_plane.heartbeat(runner.id, credential, _NOW)
        return project_id, str(issue["id"]), runner.id

    async def _in_flight_run(self) -> tuple[str, str, str, str]:
        """Drives a seeded project to a run with real progress waiting for a
        queued execution request: a branch, an open pull request, and a job
        that finished its previous execution."""
        project_id, _, runner_id = await self._seed()
        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert scheduled is not None
        job_id = scheduled.offer.job_id
        workflow_run_id = scheduled.workflow.id
        await self.control_plane.accept_offer(job_id, runner_id, _NOW)
        await self.pool.execute(
            """
            UPDATE app.workflow_runs
            SET status = 'ai_review', current_phase = 'ai_review', branch_name = 'moirai/issue-91',
                pull_request_external_id = '4242', pull_request_url = 'https://example.test/pull/4242',
                total_agent_executions = 3
            WHERE id = $1
            """,
            UUID(workflow_run_id),
        )
        await self.pool.execute(
            "UPDATE app.jobs SET status = 'completed', finished_at = $2 WHERE id = $1",
            UUID(job_id),
            _NOW,
        )
        request_id = uuid4()
        await self.pool.execute(
            """
            INSERT INTO app.workflow_execution_requests
                (id, workflow_run_id, role, attempt, status, created_at)
            VALUES ($1, $2, 'reviewer', 1, 'queued', $3)
            """,
            request_id,
            UUID(workflow_run_id),
            _NOW,
        )
        return project_id, workflow_run_id, job_id, str(request_id)

    async def _workflow_run(self, workflow_run_id: str) -> Any:
        record = await self.pool.fetchrow(
            """
            SELECT status, current_phase, terminal_reason, blocking_reason, branch_name,
                   pull_request_external_id
            FROM app.workflow_runs WHERE id = $1
            """,
            UUID(workflow_run_id),
        )
        assert record is not None
        return record

    async def _holds_project_lock(self, project_id: str, workflow_run_id: str) -> bool:
        lock = await self.pool.fetchrow(
            "SELECT 1 FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
            UUID(project_id),
            UUID(workflow_run_id),
        )
        return lock is not None

    async def test_expired_offer_requeues_an_in_flight_run(self) -> None:
        project_id, workflow_run_id, job_id, request_id = await self._in_flight_run()
        offered = await self.control_plane.schedule_execution(_NOW, timedelta(seconds=30))
        assert offered is not None
        self.assertEqual(offered.offer.job_id, job_id)

        expired = await self.control_plane.expire_offers(_NOW + timedelta(seconds=31))

        self.assertIn(job_id, expired)
        run = await self._workflow_run(workflow_run_id)
        self.assertEqual(run["status"], "ai_review")
        self.assertEqual(run["current_phase"], "ai_review")
        self.assertIsNone(run["terminal_reason"])
        self.assertEqual(run["branch_name"], "moirai/issue-91")
        self.assertEqual(run["pull_request_external_id"], "4242")
        self.assertTrue(await self._holds_project_lock(project_id, workflow_run_id))
        request = await self.pool.fetchrow(
            "SELECT status, dispatched_at FROM app.workflow_execution_requests WHERE id = $1",
            UUID(request_id),
        )
        assert request is not None
        self.assertEqual(request["status"], "queued")
        self.assertIsNone(request["dispatched_at"])

        reoffered = await self.control_plane.schedule_execution(
            _NOW + timedelta(seconds=32), timedelta(seconds=30)
        )
        assert reoffered is not None
        self.assertEqual(reoffered.offer.job_id, job_id)

    async def test_rejected_offer_requeues_an_in_flight_run(self) -> None:
        project_id, workflow_run_id, job_id, request_id = await self._in_flight_run()
        offered = await self.control_plane.schedule_execution(_NOW, timedelta(seconds=30))
        assert offered is not None

        await self.control_plane.reject_offer(job_id, offered.offer.runner_id, _NOW)

        run = await self._workflow_run(workflow_run_id)
        self.assertEqual(run["status"], "ai_review")
        self.assertTrue(await self._holds_project_lock(project_id, workflow_run_id))
        request = await self.pool.fetchrow(
            "SELECT status FROM app.workflow_execution_requests WHERE id = $1",
            UUID(request_id),
        )
        assert request is not None
        self.assertEqual(request["status"], "queued")
        reoffered = await self.control_plane.schedule_execution(
            _NOW + timedelta(seconds=1), timedelta(seconds=30)
        )
        assert reoffered is not None
        self.assertEqual(reoffered.offer.job_id, job_id)

    async def test_expired_recovery_offer_returns_the_run_to_recovering(self) -> None:
        """A run recovered from lease expiry has no queued execution request,
        so an unanswered recovery offer must go back to `recovering` for
        recover_one to pick up rather than cancel the run."""
        project_id, _, runner_id = await self._seed()
        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert scheduled is not None
        job_id = scheduled.offer.job_id
        workflow_run_id = scheduled.workflow.id
        await self.control_plane.accept_offer(job_id, runner_id, _NOW)
        await self.pool.execute(
            """
            UPDATE app.workflow_runs
            SET status = 'implementing', current_phase = 'implementing', branch_name = 'moirai/issue-91'
            WHERE id = $1
            """,
            UUID(workflow_run_id),
        )
        self.assertEqual(await self.control_plane.expire_leases(_NOW + timedelta(minutes=10)), (job_id,))
        await self.pool.execute(
            "UPDATE app.runners SET status = 'online' WHERE id = $1",
            UUID(runner_id),
        )
        recovered = await self.control_plane.recover_one(_NOW + timedelta(minutes=10), timedelta(seconds=30))
        assert recovered is not None
        self.assertEqual(recovered.offer.job_id, job_id)

        expired = await self.control_plane.expire_offers(_NOW + timedelta(minutes=11))

        self.assertIn(job_id, expired)
        run = await self._workflow_run(workflow_run_id)
        self.assertEqual(run["status"], "recovering")
        self.assertIsNone(run["terminal_reason"])
        self.assertEqual(run["branch_name"], "moirai/issue-91")
        self.assertTrue(await self._holds_project_lock(project_id, workflow_run_id))
        job = await self.pool.fetchrow("SELECT status FROM app.jobs WHERE id = $1", UUID(job_id))
        assert job is not None
        self.assertEqual(job["status"], "recovering")
        reoffered = await self.control_plane.recover_one(
            _NOW + timedelta(minutes=11), timedelta(seconds=30)
        )
        assert reoffered is not None
        self.assertEqual(reoffered.offer.job_id, job_id)

    async def test_an_accepted_run_that_reported_nothing_is_never_treated_as_bootstrap(self) -> None:
        """A runner that accepts an offer and then dies silently -- a bad
        image, a crash-looping agent, broken workspace prep -- must keep the
        bounded outcome it had before issue #141: re-offered by `recover_one`
        and eventually blocked by the unanswered-offer limit.

        The bootstrap arm cancels the run, releases the project lock and leaves
        the issue eligible, so `schedule()` immediately builds a *new* run with
        a *new* job -- and the unanswered-offer streak is counted per job, so
        the budget that is supposed to stop this resets on every cycle. Once
        `accept_offer` stopped overwriting `current_phase`, the phase alone no
        longer distinguished the two cases; the predicate tests `app.job_offers`
        for an acceptance instead.
        """
        project_id, issue_id, runner_id = await self._seed()
        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert scheduled is not None
        job_id = scheduled.offer.job_id
        workflow_run_id = scheduled.workflow.id
        # Accepted, then silence: no `started` event, so no execution row, no
        # branch, no execution request. Every other bootstrap guard holds.
        await self.control_plane.accept_offer(job_id, runner_id, _NOW)
        self.assertEqual(await self.control_plane.expire_leases(_NOW + timedelta(minutes=10)), (job_id,))
        await self.pool.execute(
            "UPDATE app.runners SET status = 'online' WHERE id = $1", UUID(runner_id)
        )
        recovered = await self.control_plane.recover_one(
            _NOW + timedelta(minutes=10), timedelta(seconds=30)
        )
        assert recovered is not None
        self.assertEqual(recovered.offer.job_id, job_id)

        self.assertIn(job_id, await self.control_plane.expire_offers(_NOW + timedelta(minutes=11)))

        run = await self._workflow_run(workflow_run_id)
        self.assertEqual(run["status"], "recovering")
        self.assertIsNone(run["terminal_reason"])
        self.assertTrue(await self._holds_project_lock(project_id, workflow_run_id))
        # The issue is still owned by this run, so nothing re-enters the global
        # queue behind its back and the per-job unanswered streak keeps counting.
        self.assertIsNone(await self.control_plane.schedule(_NOW + timedelta(minutes=11), timedelta(minutes=5)))
        self.assertEqual(
            int(
                await self.pool.fetchval(
                    "SELECT COUNT(*) FROM app.workflow_runs WHERE issue_id = $1", UUID(issue_id)
                )
            ),
            1,
        )

    async def test_expired_bootstrap_offer_cancels_the_run_and_releases_the_lock(self) -> None:
        project_id, issue_id, _ = await self._seed()
        scheduled = await self.control_plane.schedule(_NOW, timedelta(seconds=30))
        assert scheduled is not None
        self.assertEqual(scheduled.assignment.issue.id, issue_id)

        expired = await self.control_plane.expire_offers(_NOW + timedelta(seconds=31))

        self.assertIn(scheduled.offer.job_id, expired)
        run = await self._workflow_run(scheduled.workflow.id)
        self.assertEqual(run["status"], "cancelled")
        self.assertEqual(run["terminal_reason"], "offer_expired")
        self.assertFalse(await self._holds_project_lock(project_id, scheduled.workflow.id))
        rescheduled = await self.control_plane.schedule(
            _NOW + timedelta(seconds=32), timedelta(seconds=30)
        )
        assert rescheduled is not None
        self.assertEqual(rescheduled.assignment.issue.id, issue_id)

    async def test_repeated_unanswered_offers_block_the_run_after_the_grace_period(self) -> None:
        control_plane = AsyncpgControlPlane(
            self.pool, unanswered_offer_limit=3, unanswered_offer_grace=timedelta(minutes=10)
        )
        project_id, workflow_run_id, job_id, request_id = await self._in_flight_run()

        for index in range(2):
            moment = _NOW + timedelta(minutes=index)
            offered = await control_plane.schedule_execution(moment, timedelta(seconds=30))
            assert offered is not None
            self.assertEqual(offered.offer.job_id, job_id)
            self.assertIn(job_id, await control_plane.expire_offers(moment + timedelta(seconds=31)))
            run = await self._workflow_run(workflow_run_id)
            self.assertEqual(run["status"], "ai_review")

        # Third strike, but still inside the grace window: keep retrying.
        offered = await control_plane.schedule_execution(_NOW + timedelta(minutes=2), timedelta(seconds=30))
        assert offered is not None
        await control_plane.expire_offers(_NOW + timedelta(minutes=2, seconds=31))
        run = await self._workflow_run(workflow_run_id)
        self.assertEqual(run["status"], "ai_review")
        self.assertTrue(await self._holds_project_lock(project_id, workflow_run_id))

        # Past the grace window the run stops ping-ponging and is blocked.
        offered = await control_plane.schedule_execution(_NOW + timedelta(minutes=20), timedelta(seconds=30))
        assert offered is not None
        await control_plane.expire_offers(_NOW + timedelta(minutes=20, seconds=31))

        run = await self._workflow_run(workflow_run_id)
        self.assertEqual(run["status"], "blocked")
        self.assertEqual(run["blocking_reason"], "unanswered_offer_limit")
        self.assertFalse(await self._holds_project_lock(project_id, workflow_run_id))
        request = await self.pool.fetchrow(
            "SELECT status FROM app.workflow_execution_requests WHERE id = $1",
            UUID(request_id),
        )
        assert request is not None
        self.assertEqual(request["status"], "expired")
        self.assertIsNone(
            await control_plane.schedule_execution(_NOW + timedelta(minutes=21), timedelta(seconds=30))
        )

    async def test_accepted_offer_resets_the_unanswered_offer_streak(self) -> None:
        control_plane = AsyncpgControlPlane(
            self.pool, unanswered_offer_limit=2, unanswered_offer_grace=timedelta()
        )
        _, workflow_run_id, job_id, _ = await self._in_flight_run()
        offered = await control_plane.schedule_execution(_NOW, timedelta(seconds=30))
        assert offered is not None
        await control_plane.expire_offers(_NOW + timedelta(seconds=31))

        accepted = await control_plane.schedule_execution(_NOW + timedelta(minutes=1), timedelta(seconds=30))
        assert accepted is not None
        await control_plane.accept_offer(job_id, accepted.offer.runner_id, _NOW + timedelta(minutes=1))
        await self.pool.execute(
            "UPDATE app.jobs SET status = 'completed' WHERE id = $1",
            UUID(job_id),
        )
        await self.pool.execute(
            """
            UPDATE app.workflow_runs SET status = 'ai_review', current_phase = 'ai_review' WHERE id = $1
            """,
            UUID(workflow_run_id),
        )
        await self.pool.execute(
            """
            UPDATE app.workflow_execution_requests SET status = 'queued', dispatched_at = NULL
            WHERE workflow_run_id = $1
            """,
            UUID(workflow_run_id),
        )

        reoffered = await control_plane.schedule_execution(_NOW + timedelta(minutes=2), timedelta(seconds=30))
        assert reoffered is not None
        await control_plane.expire_offers(_NOW + timedelta(minutes=2, seconds=31))

        run = await self._workflow_run(workflow_run_id)
        self.assertEqual(run["status"], "ai_review")


_PLANNER_READY: dict[str, Any] = {
    "status": "ready",
    "summary": "plan ready",
    "assumptions": [],
    "questions": [],
    "risk": "low",
    "acceptanceCriteria": [],
    "steps": [],
}


class _SingleIterationLeader:
    """Stands in for AsyncpgLeader: the maintenance loop's recovery arm only
    runs on the elected instance, and this test *is* that instance.

    Stopping the loop as soon as it claims leadership bounds it to exactly one
    iteration, so every assertion downstream is literally "within one
    interval".
    """

    def __init__(self, stop_event: Any) -> None:
        self._stop_event = stop_event
        self.iterations = 0
        self.closed = False

    async def is_leader(self) -> bool:
        self.iterations += 1
        self._stop_event.set()
        return True

    async def close(self) -> None:
        self.closed = True


class StalledRunRecoveryIntegrationTests(unittest.IsolatedAsyncioTestCase):
    """Execution requests must be closed so stalled-run recovery can fire.

    See GitHub issue #94 (platform review finding F7): `workflow_execution_requests`
    rows were only ever written as `queued` and then `dispatched`, so
    `find_stalled_workflow_runs`'s `NOT EXISTS (... 'queued', 'dispatched')`
    predicate could never hold for a run that had dispatched anything, and the
    maintenance loop's recovery arm was dead code.

    The fixtures here are also the only ones that drive the real loop end to
    end -- graph, scheduler, `accept_offer` and `accept_event` against real
    PostgreSQL, with nothing about `app.workflow_runs` patched by hand -- so
    the core-loop tests for issue #141 live here too.
    """

    async def asyncSetUp(self) -> None:
        database_url = os.environ.get(_DATABASE_URL_ENV)
        if not database_url:
            self.skipTest(f"{_DATABASE_URL_ENV} is not configured")
        self.pool = await asyncpg.create_pool(database_url)
        self.addAsyncCleanup(self.pool.close)
        self.addAsyncCleanup(self._disable_seeded_projects)
        await MigrationRunner(self.pool).run()
        self.control_plane = AsyncpgControlPlane(self.pool)
        self._checkpointer: Any = None

    async def _disable_seeded_projects(self) -> None:
        await self.pool.execute("UPDATE app.projects SET enabled = false")

    async def _delete_fixture(self, project_id: str, runner_id: str) -> None:
        """Removes everything this test seeded.

        These tests deliberately leave jobs holding live leases, and the
        control plane's sweeps (`expire_leases`, `recover_one`,
        `find_stalled_workflow_runs`) are global queries against a database
        shared with every other integration test, so the fixture cannot be
        left behind.
        """
        project = UUID(project_id)
        await self.pool.execute("DELETE FROM app.project_locks WHERE project_id = $1", project)
        await self.pool.execute("DELETE FROM app.jobs WHERE project_id = $1", project)
        await self.pool.execute("DELETE FROM app.workflow_runs WHERE project_id = $1", project)
        await self.pool.execute("DELETE FROM app.issues WHERE project_id = $1", project)
        await self.pool.execute("DELETE FROM app.projects WHERE id = $1", project)
        await self.pool.execute("DELETE FROM app.runners WHERE id = $1", UUID(runner_id))

    async def _runtime(self) -> Any:
        """The production workflow runtime.

        One checkpointer for the whole test, deliberately: it stands in for the
        durable Postgres saver, which survives the simulated crash. A fresh
        saver per call would leave the recovering runtime with an empty thread,
        so `ainvoke(None, config)` would replay the graph from START instead of
        resuming the suspended edge -- and the resume path is what these tests
        exist to exercise.
        """
        from langgraph.checkpoint.memory import InMemorySaver

        from moirai.workflows.runtime import build_persisted_runtime

        if self._checkpointer is None:
            self._checkpointer = InMemorySaver()
        return build_persisted_runtime(self.pool, checkpointer=self._checkpointer)

    @staticmethod
    def _advance(runtime: Any) -> Any:
        """The same adapter production uses (RunnerControlService._advance_workflow):
        a transition callback that re-enters the graph runtime."""

        async def advance(workflow_run_id: str, new_status: str, updates: dict[str, object]) -> None:
            await runtime.run(workflow_run_id, {"status": new_status, **updates})

        return advance

    async def _planning_run(self) -> tuple[str, str, str, str]:
        """Drives a seeded project to a run whose planner execution has been
        dispatched to a runner and accepted: the state a terminal planner event
        arrives in.

        Returns (workflow_run_id, job_id, runner_id, planner_request_id).
        """
        await self.pool.execute("UPDATE app.projects SET enabled = false")
        suffix = uuid4().hex[:12]
        label = f"stall-{suffix}"
        project = await self.control_plane.create_project(
            f"stalled-run-{suffix}",
            "managed_clone",
            "https://example.test/stalled-run.git",
            None,
            "main",
            {label},
            _NOW,
        )
        project_id = str(project["id"])
        await self.control_plane.upsert_issue(
            project_id=project_id,
            external_id=f"issue-{suffix}",
            title="Close execution requests on terminal events",
            body="",
            state="open",
            labels=["agent:ready"],
            priority=100,
            eligible=True,
            human_approval_required=False,
            external_created_at=_NOW,
            external_updated_at=_NOW,
            now=_NOW,
        )
        token = await self.control_plane.create_registration_token(
            {label}, _NOW + timedelta(minutes=30), now=_NOW
        )
        runner, credential = await self.control_plane.register_runner(
            token, f"runner-{suffix}", {label}, _NOW
        )
        self.addAsyncCleanup(self._delete_fixture, project_id, runner.id)
        await self.control_plane.heartbeat(runner.id, credential, _NOW)

        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert scheduled is not None
        job_id = scheduled.offer.job_id
        workflow_run_id = scheduled.workflow.id
        await self.control_plane.accept_offer(job_id, runner.id, _NOW)

        # The graph's own first pass: prepare -> plan queues a planner request
        # and suspends on the edge out of `plan`.
        runtime = await self._runtime()
        state = await runtime.run(workflow_run_id, {"status": "preparing"})
        self.assertEqual(state["status"], "planning")
        self.assertTrue(state["awaiting_execution"])
        request_id = await self.pool.fetchval(
            "SELECT id FROM app.workflow_execution_requests WHERE workflow_run_id = $1",
            UUID(workflow_run_id),
        )
        assert request_id is not None

        # The bootstrap job finished its offer, so schedule_execution() can
        # place the planner request on the same runner.
        await self.pool.execute(
            "UPDATE app.jobs SET status = 'completed', finished_at = $2 WHERE id = $1",
            UUID(job_id),
            _NOW,
        )
        offered = await self.control_plane.schedule_execution(_NOW, timedelta(minutes=5))
        assert offered is not None
        self.assertEqual(offered.offer.job_id, job_id)
        await self.control_plane.accept_offer(job_id, runner.id, _NOW)
        return workflow_run_id, job_id, runner.id, str(request_id)

    async def _deliver_planner_result(
        self, job_id: str, runner_id: str, request_id: str, on_transition: Any = None
    ) -> None:
        from moirai.domain.models import ExecutionEvent

        lease = await self.pool.fetchrow(
            "SELECT lease_generation FROM app.jobs WHERE id = $1", UUID(job_id)
        )
        assert lease is not None
        generation = int(lease["lease_generation"])
        for sequence, event_type, payload in (
            (1, "started", {}),
            (2, "completed", {"exitCode": 0, "result": _PLANNER_READY}),
        ):
            await self.control_plane.accept_event(
                ExecutionEvent(
                    job_id=job_id,
                    runner_id=runner_id,
                    lease_generation=generation,
                    event_sequence=sequence,
                    event_type=event_type,
                    execution_id=f"{request_id}-plan",
                    payload=payload,
                ),
                _NOW,
                on_transition=on_transition,
            )

    async def _implementing_run(self) -> tuple[str, str, str]:
        """Drives a run one phase further: the planner reported, the graph
        resumed and queued the developer execution, and that execution has been
        placed on a runner.

        Returns (workflow_run_id, job_id, runner_id).
        """
        workflow_run_id, job_id, runner_id, request_id = await self._planning_run()
        runtime = await self._runtime()
        await self._deliver_planner_result(
            job_id, runner_id, request_id, on_transition=self._advance(runtime)
        )
        self.assertEqual(await self._run_status(workflow_run_id), "implementing")
        offered = await self.control_plane.schedule_execution(_NOW, timedelta(minutes=5))
        assert offered is not None
        self.assertEqual(offered.offer.job_id, job_id)
        await self.control_plane.accept_offer(job_id, runner_id, _NOW)
        return workflow_run_id, job_id, runner_id

    async def _nodes_that_ran(self, workflow_run_id: str) -> int:
        """How many graph nodes have committed a transition for this run.

        Every node calls `AsyncpgWorkflowPersistence.transition`, which appends
        one `workflow_transition` event, so the count is a direct read of how
        much of the graph executed.
        """
        return int(
            await self.pool.fetchval(
                """
                SELECT COUNT(*) FROM app.workflow_events
                WHERE workflow_run_id = $1 AND event_type = 'workflow_transition'
                """,
                UUID(workflow_run_id),
            )
        )

    async def _run_status(self, workflow_run_id: str) -> str:
        status = await self.pool.fetchval(
            "SELECT status FROM app.workflow_runs WHERE id = $1", UUID(workflow_run_id)
        )
        return str(status)

    async def _run_phase(self, workflow_run_id: str) -> str:
        phase = await self.pool.fetchval(
            "SELECT current_phase FROM app.workflow_runs WHERE id = $1", UUID(workflow_run_id)
        )
        return str(phase)

    async def _job_status(self, job_id: str) -> str:
        status = await self.pool.fetchval(
            "SELECT status FROM app.jobs WHERE id = $1", UUID(job_id)
        )
        return str(status)

    async def _dispatched_request_id(self, workflow_run_id: str) -> str:
        """The request the run is currently waiting on, exactly as
        `build_task_packet` picks it."""
        value = await self.pool.fetchval(
            """
            SELECT id FROM app.workflow_execution_requests
            WHERE workflow_run_id = $1 AND status = 'dispatched'
            ORDER BY dispatched_at DESC, id DESC LIMIT 1
            """,
            UUID(workflow_run_id),
        )
        assert value is not None
        return str(value)

    async def _deliver_developer_result(
        self, job_id: str, runner_id: str, request_id: str, on_transition: Any = None
    ) -> None:
        """A successful developer execution, reported the way the runner does:
        `started`, then `completed` with the files it changed.

        The `result` document is what makes this a *delivery*. Since issue #105
        the orchestrator will not advance a run on an exit code alone: a
        terminal event whose agent result is missing or invalid, or that
        reports no changed files or outstanding `remainingWork`, is recorded as
        a non-delivery and the run holds its phase. Omitting it here would test
        that guard rather than the transition.
        """
        from moirai.domain.models import ExecutionEvent

        generation = int(
            await self.pool.fetchval(
                "SELECT lease_generation FROM app.jobs WHERE id = $1", UUID(job_id)
            )
        )
        result = {
            "protocolVersion": "1.0",
            "executionId": f"{request_id}-implement",
            "status": "completed",
            "summary": "implemented the requested change",
            "changedFiles": ["src/main.py"],
            "commandsRun": ["make test"],
            "remainingWork": [],
            "knownLimitations": [],
        }
        for sequence, event_type, payload in (
            (1, "started", {}),
            (
                2,
                "completed",
                {
                    "exitCode": 0,
                    "changedFiles": ["src/main.py"],
                    "commandsRun": ["make test"],
                    "result": result,
                },
            ),
        ):
            await self.control_plane.accept_event(
                ExecutionEvent(
                    job_id=job_id,
                    runner_id=runner_id,
                    lease_generation=generation,
                    event_sequence=sequence,
                    event_type=event_type,
                    execution_id=f"{request_id}-implement",
                    payload=payload,
                ),
                _NOW,
                on_transition=on_transition,
            )

    async def _counters(self, workflow_run_id: str) -> dict[str, int]:
        row = await self.pool.fetchrow(
            """
            SELECT planning_attempts, implementation_attempts, pipeline_repair_attempts,
                   review_cycles, ci_repair_attempts, total_agent_executions
            FROM app.workflow_runs WHERE id = $1
            """,
            UUID(workflow_run_id),
        )
        assert row is not None
        return {key: int(value) for key, value in dict(row).items()}

    async def _outbox_statuses(self, workflow_run_id: str) -> list[str]:
        rows = await self.pool.fetch(
            """
            SELECT status FROM app.workflow_transition_outbox
            WHERE workflow_run_id = $1 ORDER BY created_at
            """,
            UUID(workflow_run_id),
        )
        return [str(row["status"]) for row in rows]

    def _advance_only(self, runtime: Any, workflow_run_id: str) -> Any:
        """`_advance`, restricted to one run.

        `drain_pending_transitions` is a global query against a database shared
        with every other integration test, so an unrestricted callback would
        also hand other fixtures' rows to this test's graph runtime.
        """
        advance = self._advance(runtime)
        delivered: list[tuple[str, str, dict[str, object]]] = []

        async def advance_one(target: str, new_status: str, updates: dict[str, object]) -> None:
            if target != workflow_run_id:
                return
            delivered.append((target, new_status, dict(updates)))
            await advance(target, new_status, updates)

        advance_one.delivered = delivered  # type: ignore[attr-defined]
        return advance_one

    async def _request_statuses(self, workflow_run_id: str) -> list[tuple[str, int, str]]:
        """Every execution request for the run as (role, attempt, status).

        Ordered by the schema's own identity for a request -- `(role, attempt)`
        -- rather than by `created_at`, because the fixture drives the control
        plane on a fixed test clock while the workflow persistence layer
        timestamps its own writes with the wall clock.
        """
        rows = await self.pool.fetch(
            """
            SELECT role, attempt, status FROM app.workflow_execution_requests
            WHERE workflow_run_id = $1 ORDER BY role, attempt
            """,
            UUID(workflow_run_id),
        )
        return [(str(row["role"]), int(row["attempt"]), str(row["status"])) for row in rows]

    async def _run_maintenance_once(
        self, on_transition: Any, now: datetime
    ) -> tuple[list[tuple[str, str, dict[str, object]]], _SingleIterationLeader]:
        """Runs exactly one iteration of the real maintenance loop.

        `_SingleIterationLeader` stops the loop the moment it claims
        leadership, so the iteration runs to completion and the loop then
        exits: everything asserted afterwards happened within one interval.
        """
        import asyncio

        from moirai.main import _run_workflow_maintenance_loop

        stop_event = asyncio.Event()
        leader = _SingleIterationLeader(stop_event)
        recovered: list[tuple[str, str, dict[str, object]]] = []

        async def recording(workflow_run_id: str, new_status: str, updates: dict[str, object]) -> None:
            recovered.append((workflow_run_id, new_status, dict(updates)))
            await on_transition(workflow_run_id, new_status, updates)

        await asyncio.wait_for(
            _run_workflow_maintenance_loop(
                self.control_plane,
                recording,
                stop_event,
                lambda: now,
                timedelta(milliseconds=1),
                leader,
            ),
            timeout=30,
        )
        self.assertEqual(leader.iterations, 1)
        self.assertTrue(leader.closed)
        return recovered, leader

    async def _lose_the_job(self, job_id: str) -> None:
        """The runner's job goes away without the execution request being
        requeued -- the leak that pins a run outside the stall detector."""
        await self.pool.execute(
            "UPDATE app.jobs SET status = 'cancelled', finished_at = $2 WHERE id = $1",
            UUID(job_id),
            _NOW,
        )

    async def test_terminal_event_closes_the_dispatched_execution_request(self) -> None:
        """Acceptance criterion 1: once a phase's execution reports terminal,
        no queued/dispatched request row remains for that phase."""
        workflow_run_id, job_id, runner_id, request_id = await self._planning_run()
        self.assertEqual(
            await self._request_statuses(workflow_run_id), [("planner", 1, "dispatched")]
        )
        runtime = await self._runtime()

        await self._deliver_planner_result(
            job_id, runner_id, request_id, on_transition=self._advance(runtime)
        )

        # The planner phase left nothing schedulable behind; the developer
        # request the resumed graph queued is the only open row.
        self.assertEqual(
            await self._request_statuses(workflow_run_id),
            [("developer", 1, "queued"), ("planner", 1, "completed")],
        )
        self.assertEqual(
            await self.control_plane.find_stalled_workflow_runs(
                _NOW + timedelta(hours=1), timedelta(seconds=30)
            ),
            (),
        )

    async def test_a_redelivered_outbox_transition_changes_nothing(self) -> None:
        """Issue #96, acceptance criterion 1: delivering the same outbox
        transition twice is observationally identical to delivering it once.

        The crash modelled here is the one the outbox exists for: the graph was
        invoked and committed everything it does, and the process died before
        the row could be marked processed."""
        workflow_run_id, job_id, runner_id, request_id = await self._planning_run()
        runtime = await self._runtime()
        await self._deliver_planner_result(
            job_id, runner_id, request_id, on_transition=self._advance(runtime)
        )
        requests = await self._request_statuses(workflow_run_id)
        counters = await self._counters(workflow_run_id)
        status = await self._run_status(workflow_run_id)
        nodes = await self._nodes_that_ran(workflow_run_id)
        self.assertEqual(requests, [("developer", 1, "queued"), ("planner", 1, "completed")])
        await self.pool.execute(
            """
            UPDATE app.workflow_transition_outbox
            SET status = 'pending', processing_started_at = NULL, processed_at = NULL
            WHERE workflow_run_id = $1
            """,
            UUID(workflow_run_id),
        )

        advance = self._advance_only(runtime, workflow_run_id)
        await self.control_plane.drain_pending_transitions(advance, _NOW)

        self.assertEqual(len(advance.delivered), 1)
        self.assertEqual(await self._request_statuses(workflow_run_id), requests)
        self.assertEqual(await self._counters(workflow_run_id), counters)
        self.assertEqual(await self._run_status(workflow_run_id), status)
        # No node ran a second time, so the graph is still suspended on the
        # edge that is waiting for the developer execution.
        self.assertEqual(await self._nodes_that_ran(workflow_run_id), nodes)
        self.assertEqual(await self._outbox_statuses(workflow_run_id), ["processed"])

    async def test_a_redelivery_after_the_scheduler_claimed_the_request_changes_nothing(self) -> None:
        """The same replay, one step later: the scheduler has already offered
        the queued request, so the row this run is waiting on is `dispatched`.
        Matching only `queued` rows is what let a replay queue the same agent
        work twice."""
        workflow_run_id, job_id, runner_id, request_id = await self._planning_run()
        runtime = await self._runtime()
        await self._deliver_planner_result(
            job_id, runner_id, request_id, on_transition=self._advance(runtime)
        )
        await self.pool.execute(
            "UPDATE app.jobs SET status = 'completed', finished_at = $2 WHERE id = $1",
            UUID(job_id),
            _NOW,
        )
        offered = await self.control_plane.schedule_execution(_NOW, timedelta(minutes=5))
        assert offered is not None
        requests = await self._request_statuses(workflow_run_id)
        counters = await self._counters(workflow_run_id)
        self.assertEqual(requests, [("developer", 1, "dispatched"), ("planner", 1, "completed")])
        await self.pool.execute(
            """
            UPDATE app.workflow_transition_outbox
            SET status = 'pending', processing_started_at = NULL, processed_at = NULL
            WHERE workflow_run_id = $1
            """,
            UUID(workflow_run_id),
        )

        advance = self._advance_only(runtime, workflow_run_id)
        await self.control_plane.drain_pending_transitions(advance, _NOW)

        self.assertEqual(len(advance.delivered), 1)
        self.assertEqual(await self._request_statuses(workflow_run_id), requests)
        self.assertEqual(await self._counters(workflow_run_id), counters)

    async def test_an_outbox_row_stuck_in_processing_is_reclaimed(self) -> None:
        """Issue #96, acceptance criterion 2: no outbox row stays in
        `processing` indefinitely. The claim is committed before the delivery
        starts, so a drainer that dies mid-delivery cannot release its own
        row -- only the lease can."""
        workflow_run_id, job_id, runner_id, request_id = await self._planning_run()
        runtime = await self._runtime()
        await self._deliver_planner_result(
            job_id, runner_id, request_id, on_transition=self._advance(runtime)
        )
        await self.pool.execute(
            """
            UPDATE app.workflow_transition_outbox
            SET status = 'processing', processing_started_at = $2, processed_at = NULL
            WHERE workflow_run_id = $1
            """,
            UUID(workflow_run_id),
            _NOW,
        )

        held = self._advance_only(runtime, workflow_run_id)
        await self.control_plane.drain_pending_transitions(held, _NOW + timedelta(seconds=30))
        self.assertEqual(held.delivered, [])
        self.assertEqual(await self._outbox_statuses(workflow_run_id), ["processing"])

        reclaimed = self._advance_only(runtime, workflow_run_id)
        await self.control_plane.drain_pending_transitions(reclaimed, _NOW + timedelta(minutes=10))

        self.assertEqual(len(reclaimed.delivered), 1)
        self.assertEqual(await self._outbox_statuses(workflow_run_id), ["processed"])

    async def test_committed_transition_without_a_graph_invocation_is_recovered(self) -> None:
        """Acceptance criterion 2: a crash between committing a terminal
        transition and invoking the graph runtime is repaired by the
        maintenance loop within one interval."""
        workflow_run_id, job_id, runner_id, request_id = await self._planning_run()
        runtime = await self._runtime()

        # The crash: accept_event commits the transition and the outbox row,
        # then the process dies with the row claimed mid-drain ('processing',
        # with no lease recorded because the drainer never got that far).
        await self._deliver_planner_result(job_id, runner_id, request_id, on_transition=None)
        await self.pool.execute(
            "UPDATE app.workflow_transition_outbox SET status = 'processing' WHERE workflow_run_id = $1",
            UUID(workflow_run_id),
        )
        self.assertEqual(await self._run_status(workflow_run_id), "implementing")
        self.assertEqual(
            await self._request_statuses(workflow_run_id), [("planner", 1, "completed")]
        )
        self.assertEqual(
            await self.control_plane.find_stalled_workflow_runs(
                _NOW + timedelta(hours=1), timedelta(seconds=30)
            ),
            (workflow_run_id,),
        )

        nodes_before = await self._nodes_that_ran(workflow_run_id)
        recovered, _ = await self._run_maintenance_once(
            self._advance(runtime), _NOW + timedelta(hours=1)
        )

        # Exactly one node ran: the graph resumed from the suspended edge out
        # of `plan` (`aupdate_state` + `ainvoke(None, config)`). A replay from
        # START would have re-run `prepare` as well -- which is what happens if
        # the recovering runtime is handed a checkpointer that does not carry
        # the run's thread.
        self.assertEqual(await self._nodes_that_ran(workflow_run_id) - nodes_before, 1)
        # The stranded claim is reclaimed by the drain arm (issue #96), so the
        # run is recovered by the transition that was actually committed --
        # `plan_valid` and all -- rather than by the stalled-run arm's bare
        # `awaiting_execution` clear. The planner's verdict is not thrown away,
        # so the resumed `plan` edge routes forward into `implement` instead of
        # spending a second planning attempt on work that already succeeded.
        self.assertEqual(
            [entry for entry in recovered if entry[0] == workflow_run_id],
            [(workflow_run_id, "implementing", {
                "status": "implementing", "plan_valid": True, "awaiting_execution": False,
            })],
        )
        self.assertEqual(await self._outbox_statuses(workflow_run_id), ["processed"])
        self.assertEqual(
            await self._request_statuses(workflow_run_id),
            [("developer", 1, "queued"), ("planner", 1, "completed")],
        )
        self.assertEqual(
            await self.control_plane.find_stalled_workflow_runs(
                _NOW + timedelta(hours=2), timedelta(seconds=30)
            ),
            (),
        )

    async def test_lost_planner_execution_is_requeued_rather_than_left_dispatched(self) -> None:
        """A dispatched request whose job was released without requeueing it
        pins the run outside the stall detector forever."""
        workflow_run_id, job_id, _, _ = await self._planning_run()
        await self._lose_the_job(job_id)
        self.assertEqual(
            await self._request_statuses(workflow_run_id), [("planner", 1, "dispatched")]
        )
        runtime = await self._runtime()

        recovered, _ = await self._run_maintenance_once(
            self._advance(runtime), _NOW + timedelta(hours=1)
        )

        self.assertEqual(recovered, [])
        self.assertEqual(
            await self._request_statuses(workflow_run_id),
            [("planner", 1, "orphaned"), ("planner", 2, "queued")],
        )
        self.assertEqual(
            await self.control_plane.find_stalled_workflow_runs(
                _NOW + timedelta(hours=2), timedelta(seconds=30)
            ),
            (),
        )

    async def test_open_requests_on_a_terminal_run_are_closed(self) -> None:
        """A run that reached a terminal status must leave nothing schedulable
        behind: `schedule_execution` matches on the request alone, so an open
        row would keep offering work for a finished run."""
        workflow_run_id, job_id, _, _ = await self._planning_run()
        await self.pool.execute(
            """
            UPDATE app.workflow_execution_requests SET status = 'queued', dispatched_at = NULL
            WHERE workflow_run_id = $1
            """,
            UUID(workflow_run_id),
        )
        await self.pool.execute(
            """
            UPDATE app.workflow_runs
            SET status = 'blocked', current_phase = 'blocked', terminal_reason = 'test'
            WHERE id = $1
            """,
            UUID(workflow_run_id),
        )
        await self._lose_the_job(job_id)

        closed = await self.control_plane.close_orphaned_execution_requests(
            _NOW + timedelta(hours=1), timedelta(minutes=2)
        )

        self.assertEqual(closed, 1)
        self.assertEqual(
            await self._request_statuses(workflow_run_id), [("planner", 1, "orphaned")]
        )
        self.assertIsNone(
            await self.control_plane.schedule_execution(
                _NOW + timedelta(hours=1), timedelta(minutes=5)
            )
        )

    async def test_successful_developer_execution_advances_to_the_local_pipeline(self) -> None:
        """Issue #141, acceptance criterion: a successful developer terminal
        event delivered through `accept_event`, while the run holds the status
        `accept_offer` actually leaves behind, produces a workflow transition
        and resumes the graph.

        Nothing about `app.workflow_runs` is patched here: the run reaches
        `implement` through the real graph, `schedule_execution` offers the
        developer request and `accept_offer` accepts it, exactly as production
        does. Before the fix this committed nothing at all -- no transition, so
        the graph stayed suspended on the edge out of `implement`, the job
        stayed `running` until its lease expired, and `recover_one` re-offered
        the same work forever.
        """
        workflow_run_id, job_id, runner_id = await self._implementing_run()
        request_id = await self._dispatched_request_id(workflow_run_id)
        # What `accept_offer` leaves behind: the job's lifecycle status on the
        # run, with the phase the `implement` node committed intact underneath.
        self.assertEqual(await self._run_status(workflow_run_id), "preparing")
        self.assertEqual(await self._run_phase(workflow_run_id), "implementing")
        nodes_before = await self._nodes_that_ran(workflow_run_id)
        runtime = await self._runtime()

        await self._deliver_developer_result(
            job_id, runner_id, request_id, on_transition=self._advance(runtime)
        )

        self.assertEqual(await self._run_status(workflow_run_id), "local_pipeline")
        self.assertEqual(await self._run_phase(workflow_run_id), "local_pipeline")
        # The graph really resumed: `pipeline` ran and queued the deterministic
        # gate's own execution. `pipeline_passed` is deliberately not set by the
        # developer's exit code, so the pipeline execution is what decides it.
        self.assertEqual(await self._nodes_that_ran(workflow_run_id) - nodes_before, 1)
        self.assertEqual(
            await self._request_statuses(workflow_run_id),
            [
                ("developer", 1, "completed"),
                ("pipeline", 1, "queued"),
                ("planner", 1, "completed"),
            ],
        )
        # The job finished with its execution rather than being left `running`
        # for `expire_leases` to sweep into the re-offer loop.
        self.assertEqual(await self._job_status(job_id), "completed")
        self.assertEqual(await self._outbox_statuses(workflow_run_id), ["processed", "processed"])

    async def test_developer_execution_recovered_from_a_lease_expiry_still_transitions(self) -> None:
        """The same criterion one failure deeper. `expire_leases` fences the job
        and `recover_one` re-offers it, and the re-offer carries the *same*
        dispatched developer request -- so the terminal event it eventually
        produces still has to be able to transition. Stamping `current_phase`
        on either sweep is what turned this into the unbounded re-offer loop
        issue #141 describes."""
        workflow_run_id, job_id, runner_id = await self._implementing_run()

        expired = await self.control_plane.expire_leases(_NOW + timedelta(hours=1))
        self.assertIn(job_id, expired)
        self.assertEqual(await self._run_status(workflow_run_id), "recovering")
        self.assertEqual(await self._run_phase(workflow_run_id), "implementing")
        recovered = await self.control_plane.recover_one(
            _NOW + timedelta(hours=1), timedelta(minutes=5)
        )
        assert recovered is not None
        self.assertEqual(recovered.offer.job_id, job_id)
        packet = await self.control_plane.build_task_packet(recovered)
        self.assertEqual(packet["role"], "developer")
        await self.control_plane.accept_offer(job_id, runner_id, _NOW + timedelta(hours=1))
        self.assertEqual(await self._run_phase(workflow_run_id), "implementing")

        runtime = await self._runtime()
        await self._deliver_developer_result(
            job_id,
            runner_id,
            await self._dispatched_request_id(workflow_run_id),
            on_transition=self._advance(runtime),
        )

        self.assertEqual(await self._run_status(workflow_run_id), "local_pipeline")
        self.assertEqual(
            await self._request_statuses(workflow_run_id),
            [
                ("developer", 1, "completed"),
                ("pipeline", 1, "queued"),
                ("planner", 1, "completed"),
            ],
        )

    async def test_lost_execution_never_advances_the_graph_past_its_phase(self) -> None:
        """The `implement` node's outgoing edge is unconditional, so recovering
        a lost developer execution by clearing `awaiting_execution` and
        resuming would dispatch the *pipeline* -- skipping the implementation
        nobody ran. (The same shape on `push` would open and merge a pull
        request for a branch that was never pushed.)"""
        workflow_run_id, job_id, _ = await self._implementing_run()
        self.assertEqual(
            await self._request_statuses(workflow_run_id),
            [("developer", 1, "dispatched"), ("planner", 1, "completed")],
        )
        await self._lose_the_job(job_id)
        runtime = await self._runtime()

        recovered, _ = await self._run_maintenance_once(
            self._advance(runtime), _NOW + timedelta(hours=1)
        )

        statuses = await self._request_statuses(workflow_run_id)
        self.assertNotIn("pipeline", [role for role, _, _ in statuses])
        self.assertEqual(
            statuses,
            [
                ("developer", 1, "orphaned"),
                ("developer", 2, "queued"),
                ("planner", 1, "completed"),
            ],
        )
        # The graph was not re-entered at all: it stays suspended where it is,
        # waiting for the developer execution that has just been queued again.
        self.assertEqual(recovered, [])
        self.assertEqual(
            await self.control_plane.find_stalled_workflow_runs(
                _NOW + timedelta(hours=2), timedelta(seconds=30)
            ),
            (),
        )


class CircuitBreakerIntegrationTests(unittest.IsolatedAsyncioTestCase):
    """No terminal outcome may leave a circuit half-open without a live probe.

    See GitHub issue #92 (review finding F5). Three wedges are reproduced here
    against real PostgreSQL, because every one of them is a property of what a
    transaction commits, which no query-string fake can observe:

    1. A half-open claim that only half succeeded used to be committed, leaving
       the project pointing at a workflow run that was never inserted. `schedule`
       excludes half-open projects, so the project never scheduled again.
    2. Probes were resolved only by `completed` and `blocked`. A cancelled or
       failed probe left the circuit half-open forever, and nothing reaped it.
    3. Closing a circuit left `probe_workflow_run_id` set, so a stale workflow
       could later reopen a circuit that had been legitimately closed.
    """

    COOLDOWN = timedelta(minutes=5)

    async def asyncSetUp(self) -> None:
        database_url = os.environ.get(_DATABASE_URL_ENV)
        if not database_url:
            self.skipTest(f"{_DATABASE_URL_ENV} is not configured")
        self.pool = await asyncpg.create_pool(database_url)
        self.addAsyncCleanup(self.pool.close)
        self.addAsyncCleanup(self._reset_shared_state)
        await MigrationRunner(self.pool).run()
        await self._reset_shared_state()
        self.control_plane = AsyncpgControlPlane(self.pool, circuit_probe_cooldown=self.COOLDOWN)
        self.provider = ""
        self._seeded_fixtures: list[tuple[UUID, UUID]] = []
        self.addAsyncCleanup(self._delete_seeded_fixtures)

    async def _reset_shared_state(self) -> None:
        """Circuit state and scheduling are global, so every seeded project is
        disabled and every circuit row is dropped: the reaper's counts and the
        candidate `schedule()` picks are then decided only by this test."""
        await self.pool.execute("UPDATE app.projects SET enabled = false")
        await self.pool.execute("DELETE FROM app.project_circuit_state")
        await self.pool.execute("DELETE FROM app.provider_circuit_state")

    async def _delete_seeded_fixtures(self) -> None:
        """Removes every project this class seeded.

        Disabling the projects is not enough. `StalledRunRecoveryIntegrationTests`
        looks for stalled runs with global queries against this shared database,
        and an abandoned `implementing` run left here is indistinguishable from a
        genuinely stalled one, so it would be recovered as part of that test's
        result. Locks and jobs go first: their references to `workflow_runs` do
        not cascade.
        """
        for project, runner in self._seeded_fixtures:
            await self.pool.execute("DELETE FROM app.project_locks WHERE project_id = $1", project)
            await self.pool.execute("DELETE FROM app.jobs WHERE project_id = $1", project)
            await self.pool.execute("DELETE FROM app.workflow_runs WHERE project_id = $1", project)
            await self.pool.execute("DELETE FROM app.issues WHERE project_id = $1", project)
            await self.pool.execute("DELETE FROM app.projects WHERE id = $1", project)
            await self.pool.execute("DELETE FROM app.runners WHERE id = $1", runner)
        self._seeded_fixtures.clear()

    async def _seed(self) -> tuple[str, str, str]:
        await self.pool.execute("UPDATE app.projects SET enabled = false")
        suffix = uuid4().hex[:12]
        label = f"circuit-{suffix}"
        self.provider = f"provider-{suffix}"
        project = await self.control_plane.create_project(
            f"circuit-{suffix}",
            "managed_clone",
            "https://example.test/circuit.git",
            None,
            "main",
            {label},
            _NOW,
        )
        project_id = str(project["id"])
        await self.control_plane.upsert_issue(
            project_id=project_id,
            external_id=f"issue-{suffix}",
            title="Circuit breaker wedges",
            body="",
            state="open",
            labels=["agent:ready"],
            priority=100,
            eligible=True,
            human_approval_required=False,
            external_created_at=_NOW,
            external_updated_at=_NOW,
            now=_NOW,
        )
        # A per-test provider keeps this test's provider circuit row its own.
        issue = await self.pool.fetchrow(
            "UPDATE app.issues SET provider = $2 WHERE project_id = $1 RETURNING id",
            UUID(project_id),
            self.provider,
        )
        assert issue is not None
        token = await self.control_plane.create_registration_token(
            {label}, _NOW + timedelta(minutes=30), now=_NOW
        )
        runner, credential = await self.control_plane.register_runner(
            token, f"runner-{suffix}", {label}, _NOW
        )
        await self.control_plane.heartbeat(runner.id, credential, _NOW)
        self._seeded_fixtures.append((UUID(project_id), UUID(runner.id)))
        return project_id, str(issue["id"]), runner.id

    async def _open_circuits(
        self, project_id: str, opened_at: datetime, *, provider_probe: Any | None = None
    ) -> None:
        await self.pool.execute(
            """
            INSERT INTO app.project_circuit_state
                (project_id, state, consecutive_failures, last_failure_reason, opened_at, updated_at)
            VALUES ($1, 'open', 3, 'base branch is broken', $2, $2)
            """,
            UUID(project_id),
            opened_at,
        )
        await self.pool.execute(
            """
            INSERT INTO app.provider_circuit_state
                (provider, state, consecutive_failures, last_failure_reason, opened_at,
                 probe_workflow_run_id, updated_at)
            VALUES ($1, 'open', 3, 'issue tracker is unavailable', $2, $3, $2)
            """,
            self.provider,
            opened_at,
            provider_probe,
        )

    async def _half_open_circuits(
        self,
        project_id: str,
        *,
        project_probe: Any | None,
        provider_probe: Any | None,
        claimed_at: datetime,
    ) -> None:
        await self.pool.execute(
            """
            INSERT INTO app.project_circuit_state
                (project_id, state, consecutive_failures, last_failure_reason, opened_at,
                 probe_workflow_run_id, updated_at)
            VALUES ($1, 'half_open', 3, 'base branch is broken', $2, $3, $2)
            """,
            UUID(project_id),
            claimed_at,
            project_probe,
        )
        await self.pool.execute(
            """
            INSERT INTO app.provider_circuit_state
                (provider, state, consecutive_failures, last_failure_reason, opened_at,
                 probe_workflow_run_id, updated_at)
            VALUES ($1, 'half_open', 3, 'issue tracker is unavailable', $2, $3, $2)
            """,
            self.provider,
            claimed_at,
            provider_probe,
        )

    async def _project_circuit(self, project_id: str) -> Any:
        record = await self.pool.fetchrow(
            """
            SELECT state, opened_at, probe_workflow_run_id, updated_at
            FROM app.project_circuit_state WHERE project_id = $1
            """,
            UUID(project_id),
        )
        assert record is not None
        return record

    async def _provider_circuit(self) -> Any:
        record = await self.pool.fetchrow(
            """
            SELECT state, opened_at, probe_workflow_run_id, updated_at
            FROM app.provider_circuit_state WHERE provider = $1
            """,
            self.provider,
        )
        assert record is not None
        return record

    async def _workflow_run_id(self, project_id: str, issue_id: str, status: str) -> str:
        run_id = uuid4()
        await self.pool.execute(
            """
            INSERT INTO app.workflow_runs
                (id, project_id, issue_id, thread_id, status, current_phase, created_at, updated_at)
            VALUES ($1, $2, $3, $4, $5, $5, $6, $6)
            """,
            run_id,
            UUID(project_id),
            UUID(issue_id),
            str(run_id),
            status,
            _NOW,
        )
        return str(run_id)

    async def _suppress_provider_circuit_updates(self) -> None:
        """Makes the provider half-open claim fail the way a lost race does:
        the row is locked and open, but the claiming UPDATE matches no row."""
        await self.pool.execute(
            """
            CREATE OR REPLACE FUNCTION app.moirai_test_suppress_update() RETURNS trigger
            LANGUAGE plpgsql AS $$ BEGIN RETURN NULL; END; $$
            """
        )
        await self.pool.execute(
            """
            CREATE TRIGGER moirai_test_suppress_provider_circuit
            BEFORE UPDATE ON app.provider_circuit_state
            FOR EACH ROW EXECUTE FUNCTION app.moirai_test_suppress_update()
            """
        )
        self.addAsyncCleanup(self._restore_provider_circuit_updates)

    async def _restore_provider_circuit_updates(self) -> None:
        await self.pool.execute(
            "DROP TRIGGER IF EXISTS moirai_test_suppress_provider_circuit ON app.provider_circuit_state"
        )
        await self.pool.execute("DROP FUNCTION IF EXISTS app.moirai_test_suppress_update()")

    async def test_a_partial_probe_claim_is_never_committed(self) -> None:
        """Wedge 1: `return None` from inside `async with connection.transaction()`
        commits. The project claim must not survive a failed provider claim."""
        project_id, _, _ = await self._seed()
        await self._open_circuits(project_id, _NOW - self.COOLDOWN - timedelta(seconds=1))
        await self._suppress_provider_circuit_updates()

        self.assertIsNone(await self.control_plane.schedule(_NOW, timedelta(minutes=5)))

        project = await self._project_circuit(project_id)
        self.assertEqual(project["state"], "open")
        self.assertIsNone(project["probe_workflow_run_id"])
        self.assertEqual(
            await self.pool.fetchval(
                "SELECT COUNT(*) FROM app.workflow_runs WHERE project_id = $1", UUID(project_id)
            ),
            0,
        )

        await self._restore_provider_circuit_updates()
        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        self.assertIsNotNone(scheduled)

    async def test_a_stale_provider_probe_pointer_does_not_wedge_the_project(self) -> None:
        """Wedges 1 and 3 together: a pointer left behind by a closed circuit
        made every later provider claim fail, and the project claim that ran
        first was committed anyway."""
        project_id, _, _ = await self._seed()
        await self._open_circuits(
            project_id, _NOW - self.COOLDOWN - timedelta(seconds=1), provider_probe=uuid4()
        )

        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))

        assert scheduled is not None
        project = await self._project_circuit(project_id)
        provider = await self._provider_circuit()
        self.assertEqual(project["state"], "half_open")
        self.assertEqual(provider["state"], "half_open")
        self.assertEqual(str(project["probe_workflow_run_id"]), scheduled.workflow.id)
        self.assertEqual(str(provider["probe_workflow_run_id"]), scheduled.workflow.id)
        self.assertIsNotNone(
            await self.pool.fetchrow(
                "SELECT id FROM app.workflow_runs WHERE id = $1", UUID(scheduled.workflow.id)
            )
        )

    async def test_a_cancelled_probe_reopens_the_circuits_and_allows_a_later_probe(self) -> None:
        """Wedge 2 and acceptance criterion 2: killing the probe workflow must
        reopen the circuit and allow another probe once the cooldown elapses."""
        project_id, _, _ = await self._seed()
        await self._open_circuits(project_id, _NOW - self.COOLDOWN - timedelta(seconds=1))
        probe = await self.control_plane.schedule(_NOW, timedelta(seconds=30))
        assert probe is not None
        self.assertEqual((await self._project_circuit(project_id))["state"], "half_open")

        expired_at = _NOW + timedelta(seconds=31)
        self.assertIn(probe.offer.job_id, await self.control_plane.expire_offers(expired_at))

        run = await self.pool.fetchrow(
            "SELECT status FROM app.workflow_runs WHERE id = $1", UUID(probe.workflow.id)
        )
        assert run is not None
        self.assertEqual(run["status"], "cancelled")
        for circuit in (await self._project_circuit(project_id), await self._provider_circuit()):
            self.assertEqual(circuit["state"], "open")
            self.assertEqual(circuit["opened_at"], expired_at)
            self.assertIsNone(circuit["probe_workflow_run_id"])
        self.assertIsNone(await self.control_plane.schedule(expired_at, timedelta(seconds=30)))

        retried = await self.control_plane.schedule(expired_at + self.COOLDOWN, timedelta(seconds=30))

        assert retried is not None
        self.assertEqual(
            str((await self._project_circuit(project_id))["probe_workflow_run_id"]),
            retried.workflow.id,
        )

    async def test_a_failed_probe_workflow_reopens_the_circuits(self) -> None:
        """Wedge 2 for the other unhandled terminal status."""
        project_id, _, _ = await self._seed()
        await self._open_circuits(project_id, _NOW - self.COOLDOWN - timedelta(seconds=1))
        probe = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert probe is not None
        failed_at = _NOW + timedelta(minutes=1)

        await AsyncpgWorkflowPersistence(self.pool, now=lambda: failed_at).transition(
            probe.workflow.id, "failed", {"status": "failed"}
        )

        for circuit in (await self._project_circuit(project_id), await self._provider_circuit()):
            self.assertEqual(circuit["state"], "open")
            self.assertEqual(circuit["opened_at"], failed_at)
            self.assertIsNone(circuit["probe_workflow_run_id"])

    async def test_a_terminal_status_written_outside_the_transition_path_releases_the_probe(self) -> None:
        """`accept_event` writes some terminal statuses straight to
        app.workflow_runs, and `PersistedWorkflowRuntime.run` returns early for
        a run that is already terminal -- so `transition` never runs for it and
        every circuit write in it is skipped. `load_state` compensates, exactly
        as it already does for the project lock."""
        project_id, _, _ = await self._seed()
        await self._open_circuits(project_id, _NOW - self.COOLDOWN - timedelta(seconds=1))
        probe = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert probe is not None
        await self.pool.execute(
            """
            UPDATE app.workflow_runs SET status = 'blocked', current_phase = 'blocked' WHERE id = $1
            """,
            UUID(probe.workflow.id),
        )
        resolved_at = _NOW + timedelta(minutes=1)

        await AsyncpgWorkflowPersistence(self.pool, now=lambda: resolved_at).load_state(
            probe.workflow.id
        )

        for circuit in (await self._project_circuit(project_id), await self._provider_circuit()):
            self.assertEqual(circuit["state"], "open")
            self.assertEqual(circuit["opened_at"], resolved_at)
            self.assertIsNone(circuit["probe_workflow_run_id"])

    async def test_a_closed_provider_circuit_is_not_reopened_by_a_stale_probe(self) -> None:
        """Wedge 3: `clear_provider_failure` closed the circuit but kept the
        pointer, so the probe's own terminal event reopened it later."""
        project_id, _, _ = await self._seed()
        await self._open_circuits(project_id, _NOW - self.COOLDOWN - timedelta(seconds=1))
        probe = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert probe is not None

        await self.control_plane.clear_provider_failure(self.provider, _NOW + timedelta(minutes=1))

        provider = await self._provider_circuit()
        self.assertEqual(provider["state"], "closed")
        self.assertIsNone(provider["probe_workflow_run_id"])

        await AsyncpgWorkflowPersistence(
            self.pool, now=lambda: _NOW + timedelta(minutes=2)
        ).transition(probe.workflow.id, "blocked", {"status": "blocked", "blocking_reason": "probe failed"})

        provider = await self._provider_circuit()
        self.assertEqual(provider["state"], "closed")
        self.assertIsNone(provider["opened_at"])

    async def test_a_stale_pointer_cannot_flip_a_circuit_that_is_no_longer_half_open(self) -> None:
        """Wedge 3, the general case behind the `state = 'half_open'` guards.

        Clearing the pointer everywhere removes the way this happened in
        practice, but not the hazard: a row written by an older orchestrator, or
        one restored from a backup, can still name a workflow that no longer
        owns the probe. Matching on the pointer alone let that workflow close an
        open provider circuit -- re-enabling scheduling in the middle of an
        outage -- and reopen a closed one.
        """
        project_id, issue_id, _ = await self._seed()
        completing = await self._workflow_run_id(project_id, issue_id, "ai_review")
        blocking = await self._workflow_run_id(project_id, issue_id, "ai_review")
        await self.pool.execute(
            """
            INSERT INTO app.provider_circuit_state
                (provider, state, consecutive_failures, last_failure_reason, opened_at,
                 probe_workflow_run_id, updated_at)
            VALUES ($1, 'open', 3, 'issue tracker is unavailable', $2, $3, $2)
            """,
            self.provider,
            _NOW,
            UUID(completing),
        )

        await AsyncpgWorkflowPersistence(
            self.pool, now=lambda: _NOW + timedelta(minutes=1)
        ).transition(completing, "completed", {"status": "completed"})

        provider = await self._provider_circuit()
        self.assertEqual(provider["state"], "open")
        self.assertEqual(provider["opened_at"], _NOW)

        await self.pool.execute(
            """
            UPDATE app.provider_circuit_state
            SET state = 'closed', consecutive_failures = 0, opened_at = NULL,
                probe_workflow_run_id = $2
            WHERE provider = $1
            """,
            self.provider,
            UUID(blocking),
        )

        await AsyncpgWorkflowPersistence(
            self.pool, now=lambda: _NOW + timedelta(minutes=2)
        ).transition(blocking, "blocked", {"status": "blocked", "blocking_reason": "stale"})

        provider = await self._provider_circuit()
        self.assertEqual(provider["state"], "closed")
        self.assertIsNone(provider["opened_at"])

    async def test_recording_a_provider_failure_drops_the_probe_pointer(self) -> None:
        """A new failure invalidates any probe: leaving the pointer set made
        the circuit unclaimable for the rest of its life."""
        project_id, _, _ = await self._seed()
        await self._open_circuits(project_id, _NOW - self.COOLDOWN - timedelta(seconds=1))
        probe = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert probe is not None

        failed_at = _NOW + timedelta(minutes=1)
        await self.control_plane.record_provider_failure(self.provider, "gh api failed", failed_at)

        provider = await self._provider_circuit()
        self.assertEqual(provider["state"], "open")
        self.assertEqual(provider["opened_at"], failed_at)
        self.assertIsNone(provider["probe_workflow_run_id"])

    async def test_orphaned_half_open_probes_are_reaped_after_the_cooldown(self) -> None:
        """Step 4: nothing else resolves a probe whose workflow row never
        existed, or died before any terminal transition reached the circuit."""
        project_id, issue_id, _ = await self._seed()
        terminal_run = await self._workflow_run_id(project_id, issue_id, "cancelled")
        await self._half_open_circuits(
            project_id,
            project_probe=uuid4(),
            provider_probe=UUID(terminal_run),
            claimed_at=_NOW - self.COOLDOWN - timedelta(seconds=1),
        )

        reaped = await self.control_plane.reap_orphaned_circuit_probes(_NOW)

        self.assertEqual(reaped, {"project_circuits": 1, "provider_circuits": 1})
        for circuit in (await self._project_circuit(project_id), await self._provider_circuit()):
            self.assertEqual(circuit["state"], "open")
            self.assertEqual(circuit["opened_at"], _NOW)
            self.assertIsNone(circuit["probe_workflow_run_id"])
        scheduled = await self.control_plane.schedule(_NOW + self.COOLDOWN, timedelta(minutes=5))
        self.assertIsNotNone(scheduled)

    async def test_a_live_or_recent_probe_is_never_reaped(self) -> None:
        project_id, issue_id, _ = await self._seed()
        live_run = await self._workflow_run_id(project_id, issue_id, "implementing")
        await self._half_open_circuits(
            project_id,
            project_probe=UUID(live_run),
            provider_probe=uuid4(),
            claimed_at=_NOW - timedelta(seconds=1),
        )

        self.assertEqual(
            await self.control_plane.reap_orphaned_circuit_probes(_NOW),
            {"project_circuits": 0, "provider_circuits": 0},
        )

        # The provider probe is a dangling pointer, so only the cooldown keeps
        # it alive; the project probe is a running workflow and stays claimed.
        reaped = await self.control_plane.reap_orphaned_circuit_probes(
            _NOW + self.COOLDOWN + timedelta(seconds=1)
        )

        self.assertEqual(reaped, {"project_circuits": 0, "provider_circuits": 1})
        self.assertEqual((await self._project_circuit(project_id))["state"], "half_open")
        self.assertEqual(str((await self._project_circuit(project_id))["probe_workflow_run_id"]), live_run)


class RunnerDrainIntegrationTests(unittest.IsolatedAsyncioTestCase):
    """A runner's self-reported drain must reach the real placement queries.

    See GitHub issue #111. `test_runner_grpc.py` covers the gRPC handler
    against the in-memory control plane; what actually stops placement in
    production is `r.draining = false` in `schedule()`, `schedule_execution()`
    and `recover_one()`, and only a real database exercises that.
    """

    async def asyncSetUp(self) -> None:
        database_url = os.environ.get(_DATABASE_URL_ENV)
        if not database_url:
            self.skipTest(f"{_DATABASE_URL_ENV} is not configured")
        self.pool = await asyncpg.create_pool(database_url)
        self.addAsyncCleanup(self.pool.close)
        self.addAsyncCleanup(self._disable_seeded_projects)
        await MigrationRunner(self.pool).run()
        self.control_plane = AsyncpgControlPlane(self.pool)

    async def _disable_seeded_projects(self) -> None:
        """Leaves the shared database schedulable-neutral for other tests."""
        await self.pool.execute("UPDATE app.projects SET enabled = false")

    async def _seed(self) -> tuple[str, str]:
        """Creates an isolated project, eligible issue, and one online runner.

        Scheduling queries are global, so every project seeded by an earlier
        test is disabled first and the runner's label is unique per test: the
        only candidate `schedule()` can pick is the one this test created.
        """
        await self.pool.execute("UPDATE app.projects SET enabled = false")
        suffix = uuid4().hex[:12]
        label = f"drain-{suffix}"
        project = await self.control_plane.create_project(
            f"runner-drain-{suffix}",
            "managed_clone",
            "https://example.test/runner-drain.git",
            None,
            "main",
            {label},
            _NOW,
        )
        await self.control_plane.upsert_issue(
            project_id=str(project["id"]),
            external_id=f"issue-{suffix}",
            title="A draining runner must not be offered work",
            body="",
            state="open",
            labels=["agent:ready"],
            priority=100,
            eligible=True,
            human_approval_required=False,
            external_created_at=_NOW,
            external_updated_at=_NOW,
            now=_NOW,
        )
        token = await self.control_plane.create_registration_token(
            {label}, _NOW + timedelta(minutes=30), now=_NOW
        )
        runner, credential = await self.control_plane.register_runner(
            token, f"runner-{suffix}", {label}, _NOW
        )
        await self.control_plane.heartbeat(runner.id, credential, _NOW)
        return runner.id, credential

    async def _runner_row(self, runner_id: str) -> dict[str, Any]:
        record = await self.pool.fetchrow(
            "SELECT enabled, draining, operator_draining, status, revoked_at FROM app.runners WHERE id = $1",
            UUID(runner_id),
        )
        assert record is not None
        return dict(record)

    async def test_a_drained_runner_is_not_selected_and_a_cleared_one_is(self) -> None:
        runner_id, _ = await self._seed()

        await self.control_plane.set_runner_draining(runner_id, True)
        self.assertIsNone(await self.control_plane.schedule(_NOW, timedelta(minutes=5)))

        await self.control_plane.set_runner_draining(runner_id, False)
        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert scheduled is not None
        self.assertEqual(scheduled.assignment.runner.id, runner_id)
        # Hand the run back so this test leaves no lock or live offer behind.
        await self.control_plane.reject_offer(scheduled.offer.job_id, runner_id, _NOW)

    async def test_operator_drain_survives_runner_report_until_undrained(self) -> None:
        runner_id, _ = await self._seed()

        await self.control_plane.set_runner_state(runner_id, "drain", None, _NOW)
        await self.control_plane.set_runner_draining(runner_id, False)
        drained = await self._runner_row(runner_id)
        self.assertTrue(drained["draining"])
        self.assertTrue(drained["operator_draining"])
        self.assertIsNone(await self.control_plane.schedule(_NOW, timedelta(minutes=5)))

        await self.control_plane.set_runner_state(runner_id, "enable", None, _NOW)
        cleared = await self._runner_row(runner_id)
        self.assertFalse(cleared["draining"])
        self.assertFalse(cleared["operator_draining"])
        scheduled = await self.control_plane.schedule(_NOW, timedelta(minutes=5))
        assert scheduled is not None
        await self.control_plane.reject_offer(scheduled.offer.job_id, runner_id, _NOW)

    async def test_a_drain_report_writes_only_the_draining_column(self) -> None:
        """The discriminator against `set_runner_state("drain")`/`("enable")`.

        Those states also write `enabled`, so a runner reporting its own drain
        state through them would silently re-enable a runner an operator had
        deliberately disabled -- in both directions.
        """
        runner_id, credential = await self._seed()

        # Heartbeating while draining must not silently undo the drain.
        await self.control_plane.set_runner_draining(runner_id, True)
        self.assertTrue((await self.control_plane.heartbeat(runner_id, credential, _NOW)).draining)

        await self.control_plane.set_runner_state(runner_id, "disable", None, _NOW)
        self.assertFalse((await self._runner_row(runner_id))["draining"])

        await self.control_plane.set_runner_draining(runner_id, True)
        drained = await self._runner_row(runner_id)
        self.assertTrue(drained["draining"])
        self.assertFalse(drained["enabled"])
        self.assertEqual(drained["status"], "online")

        await self.control_plane.set_runner_draining(runner_id, False)
        cleared = await self._runner_row(runner_id)
        self.assertFalse(cleared["draining"])
        self.assertFalse(cleared["enabled"])

    async def test_a_drain_report_is_rejected_for_an_unknown_or_revoked_runner(self) -> None:
        runner_id, credential = await self._seed()

        await self.control_plane.set_runner_state(runner_id, "revoke", None, _NOW)
        with self.assertRaises(AuthenticationError):
            await self.control_plane.authenticate_runner(runner_id, credential, _NOW)
        with self.assertRaises(ValueError):
            await self.control_plane.set_runner_draining(runner_id, False)
        # Rejected means untouched: `revoke` left the row draining.
        self.assertTrue((await self._runner_row(runner_id))["draining"])

        with self.assertRaises(ValueError):
            await self.control_plane.set_runner_draining(str(uuid4()), True)


if __name__ == "__main__":
    unittest.main()
