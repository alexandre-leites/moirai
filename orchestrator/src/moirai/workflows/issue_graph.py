from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Literal, TypedDict, cast

from .policy import GateState, RetryBudget, route_after_checks, route_after_human_response, route_after_pipeline, route_after_plan, route_after_review


class IssueWorkflowState(TypedDict, total=False):
    workflow_run_id: str
    project_id: str
    issue_id: str
    status: str
    planning_attempts: int
    implementation_attempts: int
    pipeline_repair_attempts: int
    review_cycles: int
    total_agent_executions: int
    plan_valid: bool
    pipeline_passed: bool
    review_approved: bool
    checks_passed: bool
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
WorkflowNode = Callable[[IssueWorkflowState], WorkflowUpdate | Awaitable[WorkflowUpdate]]


@dataclass(frozen=True)
class IssueWorkflowNodes:
    prepare: WorkflowNode
    plan: WorkflowNode
    implement: WorkflowNode
    pipeline: WorkflowNode
    review: WorkflowNode
    repair: WorkflowNode
    push: WorkflowNode
    create_pull_request: WorkflowNode
    wait_for_checks: WorkflowNode
    wait_for_human: WorkflowNode
    merge: WorkflowNode
    complete: WorkflowNode
    blocked: WorkflowNode


def route_plan(
    state: IssueWorkflowState, budget: RetryBudget = RetryBudget()
) -> Literal["plan", "implement", "blocked"]:
    return cast(Literal["plan", "implement", "blocked"], route_after_plan(_gate_state(state), budget).value)


def route_pipeline(
    state: IssueWorkflowState, budget: RetryBudget = RetryBudget()
) -> Literal["review", "repair", "blocked"]:
    return cast(Literal["review", "repair", "blocked"], route_after_pipeline(_gate_state(state), budget).value)


def route_review(
    state: IssueWorkflowState, budget: RetryBudget = RetryBudget()
) -> Literal["push", "repair", "blocked"]:
    return cast(Literal["push", "repair", "blocked"], route_after_review(_gate_state(state), budget).value)


def route_checks(
    state: IssueWorkflowState, budget: RetryBudget = RetryBudget()
) -> Literal["wait_for_human", "merge", "repair", "blocked"]:
    return cast(
        Literal["wait_for_human", "merge", "repair", "blocked"],
        route_after_checks(_gate_state(state), budget).value,
    )


def route_human(
    state: IssueWorkflowState, budget: RetryBudget = RetryBudget()
) -> Literal["merge", "repair", "blocked"]:
    del budget
    return cast(
        Literal["merge", "repair", "blocked"],
        route_after_human_response(
            state.get("human_approved", False),
            state.get("human_changes_requested", False),
        ).value,
    )


def build_issue_graph(
    nodes: IssueWorkflowNodes,
    budget: RetryBudget = RetryBudget(),
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
        lambda state: route_plan(state, budget),
        {"plan": "plan", "implement": "implement", "blocked": "blocked"},
    )
    graph.add_edge("implement", "pipeline")
    graph.add_conditional_edges(
        "pipeline",
        lambda state: route_pipeline(state, budget),
        {"review": "review", "repair": "repair", "blocked": "blocked"},
    )
    graph.add_conditional_edges(
        "review",
        lambda state: route_review(state, budget),
        {"push": "push", "repair": "repair", "blocked": "blocked"},
    )
    graph.add_edge("repair", "pipeline")
    graph.add_edge("push", "create_pull_request")
    graph.add_edge("create_pull_request", "wait_for_checks")
    graph.add_conditional_edges(
        "wait_for_checks",
        lambda state: route_checks(state, budget),
        {
            "wait_for_human": "wait_for_human",
            "merge": "merge",
            "repair": "repair",
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
        checkpointer=checkpointer,
        interrupt_after=list(interrupt_after) if interrupt_after is not None else None,
        interrupt_before=list(interrupt_before) if interrupt_before is not None else None,
    )


def _gate_state(state: IssueWorkflowState) -> GateState:
    return GateState(
        plan_valid=state.get("plan_valid", False),
        pipeline_passed=state.get("pipeline_passed", False),
        review_approved=state.get("review_approved", False),
        checks_passed=state.get("checks_passed", False),
        human_approval_required=state.get("human_approval_required", False),
        human_approved=state.get("human_approved", False),
        planning_attempts=state.get("planning_attempts", 0),
        implementation_attempts=state.get("implementation_attempts", 0),
        pipeline_repair_attempts=state.get("pipeline_repair_attempts", 0),
        review_cycles=state.get("review_cycles", 0),
        ci_repair_attempts=state.get("ci_repair_attempts", 0),
        total_agent_executions=state.get("total_agent_executions", 0),
    )
