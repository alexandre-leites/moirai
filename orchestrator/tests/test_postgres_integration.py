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

    async def _run_status(self, workflow_run_id: str) -> str:
        status = await self.pool.fetchval(
            "SELECT status FROM app.workflow_runs WHERE id = $1", UUID(workflow_run_id)
        )
        return str(status)

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

    async def test_committed_transition_without_a_graph_invocation_is_recovered(self) -> None:
        """Acceptance criterion 2: a crash between committing a terminal
        transition and invoking the graph runtime is repaired by the
        maintenance loop within one interval."""
        workflow_run_id, job_id, runner_id, request_id = await self._planning_run()
        runtime = await self._runtime()

        # The crash: accept_event commits the transition and the outbox row,
        # then the process dies before invoking the graph. The outbox row is
        # left mid-drain ('processing'), which the drain arm never retries, so
        # recovery can only come from the stalled-run arm.
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

        recovered, _ = await self._run_maintenance_once(
            self._advance(runtime), _NOW + timedelta(hours=1)
        )

        # The execution reported, so the graph is re-entered rather than the
        # phase re-queued, and the suspension flag is cleared so the resumed
        # edge can actually move.
        self.assertEqual(
            recovered, [(workflow_run_id, "implementing", {"awaiting_execution": False})]
        )
        # `plan_valid` rode on the outbox row that was never drained, so the
        # resumed `plan` edge routes back into the planning phase and queues a
        # second planner attempt rather than advancing on a verdict the graph
        # never saw. The run is making progress again, bounded by
        # `planning_attempts` -- which is what recovery has to guarantee.
        self.assertEqual(
            await self._request_statuses(workflow_run_id),
            [("planner", 1, "completed"), ("planner", 2, "queued")],
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


if __name__ == "__main__":
    unittest.main()
