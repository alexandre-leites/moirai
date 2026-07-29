from __future__ import annotations

import os
import unittest
from datetime import UTC, datetime, timedelta
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

        self.assertEqual(await control_plane.list_projects(), [project])
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
        # An older blocked run must never win over the newest completed run,
        # whatever order the randomly generated run UUIDs happen to impose.
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
        # This suite runs against a shared database; leaving a second project
        # behind would break the sibling test's pristine-database assumption.
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


if __name__ == "__main__":
    unittest.main()
