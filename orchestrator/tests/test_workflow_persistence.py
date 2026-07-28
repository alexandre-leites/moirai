from __future__ import annotations

import unittest
from datetime import UTC, datetime
from typing import Self

from moirai.workflows.persistence import AsyncpgWorkflowPersistence

NOW = datetime(2026, 1, 1, tzinfo=UTC)
WORKFLOW_ID = "00000000-0000-0000-0000-000000000001"


class _Transaction:
    async def __aenter__(self) -> Self:
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
        if "JOIN app.issues AS i" in query:
            return {
                "id": args[0], "project_id": "00000000-0000-0000-0000-000000000002",
                "status": "waiting_github_checks", "branch_name": None,
                "planning_attempts": 1, "implementation_attempts": 2,
                "pipeline_repair_attempts": 0, "review_cycles": 1,
                "ci_repair_attempts": 0, "total_agent_executions": 4,
                "blocking_reason": None, "pull_request_external_id": "42",
                "pull_request_url": "https://github.com/example/repo/pull/42",
                "external_id": "42", "human_approval_required": True,
                "default_branch": "main", "configuration": {"merge_method": "squash"},
                "job_id": "00000000-0000-0000-0000-000000000003",
            }
        if "UPDATE app.workflow_runs" in query and self.known:
            return {"id": args[0], "project_id": "00000000-0000-0000-0000-000000000002"}
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

    async def test_terminal_transition_releases_its_project_lock_transactionally(self) -> None:
        await self.store.transition(WORKFLOW_ID, "completed", {"status": "completed"})
        self.assertTrue(any("DELETE FROM app.project_locks" in query for query in self.pool.connection.queries))

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

    async def test_terminal_transitions_update_project_circuit_and_only_mark_real_progress(self) -> None:
        await self.store.transition(
            WORKFLOW_ID,
            "blocked",
            {"status": "blocked", "blocking_reason": "base branch is broken", "progressed": False},
        )
        update_query = next(q for q in self.pool.connection.queries if "UPDATE app.workflow_runs" in q)
        self.assertNotIn("last_progress_at", update_query)
        self.assertTrue(any("project_circuit_state" in q for q in self.pool.connection.queries))
        self.pool.connection.queries.clear()
        await self.store.transition(WORKFLOW_ID, "planning", {"status": "planning", "progressed": True})
        update_query = next(q for q in self.pool.connection.queries if "UPDATE app.workflow_runs" in q)
        self.assertIn("last_progress_at", update_query)

    async def test_terminal_probe_outcomes_reopen_or_close_provider_circuits(self) -> None:
        await self.store.transition(
            WORKFLOW_ID,
            "blocked",
            {"status": "blocked", "blocking_reason": "probe failed"},
        )
        blocked_queries = "\n".join(self.pool.connection.queries)
        self.assertIn("state = 'half_open' THEN 'open'", blocked_queries)
        self.assertIn("UPDATE app.provider_circuit_state", blocked_queries)
        self.pool.connection.queries.clear()
        await self.store.transition(WORKFLOW_ID, "completed", {"status": "completed"})
        completed_queries = "\n".join(self.pool.connection.queries)
        self.assertIn("probe_workflow_run_id = NULL", completed_queries)
        self.assertIn("UPDATE app.provider_circuit_state", completed_queries)

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

    async def test_load_state_seeds_project_issue_branch_merge_and_approval_fields(self) -> None:
        state = await self.store.load_state(WORKFLOW_ID)
        self.assertEqual(state["project_id"], "00000000-0000-0000-0000-000000000002")
        self.assertEqual(state["issue_id"], "42")
        self.assertEqual(state["branch_name"], "agent/42/00000000")
        self.assertEqual(state["base_branch"], "main")
        self.assertEqual(state["merge_method"], "squash")
        self.assertTrue(state["human_approval_required"])
        self.assertTrue(any("branch_name = $2" in query for query in self.pool.connection.queries))

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
