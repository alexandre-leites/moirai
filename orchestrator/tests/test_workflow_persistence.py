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
        self.calls: list[tuple[str, tuple[object, ...]]] = []
        self.attempt = 1
        self.status = "waiting_github_checks"
        self.open_request: dict[str, object] | None = None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *args: object) -> dict[str, object] | None:
        self.queries.append(query)
        self.calls.append((query, args))
        if "JOIN app.issues AS i" in query:
            return {
                "id": args[0], "project_id": "00000000-0000-0000-0000-000000000002",
                "status": self.status, "branch_name": None,
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
        if "FROM app.workflow_execution_requests" in query:
            return self.open_request
        return {"id": args[0]} if self.known else None

    async def fetchval(self, query: str, *args: object) -> int:
        self.queries.append(query)
        return self.attempt

    async def execute(self, query: str, *args: object) -> str:
        self.queries.append(query)
        self.calls.append((query, args))
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
        self.open_request: dict[str, object] | None = None
        self.queries: list[str] = []

    def acquire(self) -> _Lease:
        return _Lease(self.connection)

    async def fetchrow(self, query: str, *args: object) -> dict[str, object] | None:
        del args
        self.queries.append(query)
        if "FROM app.workflow_execution_requests" in query:
            return self.open_request
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

    async def test_transition_stores_an_empty_outcome_hash_as_null(self) -> None:
        """Issue #101: "no diff" had two encodings. The control plane's own
        writer uses SQL NULL, so this writer must never persist "" instead."""
        await self.store.transition(
            WORKFLOW_ID,
            "blocked",
            {"status": "blocked", "last_diff_hash": "", "last_failure_fingerprint": ""},
        )
        query, arguments = next(
            call for call in self.pool.connection.calls if "UPDATE app.workflow_runs" in call[0]
        )
        self.assertIn("last_diff_hash = $4", query)
        self.assertIn("last_failure_fingerprint = $5", query)
        self.assertIsNone(arguments[3])
        self.assertIsNone(arguments[4])

    async def test_transition_preserves_a_real_outcome_hash(self) -> None:
        await self.store.transition(
            WORKFLOW_ID,
            "blocked",
            {"status": "blocked", "last_diff_hash": "a" * 64},
        )
        _, arguments = next(
            call for call in self.pool.connection.calls if "UPDATE app.workflow_runs" in call[0]
        )
        self.assertEqual(arguments[3], "a" * 64)

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

    async def test_a_verified_merge_writes_merged_at_and_the_merge_commit(self) -> None:
        """The merge node's confirming read is only worth taking if it lands in
        the durable record: `merged_at` had no writer at all before (issue
        #121), so nothing could tell an attempted merge from a completed one."""
        await self.store.transition(
            WORKFLOW_ID,
            "merging",
            {
                "status": "merging",
                "pull_request_id": "42",
                "pull_request_url": "https://github.com/example/repo/pull/42",
                "pull_request_head_commit": "abc123",
                "pull_request_state": "merged",
                "pull_request_merged": True,
                "pull_request_merged_at": "2026-01-02T03:04:05+00:00",
                "pull_request_merge_commit": "def456",
            },
        )
        query, arguments = next(
            call for call in self.pool.connection.calls if "INSERT INTO app.pull_requests" in call[0]
        )
        self.assertEqual(arguments[4], "merged")
        self.assertEqual(arguments[5], datetime(2026, 1, 2, 3, 4, 5, tzinfo=UTC))
        self.assertEqual(arguments[6], "def456")
        # A later write that carries no merge must not erase the merge.
        self.assertIn("merged_at = COALESCE(EXCLUDED.merged_at", query)
        self.assertIn("merge_commit = COALESCE(", query)

    async def test_an_unmerged_pull_request_writes_no_merge_evidence(self) -> None:
        await self.store.transition(
            WORKFLOW_ID,
            "merging",
            {
                "status": "merging",
                "pull_request_id": "42",
                "pull_request_state": "open",
                "pull_request_merged": False,
                "pull_request_merged_at": "",
                "pull_request_merge_commit": "",
            },
        )
        _, arguments = next(
            call for call in self.pool.connection.calls if "INSERT INTO app.pull_requests" in call[0]
        )
        self.assertIsNone(arguments[5])
        self.assertIsNone(arguments[6])

    async def test_a_merge_the_code_host_did_not_timestamp_still_gets_one(self) -> None:
        """A merged pull request with a NULL `merged_at` is exactly the hole
        this column exists to close, so the writer stamps its own clock rather
        than recording the merge as untimed."""
        await self.store.transition(
            WORKFLOW_ID,
            "merging",
            {
                "status": "merging",
                "pull_request_id": "42",
                "pull_request_state": "merged",
                "pull_request_merged": True,
                "pull_request_merged_at": "",
            },
        )
        _, arguments = next(
            call for call in self.pool.connection.calls if "INSERT INTO app.pull_requests" in call[0]
        )
        self.assertEqual(arguments[5], NOW)

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

    async def test_cancelled_and_failed_probes_release_their_half_open_circuits(self) -> None:
        """Issue #92, wedge 2: only `completed` and `blocked` resolved a probe,
        so a cancelled or failed probe workflow left its circuit half-open --
        and its project unschedulable -- with no reaper and no way back."""
        for status in ("cancelled", "failed"):
            with self.subTest(status=status):
                self.pool.connection.queries.clear()
                await self.store.transition(WORKFLOW_ID, status, {"status": status})
                released = [
                    query
                    for query in self.pool.connection.queries
                    if "WHERE probe_workflow_run_id = $1 AND state = 'half_open'" in query
                ]
                self.assertEqual(len(released), 2)
                for query in released:
                    self.assertIn("SET state = 'open', opened_at = $2, probe_workflow_run_id = NULL", query)
                self.assertTrue(any("app.project_circuit_state" in query for query in released))
                self.assertTrue(any("app.provider_circuit_state" in query for query in released))

    async def test_probe_resolution_only_ever_touches_a_half_open_circuit(self) -> None:
        """Issue #92, wedge 3: matching the probe pointer alone let a workflow
        whose pointer was never cleared reopen -- or close -- a provider circuit
        that had already been decided on real evidence."""
        for status in ("completed", "blocked"):
            with self.subTest(status=status):
                self.pool.connection.queries.clear()
                await self.store.transition(WORKFLOW_ID, status, {"status": status})
                provider = next(
                    query
                    for query in self.pool.connection.queries
                    if "UPDATE app.provider_circuit_state" in query
                )
                self.assertIn("WHERE probe_workflow_run_id = $1 AND state = 'half_open'", provider)

    async def test_loading_a_terminal_run_releases_the_probe_it_still_holds(self) -> None:
        """Issue #92: the control plane writes some terminal statuses straight
        to app.workflow_runs from the runner-event path, and the runtime short
        -circuits a run that is already terminal, so `transition` never runs for
        it. This is the same compensation as the project-lock release."""
        self.pool.connection.status = "blocked"
        await self.store.load_state(WORKFLOW_ID)
        released = [
            query
            for query in self.pool.connection.queries
            if "WHERE probe_workflow_run_id = $1 AND state = 'half_open'" in query
        ]
        self.assertEqual(len(released), 2)

    async def test_loading_a_completed_run_never_reopens_its_circuit(self) -> None:
        """A delivered probe closes its circuit; only `transition` can do that,
        so this path must not reopen what that success just closed."""
        self.pool.connection.status = "completed"
        await self.store.load_state(WORKFLOW_ID)
        self.assertEqual(
            [query for query in self.pool.connection.queries if "probe_workflow_run_id" in query], []
        )

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
        request = await self.store.dispatch(WORKFLOW_ID, "planner")
        self.assertRegex(str(request["id"]), r"^[0-9a-f-]{36}$")
        self.assertEqual(request["role"], "planner")
        self.assertIs(request["created"], True)
        self.assertTrue(any("FOR UPDATE" in query for query in self.pool.connection.queries))
        self.assertTrue(any(
            "INSERT INTO app.workflow_execution_requests" in query
            for query in self.pool.connection.queries
        ))

    async def test_dispatch_reuses_an_open_request_instead_of_queueing_a_second(self) -> None:
        """Issue #96: the outbox delivers a transition at least once, so a node
        can be entered again with the execution it queued still in flight. The
        decision is made inside the transaction holding the run's row lock, so
        two concurrent deliveries cannot both insert."""
        for status in ("queued", "dispatched"):
            with self.subTest(open_request_status=status):
                self.pool.connection = _Connection()
                self.pool.connection.open_request = {
                    "id": "00000000-0000-0000-0000-0000000000aa", "role": "developer", "attempt": 2,
                }
                store = AsyncpgWorkflowPersistence(self.pool, now=lambda: NOW)

                request = await store.dispatch(WORKFLOW_ID, "developer")

                self.assertEqual(request["id"], "00000000-0000-0000-0000-0000000000aa")
                self.assertEqual(request["attempt"], 2)
                self.assertIs(request["created"], False)
                self.assertFalse(any(
                    "INSERT INTO app.workflow_execution_requests" in query
                    for query in self.pool.connection.queries
                ))

    async def test_the_open_request_lookup_reads_only_live_rows(self) -> None:
        """A request closed by its own terminal event, or by the maintenance
        loop, is finished work: treating it as live would stop the phase it
        belonged to from ever running again. Newest first, because a re-queued
        phase leaves older closed rows for the same role behind."""
        self.pool.open_request = {
            "id": "00000000-0000-0000-0000-0000000000aa", "role": "reviewer", "attempt": 3,
        }

        request = await self.store.get_open_execution_request(WORKFLOW_ID)

        self.assertEqual(request, {
            "id": "00000000-0000-0000-0000-0000000000aa", "role": "reviewer",
            "attempt": 3, "created": False,
        })
        query = next(
            query for query in self.pool.queries if "FROM app.workflow_execution_requests" in query
        )
        self.assertIn("status IN ('queued', 'dispatched')", query)
        self.assertIn("ORDER BY created_at DESC", query)

    async def test_the_open_request_lookup_returns_none_when_nothing_is_in_flight(self) -> None:
        self.assertIsNone(await self.store.get_open_execution_request(WORKFLOW_ID))

    async def test_dispatch_rejects_invalid_role_and_unknown_workflow(self) -> None:
        with self.assertRaisesRegex(ValueError, "role"):
            await self.store.dispatch(WORKFLOW_ID, "merge")
        self.pool.connection.known = False
        with self.assertRaisesRegex(ValueError, "unknown"):
            await self.store.dispatch(WORKFLOW_ID, "planner")


if __name__ == "__main__":
    unittest.main()
