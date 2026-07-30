from __future__ import annotations

import inspect
import logging
from collections.abc import Awaitable, Callable
from datetime import datetime
from typing import Any, Protocol, cast

_LOGGER = logging.getLogger(__name__)


class WorkflowCheckpointStore(Protocol):
    async def latest_checkpoint(self, workflow_run_id: str) -> tuple[int, dict[str, object]] | None: ...

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int: ...

    async def load_state(self, workflow_run_id: str) -> dict[str, object]: ...

    async def get_open_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None: ...


class WorkflowGraph(Protocol):
    def ainvoke(self, state: object, config: dict[str, object]) -> Awaitable[dict[str, object]]: ...

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
    if checkpointer is None:
        # The graph suspends on the edge out of every dispatching node, and
        # resuming from that edge is a checkpointer feature: without one the
        # only way forward is to replay the graph from START, which re-enters
        # nodes whose executions already ran. Production must configure a
        # checkpointer (main.py treats a missing one as fatal unless
        # LOOP_ALLOW_NO_CHECKPOINTER is set).
        _LOGGER.warning(
            "workflow runtime built without a LangGraph checkpointer: "
            "workflows cannot resume from the node that queued an execution"
        )
    # Suspension is expressed as an edge to END rather than interrupt_after so
    # it stays conditional on a dispatch having actually happened; a static
    # interrupt would also stop on the passes where a node short-circuits
    # without queueing anything, and nothing would ever wake the run.
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
        state_updates = dict(initial_state)

        try:
            seed = await self._checkpoints.load_state(workflow_run_id)
            state_updates = {**seed, **initial_state}
            if str(state_updates.get("status", "")) in _TERMINAL_STATUSES:
                terminal_state = {**state_updates, "workflow_run_id": workflow_run_id}
                await self._checkpoints.checkpoint(workflow_run_id, terminal_state)
                return terminal_state
            if state_updates.get("awaiting_execution") is False and await self._execution_in_flight(
                workflow_run_id
            ):
                # This caller is delivering a runner transition: only those
                # clear the suspension gate (`runner_events` puts
                # `awaiting_execution: False` on every terminal transition, and
                # the stalled-run repair passes it explicitly). Yet the run
                # still has an execution open, so the transition has already
                # been applied and is being delivered a second time -- the
                # outbox is at-least-once. Advancing would walk the graph one
                # node past the execution it is actually waiting on: a phase
                # whose gates nobody has produced yet would be entered, and the
                # terminal event that eventually arrives would resume from that
                # wrong edge and skip the phase entirely. Re-assert the gate
                # instead, so the replay ends exactly where the first delivery
                # did.
                #
                # Testing for `is False` rather than "not True" keeps every
                # other entry point out of it. A human decision arrives with no
                # `awaiting_execution` key at all and has nothing to do with
                # the outbox, so it must not be gated on an execution request.
                #
                # Nothing can wedge here: the only ways a request stays open
                # are an execution that is genuinely running and one the
                # maintenance loop will close as `orphaned`.
                state_updates["awaiting_execution"] = True
            if self._has_checkpointer:
                app_checkpoint = await self._checkpoints.latest_checkpoint(workflow_run_id)
                if app_checkpoint is None:
                    result = self._graph.ainvoke({**state_updates, "workflow_run_id": workflow_run_id}, config)
                else:
                    prev_status = str(app_checkpoint[1].get("status", ""))
                    if prev_status in _TERMINAL_STATUSES and initial_state.get("status") not in _TERMINAL_STATUSES:
                        result = self._graph.ainvoke({**state_updates, "workflow_run_id": workflow_run_id}, config)
                    else:
                        await self._graph.aupdate_state(config, state_updates)
                        if state_updates.get("poll_github_checks") is True:
                            from langgraph.types import Command

                            result = self._graph.ainvoke(Command(resume="poll"), config)
                        elif any(key in state_updates for key in ("human_approved", "human_changes_requested", "human_guidance")):
                            from langgraph.types import Command

                            result = self._graph.ainvoke(Command(resume="human"), config)
                        else:
                            result = self._graph.ainvoke(None, config)
            else:
                checkpoint = await self._checkpoints.latest_checkpoint(workflow_run_id)
                state = {**(checkpoint[1] if checkpoint is not None else {}), **state_updates}
                state["workflow_run_id"] = workflow_run_id
                if state.get("awaiting_execution"):
                    # No checkpointer means the graph can only be replayed from
                    # START, which would re-enter the node that queued the
                    # execution this run is still waiting on. Persist whatever
                    # the caller reported and leave the run suspended.
                    await self._checkpoints.checkpoint(workflow_run_id, state)
                    return state
                result = self._graph.ainvoke(state, config)

            if inspect.isawaitable(result):
                state = await result
            else:
                state = result
        except Exception as error:
            if _is_transient_error(error):
                raise
            return await self._fail(workflow_run_id, state_updates, error)

        # run() reports every invalid input as ValueError (see the
        # workflow-run-ID guard above); a lone TypeError here would split one
        # contract across two exception types.
        if not isinstance(state, dict):
            raise ValueError("workflow graph returned an invalid state")  # noqa: TRY004
        state["workflow_run_id"] = workflow_run_id
        await self._checkpoints.checkpoint(workflow_run_id, state)
        return state

    async def _execution_in_flight(self, workflow_run_id: str) -> bool:
        return (await self._checkpoints.get_open_execution_request(workflow_run_id)) is not None

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
            await transition(workflow_run_id, "failed", {"status": "failed", "blocking_reason": reason})
        await self._checkpoints.checkpoint(workflow_run_id, failed_state)
        return failed_state


def _is_transient_error(error: Exception) -> bool:
    if isinstance(error, (ConnectionError, TimeoutError, OSError)):
        return True
    return type(error).__module__.startswith("asyncpg") and type(error).__name__ in {
        "CannotConnectNowError",
        "ConnectionDoesNotExistError",
        "ConnectionFailureError",
        "InterfaceError",
        "PostgresConnectionError",
        "SerializationError",
        "TooManyConnectionsError",
    }
