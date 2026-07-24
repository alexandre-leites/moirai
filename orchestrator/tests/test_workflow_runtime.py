import sys
from types import ModuleType
import unittest

from moirai.workflows.runtime import PersistedWorkflowRuntime, build_persisted_runtime


class _Checkpoints:
    def __init__(self, checkpoint: tuple[int, dict[str, object]] | None = None) -> None:
        self.current = checkpoint
        self.saved: list[tuple[str, dict[str, object]]] = []

    async def latest_checkpoint(self, workflow_run_id: str) -> tuple[int, dict[str, object]] | None:
        del workflow_run_id
        return self.current

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int:
        self.saved.append((workflow_run_id, state))
        return len(self.saved)


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

    async def test_resumes_from_latest_checkpoint(self) -> None:
        checkpoints = _Checkpoints((3, {"status": "implementing", "planning_attempts": 1}))
        graph = _Graph({"status": "local_pipeline"})
        runtime = PersistedWorkflowRuntime(graph, checkpoints)
        await runtime.run("workflow-1", {"status": "offered"})
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
        setattr(graph_module, "START", "start")
        setattr(graph_module, "END", "end")
        setattr(graph_module, "StateGraph", StateGraph)
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
