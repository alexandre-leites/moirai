import sys
import unittest
from types import ModuleType

from moirai.workflows.runtime import PersistedWorkflowRuntime, build_persisted_runtime


class _Checkpoints:
    def __init__(
        self,
        checkpoint: tuple[int, dict[str, object]] | None = None,
        seed: dict[str, object] | None = None,
        open_request: dict[str, object] | None = None,
    ) -> None:
        self.current = checkpoint
        self.seed = seed or {}
        self.saved: list[tuple[str, dict[str, object]]] = []
        self.open_request = open_request

    async def get_open_execution_request(self, workflow_run_id: str) -> dict[str, object] | None:
        del workflow_run_id
        return self.open_request

    async def latest_checkpoint(self, workflow_run_id: str) -> tuple[int, dict[str, object]] | None:
        del workflow_run_id
        return self.current

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int:
        self.saved.append((workflow_run_id, state))
        return len(self.saved)

    async def load_state(self, workflow_run_id: str) -> dict[str, object]:
        del workflow_run_id
        return dict(self.seed)


class _TransitioningCheckpoints(_Checkpoints):
    def __init__(self, checkpoint: tuple[int, dict[str, object]] | None = None) -> None:
        super().__init__(checkpoint)
        self.transitions: list[tuple[str, str, dict[str, object]]] = []

    async def transition(self, workflow_run_id: str, status: str, updates: dict[str, object]) -> None:
        self.transitions.append((workflow_run_id, status, updates))


class _FailingGraph:
    def __init__(self, error: Exception) -> None:
        self.error = error

    async def ainvoke(self, state: dict[str, object] | None, config: dict[str, object]) -> dict[str, object]:
        raise self.error


class _Graph:
    def __init__(self, response: object) -> None:
        self.response = response
        self.calls: list[tuple[dict[str, object], dict[str, object]]] = []

    async def ainvoke(self, state: dict[str, object], config: dict[str, object]) -> dict[str, object]:
        self.calls.append((state, config))
        return self.response  # type: ignore[return-value]


class _GraphWithUpdate:
    def __init__(self, response: object) -> None:
        self.response = response
        self.calls: list = []
        self.updates: list = []

    async def ainvoke(self, state: dict[str, object] | None, config: dict[str, object]) -> dict[str, object]:
        self.calls.append((state, config))
        return self.response  # type: ignore[return-value]

    async def aupdate_state(self, config: dict[str, object], values: dict[str, object]) -> None:
        self.updates.append((config, values))


class PersistedWorkflowRuntimeTests(unittest.IsolatedAsyncioTestCase):
    async def test_starts_from_initial_state_and_checkpoints_result(self) -> None:
        checkpoints = _Checkpoints()
        graph = _Graph({"status": "planning"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)
        result = await runtime.run("workflow-1", {"status": "offered"})
        self.assertEqual(graph.calls[0][0], {"status": "offered", "workflow_run_id": "workflow-1"})
        self.assertEqual(graph.calls[0][1], {"configurable": {"thread_id": "workflow-1"}})
        self.assertEqual(result, {"status": "planning", "workflow_run_id": "workflow-1"})
        self.assertEqual(checkpoints.saved, [("workflow-1", result)])

    async def test_seeds_durable_project_and_approval_state_before_invocation(self) -> None:
        checkpoints = _Checkpoints(seed={"project_id": "project-1", "issue_id": "42", "human_approval_required": True})
        graph = _Graph({"status": "waiting_human"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)
        await runtime.run("workflow-1", {"status": "waiting_github_checks"})
        state = graph.calls[0][0]
        self.assertEqual(state["project_id"], "project-1")
        self.assertEqual(state["issue_id"], "42")
        self.assertTrue(state["human_approval_required"])

    async def test_terminal_state_is_checkpointed_without_reentering_the_graph(self) -> None:
        checkpoints = _Checkpoints(seed={"status": "cancelled"})
        graph = _Graph({"status": "planning"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)
        result = await runtime.run("workflow-1", {"status": "cancelled"})
        self.assertEqual(result["status"], "cancelled")
        self.assertEqual(graph.calls, [])

    async def test_resumes_from_latest_checkpoint(self) -> None:
        checkpoints = _Checkpoints(
            (3, {"status": "implementing", "planning_attempts": 1}),
            seed={"status": "implementing"},
        )
        graph = _Graph({"status": "local_pipeline"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)
        await runtime.run("workflow-1", {})
        self.assertEqual(graph.calls[0][0]["status"], "implementing")
        self.assertEqual(graph.calls[0][0]["planning_attempts"], 1)

    async def test_rejects_missing_id_and_invalid_graph_state(self) -> None:
        runtime = PersistedWorkflowRuntime(_Graph([]), _Checkpoints())
        with self.assertRaisesRegex(ValueError, "workflow run ID"):
            await runtime.run("", {})
        with self.assertRaisesRegex(ValueError, "invalid state"):
            await runtime.run("workflow-1", {})

    async def test_checkpointer_applies_state_updates_via_update_state(self) -> None:
        checkpoints = _Checkpoints((1, {"status": "planning", "planning_attempts": 1}))
        graph = _GraphWithUpdate({"status": "implementing"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints, has_checkpointer=True)
        result = await runtime.run("wf-1", {"status": "implementing", "plan_valid": True})
        self.assertEqual(len(graph.updates), 1)
        self.assertEqual(graph.updates[0][1], {"status": "implementing", "plan_valid": True})
        self.assertEqual(graph.calls[0][0], None)
        self.assertEqual(result["status"], "implementing")
        self.assertEqual(result["workflow_run_id"], "wf-1")

    async def test_checkpointer_first_call_passes_state_directly(self) -> None:
        checkpoints = _Checkpoints()
        graph = _GraphWithUpdate({"status": "planning"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)
        result = await runtime.run("wf-1", {"status": "offered"})
        self.assertEqual(len(graph.updates), 0)
        self.assertEqual(graph.calls[0][0], {"status": "offered", "workflow_run_id": "wf-1"})
        self.assertEqual(result["status"], "planning")

    async def test_without_a_checkpointer_a_suspended_run_is_not_replayed_from_the_start(self) -> None:
        """Replaying from START would re-enter the node that queued the
        execution the run is still waiting on, dispatching it a second time."""
        checkpoints = _Checkpoints(
            (2, {"status": "implementing", "awaiting_execution": True}),
            seed={"status": "implementing"},
        )
        graph = _Graph({"status": "local_pipeline"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)

        result = await runtime.run("workflow-1", {"human_approved": True})

        self.assertEqual(graph.calls, [])
        self.assertEqual(result["status"], "implementing")
        self.assertEqual(checkpoints.saved, [("workflow-1", result)])

    async def test_a_cleared_awaiting_gate_lets_the_run_advance_again(self) -> None:
        checkpoints = _Checkpoints(
            (2, {"status": "implementing", "awaiting_execution": True}),
            seed={"status": "implementing"},
        )
        graph = _Graph({"status": "local_pipeline"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)

        result = await runtime.run("workflow-1", {"status": "local_pipeline", "awaiting_execution": False})

        self.assertEqual(len(graph.calls), 1)
        self.assertEqual(result["status"], "local_pipeline")

    async def test_a_transition_replayed_while_an_execution_is_open_does_not_advance(self) -> None:
        """Issue #96: the transition outbox is at-least-once, so a delivery that
        already advanced the graph can arrive again with its gate cleared. The
        run still has an execution open, so re-asserting the gate is what keeps
        the second delivery from walking onto the next phase."""
        checkpoints = _Checkpoints(
            (2, {"status": "implementing", "awaiting_execution": True}),
            seed={"status": "implementing"},
            open_request={"id": "request-1", "role": "developer", "attempt": 1, "created": False},
        )
        graph = _Graph({"status": "local_pipeline"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)

        result = await runtime.run("workflow-1", {"status": "implementing", "awaiting_execution": False})

        self.assertEqual(graph.calls, [])
        self.assertIs(result["awaiting_execution"], True)
        self.assertEqual(result["status"], "implementing")

    async def test_the_replay_gate_only_applies_to_callers_clearing_the_gate(self) -> None:
        """Only a runner transition clears `awaiting_execution`, and only those
        can be replayed by the outbox. A human decision arrives with no such
        key and has nothing to do with an execution request, so it must not be
        held back by one -- otherwise an approval would be silently dropped by
        a request the maintenance loop has not closed yet."""
        checkpoints = _Checkpoints(
            (2, {"status": "waiting_github_checks"}),
            seed={"status": "waiting_github_checks"},
            open_request={"id": "request-1", "role": "developer", "attempt": 1, "created": False},
        )
        graph = _Graph({"status": "merging"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)

        result = await runtime.run(
            "workflow-1", {"human_approved": True, "human_changes_requested": False}
        )

        self.assertEqual(len(graph.calls), 1)
        self.assertNotIn("awaiting_execution", graph.calls[0][0])
        self.assertEqual(result["status"], "merging")

    async def test_an_open_request_never_holds_back_a_terminal_run(self) -> None:
        """A terminal run is already finished; the maintenance loop closes the
        request it leaked. Consulting the gate first would only delay the
        checkpoint that records the terminal state."""
        checkpoints = _Checkpoints(
            seed={"status": "blocked"},
            open_request={"id": "request-1", "role": "developer", "attempt": 1, "created": False},
        )
        graph = _Graph({"status": "blocked"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)

        result = await runtime.run("workflow-1", {"status": "blocked"})

        self.assertEqual(graph.calls, [])
        self.assertNotIn("awaiting_execution", result)
        self.assertEqual(checkpoints.saved, [("workflow-1", result)])

    async def test_node_exception_transitions_run_to_failed_instead_of_propagating(self) -> None:
        checkpoints = _TransitioningCheckpoints()
        graph = _FailingGraph(RuntimeError("node exploded"))
        runtime = PersistedWorkflowRuntime(graph, checkpoints)

        result = await runtime.run("workflow-1", {"status": "implementing"})

        self.assertEqual(result["status"], "failed")
        self.assertIn("node exploded", str(result["blocking_reason"]))
        self.assertEqual(checkpoints.transitions, [
            ("workflow-1", "failed", {"status": "failed", "blocking_reason": result["blocking_reason"]}),
        ])
        self.assertEqual(checkpoints.saved, [("workflow-1", result)])

    async def test_transient_storage_error_is_re_raised_for_outbox_retry(self) -> None:
        checkpoints = _TransitioningCheckpoints()
        runtime = PersistedWorkflowRuntime(_FailingGraph(ConnectionError("database unavailable")), checkpoints)
        with self.assertRaisesRegex(ConnectionError, "database unavailable"):
            await runtime.run("workflow-1", {"status": "implementing"})
        self.assertEqual(checkpoints.transitions, [])
        self.assertEqual(checkpoints.saved, [])

    async def test_node_exception_without_transition_support_still_fails_gracefully(self) -> None:
        checkpoints = _Checkpoints()
        graph = _FailingGraph(RuntimeError("checkpointer unavailable"))
        runtime = PersistedWorkflowRuntime(graph, checkpoints)

        result = await runtime.run("workflow-1", {"status": "implementing"})

        self.assertEqual(result["status"], "failed")
        self.assertEqual(checkpoints.saved, [("workflow-1", result)])

    def test_factory_builds_langgraph_compatible_runtime(self) -> None:
        class Graph:
            async def ainvoke(self, state: dict[str, object], config: dict[str, object]) -> dict[str, object]:
                return state

        class StateGraph:
            def __init__(self, state_type: object) -> None:
                del state_type

            def add_node(self, *args: object) -> None:
                pass

            def add_edge(self, *args: object) -> None:
                pass

            def add_conditional_edges(self, *args: object) -> None:
                pass

            def compile(
                self,
                checkpointer: object = None,
                interrupt_before: object = None,
                interrupt_after: object = None,
            ) -> Graph:
                del checkpointer, interrupt_before, interrupt_after
                return Graph()

        package = ModuleType("langgraph")
        graph_module = ModuleType("langgraph.graph")
        graph_module.START = "start"
        graph_module.END = "end"
        graph_module.StateGraph = StateGraph
        previous_package = sys.modules.get("langgraph")
        previous_graph = sys.modules.get("langgraph.graph")
        sys.modules["langgraph"] = package
        sys.modules["langgraph.graph"] = graph_module
        try:
            runtime = build_persisted_runtime(object())
        finally:
            if previous_package is None:
                del sys.modules["langgraph"]
            else:
                sys.modules["langgraph"] = previous_package
            if previous_graph is None:
                del sys.modules["langgraph.graph"]
            else:
                sys.modules["langgraph.graph"] = previous_graph
        self.assertIsInstance(runtime, PersistedWorkflowRuntime)


if __name__ == "__main__":
    unittest.main()
