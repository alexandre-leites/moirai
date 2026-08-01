from __future__ import annotations

import unittest
from datetime import UTC, datetime

from moirai.persistence.control_plane import AsyncpgControlPlane


class _Pool:
    def __init__(self, row: dict[str, object] | None) -> None:
        self.row = row
        self.query = ""
        self.arguments: tuple[object, ...] = ()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        self.query = query
        self.arguments = arguments
        return self.row


class MetricsSnapshotTests(unittest.IsolatedAsyncioTestCase):
    async def test_snapshot_reads_queue_workflows_jobs_and_runner_heartbeat_age(self) -> None:
        pool = _Pool(
            {"queue_depth": 3, "active_workflows": 2, "scheduled_jobs": 1, "runner_heartbeat_age": 4.5}
        )
        snapshot = await AsyncpgControlPlane(pool).metrics_snapshot(datetime(2026, 1, 1, tzinfo=UTC))
        self.assertEqual(
            snapshot,
            {
                "queue_depth": 3.0,
                "active_workflows": 2.0,
                "scheduled_jobs": 1.0,
                "runner_heartbeat_age": 4.5,
            },
        )
        self.assertIn("app.issues", pool.query)
        self.assertIn("app.workflow_runs", pool.query)
        self.assertIn("app.jobs", pool.query)
        self.assertIn("app.runners", pool.query)

    async def test_snapshot_defaults_missing_values_to_zero(self) -> None:
        # Every gauge GetSchedulerMetrics reads must be present even when the
        # query returns no row, or the RPC raises a KeyError instead of
        # reporting an idle control plane.
        snapshot = await AsyncpgControlPlane(_Pool(None)).metrics_snapshot(datetime(2026, 1, 1, tzinfo=UTC))
        self.assertEqual(
            snapshot,
            {"queue_depth": 0, "active_workflows": 0, "scheduled_jobs": 0, "runner_heartbeat_age": 0},
        )
