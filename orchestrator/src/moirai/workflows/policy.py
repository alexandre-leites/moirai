from __future__ import annotations

from dataclasses import dataclass
from enum import StrEnum


class WorkflowRoute(StrEnum):
    PLAN = "plan"
    IMPLEMENT = "implement"
    PIPELINE = "pipeline"
    REVIEW = "review"
    # Two repair routes for one `repairer` role. They exist so the gate that
    # asked for the repair decides which budget the repair spends: a failing
    # local pipeline (or AI review, or a human asking for changes) spends
    # `pipeline_repair_attempts`, a failing GitHub check spends
    # `ci_repair_attempts`. Collapsing them back into one route would make one
    # counter dead again and let one failure source drain the other's budget.
    REPAIR = "repair"
    CI_REPAIR = "ci_repair"
    PUSH = "push"
    CREATE_PULL_REQUEST = "create_pull_request"
    WAIT_FOR_CHECKS = "wait_for_checks"
    WAIT_FOR_HUMAN = "wait_for_human"
    MERGE = "merge"
    COMPLETE = "complete"
    BLOCKED = "blocked"


@dataclass(frozen=True)
class RetryBudget:
    planning_attempts: int = 2
    implementation_attempts: int = 3
    continuation_attempts: int = 3
    pipeline_repair_attempts: int = 3
    review_cycles: int = 3
    ci_repair_attempts: int = 3
    github_check_poll_attempts: int = 20
    total_agent_executions: int = 10
    # How many times the merge node re-reads a pull request the code host has
    # not yet reported merged before the run blocks. Spent inside one node
    # entry, so nothing has to re-enter the node for the bound to be reachable.
    # It costs no agent execution -- it bounds waiting, not work.
    merge_verification_attempts: int = 5


@dataclass(frozen=True)
class GateState:
    plan_valid: bool = False
    pipeline_passed: bool = False
    review_approved: bool = False
    checks_passed: bool = False
    checks_pending: bool = False
    human_approval_required: bool = False
    human_approved: bool = False
    planning_attempts: int = 0
    implementation_attempts: int = 0
    pipeline_repair_attempts: int = 0
    review_cycles: int = 0
    ci_repair_attempts: int = 0
    total_agent_executions: int = 0
    # Set only by the merge node, and only from a pull request the code host
    # reported merged after the merge was asked for. It defaults to False so
    # every path that has not verified a merge -- including one that never
    # reached the merge node -- routes as "not merged".
    pull_request_merged: bool = False


def route_after_plan(state: GateState, budget: RetryBudget) -> WorkflowRoute:
    if state.plan_valid:
        return _agent_budget_route(state, budget, WorkflowRoute.IMPLEMENT)
    if state.planning_attempts < budget.planning_attempts:
        return _agent_budget_route(state, budget, WorkflowRoute.PLAN)
    return WorkflowRoute.BLOCKED


def route_after_pipeline(state: GateState, budget: RetryBudget) -> WorkflowRoute:
    if state.pipeline_passed:
        return _agent_budget_route(state, budget, WorkflowRoute.REVIEW)
    if state.pipeline_repair_attempts < budget.pipeline_repair_attempts:
        return _agent_budget_route(state, budget, WorkflowRoute.REPAIR)
    return WorkflowRoute.BLOCKED


def route_after_review(state: GateState, budget: RetryBudget) -> WorkflowRoute:
    if state.review_approved:
        return WorkflowRoute.PUSH
    if state.review_cycles < budget.review_cycles:
        return _agent_budget_route(state, budget, WorkflowRoute.REPAIR)
    return WorkflowRoute.BLOCKED


def route_after_checks(state: GateState, budget: RetryBudget) -> WorkflowRoute:
    if not state.pipeline_passed or not state.review_approved:
        return WorkflowRoute.BLOCKED
    if state.checks_pending:
        return WorkflowRoute.WAIT_FOR_CHECKS
    if state.checks_passed:
        if state.human_approval_required and not state.human_approved:
            return WorkflowRoute.WAIT_FOR_HUMAN
        return WorkflowRoute.MERGE
    # Failing required checks. CI_REPAIR, not REPAIR: the route is what makes
    # the dispatched repair count against `ci_repair_attempts`, which is the
    # counter this branch just gated on. Routing to REPAIR here would spend
    # `pipeline_repair_attempts` instead, leaving this bound unreachable.
    if state.ci_repair_attempts < budget.ci_repair_attempts:
        return _agent_budget_route(state, budget, WorkflowRoute.CI_REPAIR)
    return WorkflowRoute.BLOCKED


def route_after_merge(state: GateState) -> WorkflowRoute:
    """Where the graph goes once the merge node has reported.

    COMPLETE is reachable on one condition only: the code host confirmed the
    pull request merged. Completion closes the issue and applies
    `agent:delivered`, and `PROJECT.md` lists merge among the deterministic
    gates that must hold before that happens, so a merge command that returned
    without an error is not enough.

    Everything else is BLOCKED. The merge node already reports `blocked`
    itself, with the reason, on every path that is not a confirmed merge -- so
    in practice this arm is the invariant rather than the decision: no future
    edit to the node can reach `complete` by returning some other status
    without a verified merge.
    """
    if state.pull_request_merged:
        return WorkflowRoute.COMPLETE
    return WorkflowRoute.BLOCKED


def route_after_human_response(approved: bool, changes_requested: bool) -> WorkflowRoute:
    if approved:
        return WorkflowRoute.MERGE
    if changes_requested:
        # REPAIR, not CI_REPAIR: a human asking for changes is not a CI
        # failure, and `RetryBudget` has no third repair counter to give it, so
        # it keeps spending `pipeline_repair_attempts` as it always has.
        return WorkflowRoute.REPAIR
    return WorkflowRoute.BLOCKED


def _agent_budget_route(state: GateState, budget: RetryBudget, route: WorkflowRoute) -> WorkflowRoute:
    if state.total_agent_executions >= budget.total_agent_executions:
        return WorkflowRoute.BLOCKED
    return route
