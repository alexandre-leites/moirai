from __future__ import annotations

import os
import unittest
from datetime import UTC, datetime, timedelta
from typing import Any
from uuid import UUID, uuid4

import asyncpg

from moirai.persistence.control_plane import AsyncpgControlPlane
from moirai.persistence.migrations import MigrationRunner

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


if __name__ == "__main__":
    unittest.main()
