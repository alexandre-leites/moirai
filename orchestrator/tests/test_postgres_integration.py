from __future__ import annotations

import os
import unittest
from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import UUID, uuid4

import asyncpg

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

    async def _reset_shared_state(self) -> None:
        """Circuit state and scheduling are global, so every seeded project is
        disabled and every circuit row is dropped: the reaper's counts and the
        candidate `schedule()` picks are then decided only by this test."""
        await self.pool.execute("UPDATE app.projects SET enabled = false")
        await self.pool.execute("DELETE FROM app.project_circuit_state")
        await self.pool.execute("DELETE FROM app.provider_circuit_state")

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


if __name__ == "__main__":
    unittest.main()
