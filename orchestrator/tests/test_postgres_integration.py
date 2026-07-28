from __future__ import annotations

import os
import unittest
from datetime import UTC, datetime, timedelta
from uuid import uuid4

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


if __name__ == "__main__":
    unittest.main()
