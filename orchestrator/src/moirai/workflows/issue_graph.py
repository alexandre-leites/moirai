from __future__ import annotations

from collections.abc import Callable
from dataclasses import dataclass
from typing import TYPE_CHECKING, Literal, Protocol, TypedDict, cast

from .policy import (
    GateState,
    RetryBudget,
    route_after_checks,
    route_after_human_response,
    route_after_pipeline,
    route_after_plan,
    route_after_review,
)

if TYPE_CHECKING:
    from langgraph.types import Checkpointer


class IssueWorkflowState(TypedDict, total=False):
    workflow_run_id: str
    project_id: str
    issue_id: str
    status: str
    # Set by the nodes that queue an agent execution and cleared by the runner's
    # terminal event (see runner_events.workflow_transition_for_terminal_event).
    # While it is set the graph must not walk downstream nodes: their gates are
    # decided by an execution that has not run yet.
    awaiting_execution: bool
    execution_id: str
    planning_attempts: int
    implementation_attempts: int
    pipeline_repair_attempts: int
    review_cycles: int
    total_agent_executions: int
    plan_valid: bool
    pipeline_passed: bool
    review_approved: bool
    checks_passed: bool
    checks_pending: bool
    human_approval_required: bool
    human_approved: bool
    human_changes_requested: bool
    ci_repair_attempts: int
    blocking_reason: str
    branch_name: str
    base_branch: str
    pull_request_id: str
    pull_request_url: str
    merge_method: str


WorkflowUpdate = dict[str, object]


class WorkflowNode(Protocol):
    """A single graph node: takes the workflow state, returns a state update.

    Spelled as a Protocol rather than `Callable[[IssueWorkflowState], ...]`
    because langgraph's `StateGraph.add_node` expects a node whose state
    parameter can be passed by name. A bare `Callable` alias makes that
    parameter positional-only, which does not satisfy langgraph's node
    protocol under `mypy --strict`.
    """

    async def __call__(self, state: IssueWorkflowState) -> WorkflowUpdate: ...


# RetryBudget is a frozen dataclass, so one shared default instance is safe
# and avoids constructing it in argument defaults (ruff B008).
_DEFAULT_BUDGET = RetryBudget()


@dataclass(frozen=True)
class IssueWorkflowNodes:
    prepare: WorkflowNode
    plan: WorkflowNode
    implement: WorkflowNode
    pipeline: WorkflowNode
    review: WorkflowNode
    repair: WorkflowNode
    # Same repairer role and phase as `repair`; separate node so the failing-
    # checks gate spends `ci_repair_attempts` instead of the local pipeline's
    # repair budget.
    ci_repair: WorkflowNode
    push: WorkflowNode
    create_pull_request: WorkflowNode
    wait_for_checks: WorkflowNode
    wait_for_human: WorkflowNode
    merge: WorkflowNode
    complete: WorkflowNode
    blocked: WorkflowNode


def route_plan(
    state: IssueWorkflowState, budget: RetryBudget = _DEFAULT_BUDGET
) -> Literal["plan", "implement", "blocked"]:
    return cast(Literal["plan", "implement", "blocked"], route_after_plan(_gate_state(state), budget).value)


def route_pipeline(
    state: IssueWorkflowState, budget: RetryBudget = _DEFAULT_BUDGET
) -> Literal["review", "repair", "blocked"]:
    return cast(Literal["review", "repair", "blocked"], route_after_pipeline(_gate_state(state), budget).value)


def route_review(
    state: IssueWorkflowState, budget: RetryBudget = _DEFAULT_BUDGET
) -> Literal["push", "repair", "blocked"]:
    return cast(Literal["push", "repair", "blocked"], route_after_review(_gate_state(state), budget).value)


def route_checks(
    state: IssueWorkflowState, budget: RetryBudget = _DEFAULT_BUDGET
) -> Literal["wait_for_checks", "wait_for_human", "merge", "ci_repair", "blocked"]:
    return cast(
        Literal["wait_for_checks", "wait_for_human", "merge", "ci_repair", "blocked"],
        route_after_checks(_gate_state(state), budget).value,
    )


def route_human(
    state: IssueWorkflowState, budget: RetryBudget = _DEFAULT_BUDGET
) -> Literal["merge", "repair", "blocked"]:
    del budget
    return cast(
        Literal["merge", "repair", "blocked"],
        route_after_human_response(
            state.get("human_approved", False),
            state.get("human_changes_requested", False),
        ).value,
    )


# Branch name meaning "this node queued an agent execution; end the invocation".
# It is not a node: every path map below points it at END. Prefixed so it can
# never collide with a real node name.
_SUSPEND = "__awaiting_execution__"


def suspend_after_dispatch(
    router: Callable[[IssueWorkflowState], str],
) -> Callable[[IssueWorkflowState], str]:
    """Wraps a phase router so a node that just queued an agent execution ends
    the graph invocation instead of walking downstream nodes.

    The gates those nodes read (`plan_valid`, `pipeline_passed`, ...) are
    produced by the execution that was only just queued, so continuing would
    route on stale defaults, dispatch phantom executions and burn the retry
    budget before any agent ran. The next terminal runner event clears
    `awaiting_execution` and resumes the graph from this same edge.

    A node that reports `blocked` instead of dispatching short-circuits to the
    terminal node, so an exhausted budget can never fall through an
    unconditional edge into another dispatching node.
    """

    def route(state: IssueWorkflowState) -> str:
        if state.get("status") == "blocked":
            return "blocked"
        if state.get("awaiting_execution"):
            return _SUSPEND
        return router(state)

    return route


def build_issue_graph(
    nodes: IssueWorkflowNodes,
    budget: RetryBudget = _DEFAULT_BUDGET,
    checkpointer: object = None,
    interrupt_after: tuple[str, ...] | None = None,
    interrupt_before: tuple[str, ...] | None = None,
) -> object:
    from langgraph.graph import END, START, StateGraph

    graph = StateGraph(IssueWorkflowState)
    graph.add_node("prepare", nodes.prepare)
    graph.add_node("plan", nodes.plan)
    graph.add_node("implement", nodes.implement)
    graph.add_node("pipeline", nodes.pipeline)
    graph.add_node("review", nodes.review)
    graph.add_node("repair", nodes.repair)
    graph.add_node("ci_repair", nodes.ci_repair)
    graph.add_node("push", nodes.push)
    graph.add_node("create_pull_request", nodes.create_pull_request)
    graph.add_node("wait_for_checks", nodes.wait_for_checks)
    graph.add_node("wait_for_human", nodes.wait_for_human)
    graph.add_node("merge", nodes.merge)
    graph.add_node("complete", nodes.complete)
    graph.add_node("blocked", nodes.blocked)
    graph.add_edge(START, "prepare")
    graph.add_edge("prepare", "plan")
    graph.add_conditional_edges(
        "plan",
        suspend_after_dispatch(lambda state: route_plan(state, budget)),
        {"plan": "plan", "implement": "implement", "blocked": "blocked", _SUSPEND: END},
    )
    graph.add_conditional_edges(
        "implement",
        suspend_after_dispatch(lambda state: "pipeline"),
        {"pipeline": "pipeline", "blocked": "blocked", _SUSPEND: END},
    )
    graph.add_conditional_edges(
        "pipeline",
        suspend_after_dispatch(lambda state: route_pipeline(state, budget)),
        {"review": "review", "repair": "repair", "blocked": "blocked", _SUSPEND: END},
    )
    graph.add_conditional_edges(
        "review",
        suspend_after_dispatch(lambda state: route_review(state, budget)),
        {"push": "push", "repair": "repair", "blocked": "blocked", _SUSPEND: END},
    )
    graph.add_conditional_edges(
        "repair",
        suspend_after_dispatch(lambda state: "pipeline"),
        {"pipeline": "pipeline", "blocked": "blocked", _SUSPEND: END},
    )
    # A CI repair rejoins the workflow exactly where a local repair does: the
    # repaired tree gets its own local pipeline verdict, then AI review, push
    # and a fresh round of checks on the same pull request. It is a separate
    # node only so it spends `ci_repair_attempts`.
    graph.add_conditional_edges(
        "ci_repair",
        suspend_after_dispatch(lambda state: "pipeline"),
        {"pipeline": "pipeline", "blocked": "blocked", _SUSPEND: END},
    )
    graph.add_conditional_edges(
        "push",
        suspend_after_dispatch(lambda state: "create_pull_request"),
        {"create_pull_request": "create_pull_request", "blocked": "blocked", _SUSPEND: END},
    )
    graph.add_edge("create_pull_request", "wait_for_checks")
    graph.add_conditional_edges(
        "wait_for_checks",
        lambda state: route_checks(state, budget),
        {
            "wait_for_checks": END,
            "wait_for_human": "wait_for_human",
            "merge": "merge",
            "ci_repair": "ci_repair",
            "blocked": "blocked",
        },
    )
    graph.add_conditional_edges(
        "wait_for_human",
        lambda state: route_human(state, budget),
        {"merge": "merge", "repair": "repair", "blocked": "blocked"},
    )
    graph.add_edge("merge", "complete")
    graph.add_edge("complete", END)
    graph.add_edge("blocked", END)
    return graph.compile(
        # `checkpointer` stays `object` in this signature so callers need no
        # langgraph types; narrow it here, where langgraph is already imported.
        checkpointer=cast("Checkpointer", checkpointer),
        interrupt_after=list(interrupt_after) if interrupt_after is not None else None,
        interrupt_before=list(interrupt_before) if interrupt_before is not None else None,
    )


def _gate_state(state: IssueWorkflowState) -> GateState:
    return GateState(
        plan_valid=state.get("plan_valid", False),
        pipeline_passed=state.get("pipeline_passed", False),
        review_approved=state.get("review_approved", False),
        checks_passed=state.get("checks_passed", False),
        checks_pending=state.get("checks_pending", False),
        human_approval_required=state.get("human_approval_required", False),
        human_approved=state.get("human_approved", False),
        planning_attempts=state.get("planning_attempts", 0),
        implementation_attempts=state.get("implementation_attempts", 0),
        pipeline_repair_attempts=state.get("pipeline_repair_attempts", 0),
        review_cycles=state.get("review_cycles", 0),
        ci_repair_attempts=state.get("ci_repair_attempts", 0),
        total_agent_executions=state.get("total_agent_executions", 0),
    )
