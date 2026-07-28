from __future__ import annotations

import inspect
from collections.abc import Awaitable, Callable
from datetime import datetime
from typing import Any, Protocol, cast


class WorkflowCheckpointStore(Protocol):
    async def latest_checkpoint(self, workflow_run_id: str) -> tuple[int, dict[str, object]] | None: ...

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int: ...


class WorkflowGraph(Protocol):
    def ainvoke(
        self, state: dict[str, object] | None, config: dict[str, object]
    ) -> Awaitable[dict[str, object]]: ...

    def aupdate_state(
        self, config: dict[str, object], values: dict[str, object]
    ) -> Awaitable[object]: ...


def build_persisted_runtime(
    pool: Any,
    now: Callable[[], datetime] | None = None,
    checkpointer: object = None,
    code_host_factory: Any | None = None,
    issue_tracker_factory: Any | None = None,
) -> PersistedWorkflowRuntime:
    from .issue_graph import build_issue_graph
    from .nodes import PersistedWorkflowNodes
    from .persistence import AsyncpgWorkflowPersistence

    persistence = AsyncpgWorkflowPersistence(pool, now=now)
    interrupt_after = None
    interrupt_before = ("wait_for_human",) if checkpointer else None
    graph = build_issue_graph(
        PersistedWorkflowNodes(
            persistence,
            persistence,
            code_host_factory=code_host_factory,
            issue_tracker_factory=issue_tracker_factory,
        ).build(),
        checkpointer=checkpointer,
        interrupt_after=interrupt_after,
        interrupt_before=interrupt_before,
    )
    # build_issue_graph() intentionally returns `object` so this module does not
    # need langgraph's own (RunnableConfig-keyed) types. WorkflowGraph describes
    # the subset of the real CompiledStateGraph API this runtime depends on.
    return PersistedWorkflowRuntime(cast(WorkflowGraph, graph), persistence, has_checkpointer=checkpointer is not None)


class PersistedWorkflowRuntime:
    def __init__(self, graph: WorkflowGraph, checkpoints: WorkflowCheckpointStore, has_checkpointer: bool = False) -> None:
        self._graph = graph
        self._checkpoints = checkpoints
        self._has_checkpointer = has_checkpointer

    async def run(self, workflow_run_id: str, initial_state: dict[str, object]) -> dict[str, object]:
        if not workflow_run_id:
            raise ValueError("workflow run ID is required")
        config: dict[str, object] = {"configurable": {"thread_id": workflow_run_id}}

        _TERMINAL_STATUSES = frozenset({"blocked", "completed", "cancelled", "failed"})

        try:
            if self._has_checkpointer:
                app_checkpoint = await self._checkpoints.latest_checkpoint(workflow_run_id)
                if app_checkpoint is None:
                    state = dict(initial_state)
                    state["workflow_run_id"] = workflow_run_id
                    result = self._graph.ainvoke(state, config)
                else:
                    prev_status = str(app_checkpoint[1].get("status", ""))
                    if prev_status in _TERMINAL_STATUSES and initial_state.get("status") not in _TERMINAL_STATUSES:
                        state = dict(initial_state)
                        state["workflow_run_id"] = workflow_run_id
                        result = self._graph.ainvoke(state, config)
                    else:
                        await self._graph.aupdate_state(config, initial_state)
                        result = self._graph.ainvoke(None, config)
            else:
                checkpoint = await self._checkpoints.latest_checkpoint(workflow_run_id)
                state = dict(initial_state if checkpoint is None else checkpoint[1])
                state["workflow_run_id"] = workflow_run_id
                result = self._graph.ainvoke(state, config)

            if inspect.isawaitable(result):
                state = await result
            else:
                state = result
        except Exception as error:  # noqa: BLE001 - any node/checkpointer failure must not strand the run
            return await self._fail(workflow_run_id, initial_state, error)

        # run() reports every invalid input as ValueError (see the
        # workflow-run-ID guard above); a lone TypeError here would split one
        # contract across two exception types.
        if not isinstance(state, dict):
            raise ValueError("workflow graph returned an invalid state")  # noqa: TRY004
        state["workflow_run_id"] = workflow_run_id
        await self._checkpoints.checkpoint(workflow_run_id, state)
        return state

    async def _fail(
        self, workflow_run_id: str, initial_state: dict[str, object], error: Exception
    ) -> dict[str, object]:
        reason = f"{type(error).__name__}: {error}"[:500]
        failed_state = dict(initial_state)
        failed_state["workflow_run_id"] = workflow_run_id
        failed_state["status"] = "failed"
        failed_state["blocking_reason"] = reason
        transition = getattr(self._checkpoints, "transition", None)
        if transition is not None:
            try:
                await transition(workflow_run_id, "failed", {"status": "failed", "blocking_reason": reason})
            except Exception:  # noqa: BLE001, S110 - best-effort; the checkpoint below still records the failure
                pass
        try:
            await self._checkpoints.checkpoint(workflow_run_id, failed_state)
        except Exception:  # noqa: BLE001, S110 - nothing further to do if durable storage itself is failing
            pass
        return failed_state
