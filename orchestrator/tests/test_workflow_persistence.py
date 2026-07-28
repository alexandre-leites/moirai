from __future__ import annotations

from datetime import UTC, datetime
import unittest
from typing import Any

from moirai.workflows.persistence import AsyncpgWorkflowPersistence


NOW = datetime(2026, 1, 1, tzinfo=UTC)
WORKFLOW_ID = "00000000-0000-0000-0000-000000000001"


class _Transaction:
    async def __aenter__(self) -> _Transaction:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None


class _Connection:
    def __init__(self) -> None:
        self.known = True
        self.queries: list[str] = []
        self.attempt = 1

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *args: object) -> dict[str, object] | None:
        self.queries.append(query)
        return {"id": args[0]} if self.known else None

    async def fetchval(self, query: str, *args: object) -> int:
        self.queries.append(query)
        return self.attempt

    async def execute(self, query: str, *args: object) -> str:
        self.queries.append(query)
        return "INSERT 0 1"


class _Lease:
    def __init__(self, connection: _Connection) -> None:
        self.connection = connection

    async def __aenter__(self) -> _Connection:
        return self.connection

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None


class _Pool:
    def __init__(self) -> None:
        self.connection = _Connection()
        self.checkpoint: dict[str, object] | None = None

    def acquire(self) -> _Lease:
        return _Lease(self.connection)

    async def fetchrow(self, query: str, *args: object) -> dict[str, object] | None:
        del query, args
        return self.checkpoint


class AsyncpgWorkflowPersistenceTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self.pool = _Pool()
        self.store = AsyncpgWorkflowPersistence(self.pool, now=lambda: NOW)

    async def test_transition_updates_workflow_and_appends_event(self) -> None:
        await self.store.transition(WORKFLOW_ID, "planning", {"status": "planning"})
        self.assertTrue(any("UPDATE app.workflow_runs" in query for query in self.pool.connection.queries))
        self.assertTrue(any("INSERT INTO app.workflow_events" in query for query in self.pool.connection.queries))

    async def test_transition_persists_durable_columns_present_in_updates(self) -> None:
        await self.store.transition(
            WORKFLOW_ID,
            "blocked",
            {
                "status": "blocked",
                "review_cycles": 2,
                "total_agent_executions": 5,
                "blocking_reason": "workflow retry budget exhausted",
                "pull_request_id": "42",
                "pull_request_url": "https://github.com/example/repo/pull/42",
            },
        )
        update_query = next(q for q in self.pool.connection.queries if "UPDATE app.workflow_runs" in q)
        self.assertIn("review_cycles = $4", update_query)
        self.assertIn("total_agent_executions = $5", update_query)
        self.assertIn("blocking_reason = $6", update_query)
        self.assertIn("pull_request_external_id = $7", update_query)
        self.assertIn("pull_request_url = $8", update_query)
        self.assertIn("completed_at = COALESCE(completed_at, $9)", update_query)

    async def test_transition_upserts_pull_request_when_pull_request_id_present(self) -> None:
        await self.store.transition(
            WORKFLOW_ID,
            "pr_created",
            {
                "status": "pr_created",
                "pull_request_id": "42",
                "pull_request_url": "https://github.com/example/repo/pull/42",
                "pull_request_head_commit": "abc123",
                "pull_request_state": "open",
            },
        )
        self.assertTrue(any("INSERT INTO app.pull_requests" in q for q in self.pool.connection.queries))

    async def test_transition_omits_columns_not_present_in_updates(self) -> None:
        await self.store.transition(WORKFLOW_ID, "planning", {"status": "planning"})
        update_query = next(q for q in self.pool.connection.queries if "UPDATE app.workflow_runs" in q)
        self.assertNotIn("review_cycles", update_query)
        self.assertNotIn("completed_at", update_query)

    async def test_transition_rejects_unknown_workflow(self) -> None:
        self.pool.connection.known = False
        with self.assertRaisesRegex(ValueError, "unknown"):
            await self.store.transition(WORKFLOW_ID, "planning", {})

    async def test_checkpoint_versions_workflow_state_transactionally(self) -> None:
        version = await self.store.checkpoint(WORKFLOW_ID, {"status": "planning", "planning_attempts": 1})
        self.assertEqual(version, 1)
        self.assertTrue(any("workflow_checkpoints" in query for query in self.pool.connection.queries))

    async def test_latest_checkpoint_restores_json_state_and_rejects_invalid_state(self) -> None:
        self.pool.checkpoint = {"version": 3, "state": '{"status":"planning"}'}
        self.assertEqual(await self.store.latest_checkpoint(WORKFLOW_ID), (3, {"status": "planning"}))
        self.pool.checkpoint = {"version": 4, "state": "[]"}
        with self.assertRaisesRegex(ValueError, "checkpoint state"):
            await self.store.latest_checkpoint(WORKFLOW_ID)
        self.pool.checkpoint = None
        self.assertIsNone(await self.store.latest_checkpoint(WORKFLOW_ID))

    async def test_dispatch_creates_a_queued_request_with_role_attempt(self) -> None:
        execution_id = await self.store.dispatch(WORKFLOW_ID, "planner")
        self.assertRegex(execution_id, r"^[0-9a-f-]{36}$")
        self.assertTrue(any("FOR UPDATE" in query for query in self.pool.connection.queries))
        self.assertTrue(any("workflow_execution_requests" in query for query in self.pool.connection.queries))

    async def test_dispatch_rejects_invalid_role_and_unknown_workflow(self) -> None:
        with self.assertRaisesRegex(ValueError, "role"):
            await self.store.dispatch(WORKFLOW_ID, "merge")
        self.pool.connection.known = False
        with self.assertRaisesRegex(ValueError, "unknown"):
            await self.store.dispatch(WORKFLOW_ID, "planner")


if __name__ == "__main__":
    unittest.main()
