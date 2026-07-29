from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Protocol

from moirai.code_hosts import ChecksResult, CodeHost, GitHubCliError, checks_result
from moirai.issue_trackers import IssueTracker

from .issue_graph import IssueWorkflowNodes, IssueWorkflowState, WorkflowUpdate
from .policy import RetryBudget

CodeHostFactory = Callable[[str], "CodeHost | None | Awaitable[CodeHost | None]"]
IssueTrackerFactory = Callable[[str], "IssueTracker | None | Awaitable[IssueTracker | None]"]

# Single source of truth for retry limits, shared with policy.py's routing
# functions so the two can no longer drift out of sync by coincidence.
_BUDGET = RetryBudget()

# Roles the runner executes without an agent. The local pipeline runs the
# project's configured commands directly -- `dispatch.Dispatcher` does not even
# require an agent backend for it -- so it does not spend
# `total_agent_executions`, which budgets *agent* runs. Counting it would mean
# that making the pipeline mandatory silently cut the number of agent attempts a
# workflow gets: a review-driven repair cycle would cost three units instead of
# two, and an approved review could land on an exhausted budget and block with
# the work finished but no pull request. It cannot run away either: `pipeline` is
# reachable only from `implement`, `repair` and `ci_repair`, each of which
# dispatches a counted agent execution first and is capped by its own attempt
# counter.
_NON_AGENT_ROLES = frozenset({"pipeline"})


class WorkflowPersistence(Protocol):
    async def transition(
        self, workflow_run_id: str, status: str, updates: dict[str, object]
    ) -> None: ...

    async def get_queued_execution_request(
        self, workflow_run_id: str
    ) -> dict[str, Any] | None: ...


class ExecutionDispatcher(Protocol):
    async def dispatch(self, workflow_run_id: str, role: str) -> str: ...


async def _await(value: Any) -> Any:
    import inspect

    if inspect.isawaitable(value):
        return await value
    return value


@dataclass(frozen=True)
class PersistedWorkflowNodes:
    persistence: WorkflowPersistence
    dispatcher: ExecutionDispatcher
    code_host_factory: CodeHostFactory | None = None
    issue_tracker_factory: IssueTrackerFactory | None = None

    def build(self) -> IssueWorkflowNodes:
        return IssueWorkflowNodes(
            prepare=self.prepare,
            plan=self.plan,
            implement=self.implement,
            pipeline=self.pipeline,
            review=self.review,
            repair=self.repair,
            ci_repair=self.ci_repair,
            push=self.push,
            create_pull_request=self.create_pull_request,
            wait_for_checks=self.wait_for_checks,
            wait_for_human=self.wait_for_human,
            merge=self.merge,
            complete=self.complete,
            blocked=self.blocked,
        )

    async def prepare(self, state: IssueWorkflowState) -> WorkflowUpdate:
        return await self._transition(state, "planning", {"status": "planning"})

    async def plan(self, state: IssueWorkflowState) -> WorkflowUpdate:
        if state.get("plan_valid"):
            return await self._transition(
                state, "planning", {"status": "planning", "plan_valid": True, "awaiting_execution": False}
            )
        if int(state.get("planning_attempts", 0)) >= _BUDGET.planning_attempts:
            return await self._budget_exhausted(state)
        return await self._dispatch(state, "planner", "planning", "planning_attempts")

    async def implement(self, state: IssueWorkflowState) -> WorkflowUpdate:
        if int(state.get("implementation_attempts", 0)) >= _BUDGET.implementation_attempts:
            return await self._budget_exhausted(state)
        return await self._dispatch(state, "developer", "implementing", "implementation_attempts")

    async def pipeline(self, state: IssueWorkflowState) -> WorkflowUpdate:
        # Unconditional: the local pipeline is the deterministic gate that
        # decides whether the work is complete, so entering this phase always
        # dispatches a real pipeline execution. This node deliberately never
        # reads or writes `pipeline_passed` -- the gate belongs to the pipeline
        # execution's terminal event alone (runner_events.py). Short-circuiting
        # on an inherited `pipeline_passed` would skip the gate exactly when the
        # previous phase claimed success, and would leave repaired work carrying
        # the verdict of the pipeline run that predates the repair.
        return await self._dispatch(state, "pipeline", "local_pipeline", None)

    async def review(self, state: IssueWorkflowState) -> WorkflowUpdate:
        if int(state.get("review_cycles", 0)) >= _BUDGET.review_cycles:
            return await self._budget_exhausted(state)
        return await self._dispatch(state, "reviewer", "ai_review", "review_cycles")

    async def repair(self, state: IssueWorkflowState) -> WorkflowUpdate:
        # Repairs asked for before the work leaves the machine: a failing local
        # pipeline, an AI review requesting changes, a human requesting changes.
        if int(state.get("pipeline_repair_attempts", 0)) >= _BUDGET.pipeline_repair_attempts:
            return await self._budget_exhausted(state)
        return await self._dispatch(state, "repairer", "repairing", "pipeline_repair_attempts")

    async def ci_repair(self, state: IssueWorkflowState) -> WorkflowUpdate:
        # Repairs asked for by failing GitHub checks. Same repairer role and
        # `repairing` phase as `repair` -- the runner does the same work and
        # `runner_events` translates the terminal event the same way -- but a
        # distinct counter, because which node dispatched the repair is the only
        # record of why it happened. Dispatching CI repairs from `repair` left
        # `ci_repair_attempts` with no writer at all, so `route_after_checks`
        # gated on a counter frozen at 0 (the CI bound could never trip) while
        # every CI repair quietly spent the local pipeline's repair budget.
        #
        # Sharing the role costs one invariant: `_dispatch`'s replay guard
        # identifies "my own queued request" by role, so it can no longer tell
        # a replayed `repair` from a replayed `ci_repair`. Adopting the other
        # node's queued request would skip an attempt increment. It is
        # unreachable today because reaching the other repair node requires a
        # terminal event for the first execution, and a terminal event is only
        # accepted for a request whose row is still `dispatched`
        # (`_resolve_execution_identity` in persistence/control_plane.py) --
        # never for one that offer recovery returned to `queued`. Anything that
        # relaxes that has to give the two nodes distinguishable requests.
        if int(state.get("ci_repair_attempts", 0)) >= _BUDGET.ci_repair_attempts:
            return await self._budget_exhausted(state)
        return await self._dispatch(state, "repairer", "repairing", "ci_repair_attempts")

    async def push(self, state: IssueWorkflowState) -> WorkflowUpdate:
        return await self._dispatch(state, "developer", "pushing", None)

    async def create_pull_request(self, state: IssueWorkflowState) -> WorkflowUpdate:
        code_host = await self._resolve_code_host(state)
        if code_host is None:
            return await self._transition(state, "pr_created", {"status": "pr_created"})
        branch = state.get("branch_name", "")
        base_branch = state.get("base_branch", "main")
        issue_id = state.get("issue_id", "")
        workflow_run_id = _workflow_run_id(state)
        pr = await code_host.create_or_find_pull_request(
            workflow_id=workflow_run_id,
            branch=branch,
            base_branch=base_branch,
            title=f"Issue #{issue_id}: {branch}",
            issue_number=issue_id,
        )
        return await self._transition(state, "pr_created", {
            "status": "pr_created",
            "pull_request_id": pr.external_id,
            "pull_request_url": pr.url,
            "pull_request_head_commit": pr.head_commit,
            "pull_request_state": pr.state,
        })

    async def wait_for_checks(self, state: IssueWorkflowState) -> WorkflowUpdate:
        code_host = await self._resolve_code_host(state)
        if code_host is None or not state.get("pull_request_id"):
            return await self._transition(state, "waiting_github_checks", {"status": "waiting_github_checks"})
        checks = await code_host.required_checks(state["pull_request_id"])
        outcome = checks_result(checks)
        return await self._transition(state, "waiting_github_checks", {
            "status": "waiting_github_checks",
            "checks_passed": outcome is ChecksResult.PASSED,
            "checks_pending": outcome is ChecksResult.PENDING,
        })

    async def wait_for_human(self, state: IssueWorkflowState) -> WorkflowUpdate:
        updates: WorkflowUpdate = {"status": "waiting_human"}
        if state.get("human_approved"):
            updates["human_approved"] = True
        elif state.get("human_changes_requested"):
            updates["human_changes_requested"] = True
            updates["human_approved"] = False
        else:
            updates["human_approved"] = False
        return await self._transition(state, "waiting_human", updates)

    async def merge(self, state: IssueWorkflowState) -> WorkflowUpdate:
        code_host = await self._resolve_code_host(state)
        if code_host is not None and state.get("pull_request_id"):
            method = state.get("merge_method", "squash")
            try:
                pull_request = await code_host.get_pull_request(state["pull_request_id"])
                if pull_request.state.lower() != "merged":
                    await code_host.merge_pull_request(state["pull_request_id"], method)
            except GitHubCliError as error:
                return await self._transition(state, "blocked", {
                    "status": "blocked",
                    "blocking_reason": f"merge failed: {error}",
                })
        return await self._transition(state, "merging", {"status": "merging"})

    async def complete(self, state: IssueWorkflowState) -> WorkflowUpdate:
        issue_tracker = await self._resolve_issue_tracker(state)
        if issue_tracker is not None and state.get("issue_id"):
            await issue_tracker.close_issue(state["issue_id"])
            await issue_tracker.add_labels(state["issue_id"], ["agent:delivered"])
        return await self._transition(state, "completed", {"status": "completed"})

    async def blocked(self, state: IssueWorkflowState) -> WorkflowUpdate:
        reason = state.get("blocking_reason") or "workflow retry budget exhausted"
        return await self._transition(
            state, "blocked", {"status": "blocked", "blocking_reason": reason}
        )

    async def _resolve_code_host(self, state: IssueWorkflowState) -> CodeHost | None:
        if self.code_host_factory is None:
            return None
        return await _await(self.code_host_factory(state.get("project_id", "")))

    async def _resolve_issue_tracker(self, state: IssueWorkflowState) -> IssueTracker | None:
        if self.issue_tracker_factory is None:
            return None
        return await _await(self.issue_tracker_factory(state.get("project_id", "")))

    async def _budget_exhausted(self, state: IssueWorkflowState) -> WorkflowUpdate:
        return await self._transition(state, "blocked", {
            "status": "blocked",
            "blocking_reason": "workflow retry budget exhausted",
            "awaiting_execution": False,
        })

    async def _dispatch(
        self,
        state: IssueWorkflowState,
        role: str,
        status: str,
        attempt_counter: str | None,
    ) -> WorkflowUpdate:
        workflow_run_id = _workflow_run_id(state)
        # The exhaustion check applies to every role, including the ones that do
        # not spend the budget: once no agent run is affordable, the pipeline's
        # verdict has nowhere to route (both of its successors dispatch agents),
        # so validating first would only cost a runner execution to reach the
        # same blocked state.
        if int(state.get("total_agent_executions", 0)) >= _BUDGET.total_agent_executions:
            return await self._budget_exhausted(state)
        existing = await _await(self.persistence.get_queued_execution_request(workflow_run_id))
        if existing is not None and existing["role"] == role:
            # This node already queued its request and the scheduler has not
            # claimed it yet, so the node is being replayed (at-least-once
            # transition delivery, or a resume that re-entered the node).
            # Queueing a second request would duplicate the agent run, and
            # re-counting the attempt would spend two units of retry budget on
            # one execution.
            reused: WorkflowUpdate = {
                "status": status,
                "execution_id": existing["id"],
                "awaiting_execution": True,
            }
            await _await(self.persistence.transition(workflow_run_id, status, reused))
            return reused
        execution_id = await _await(self.dispatcher.dispatch(workflow_run_id, role))
        # `awaiting_execution` suspends the graph on the outgoing edge: the
        # gates the downstream nodes read only exist once this execution
        # reports a terminal event.
        updates: WorkflowUpdate = {
            "status": status,
            "execution_id": execution_id,
            "awaiting_execution": True,
        }
        if attempt_counter is not None:
            current_attempts = state.get(attempt_counter, 0)
            updates[attempt_counter] = (current_attempts if isinstance(current_attempts, int) else 0) + 1
        if role not in _NON_AGENT_ROLES:
            updates["total_agent_executions"] = int(state.get("total_agent_executions", 0)) + 1
        await _await(self.persistence.transition(workflow_run_id, status, updates))
        return updates

    async def _transition(
        self, state: IssueWorkflowState, status: str, updates: WorkflowUpdate
    ) -> WorkflowUpdate:
        await _await(self.persistence.transition(_workflow_run_id(state), status, updates))
        return updates


def _workflow_run_id(state: IssueWorkflowState) -> str:
    workflow_run_id = state.get("workflow_run_id")
    if not isinstance(workflow_run_id, str) or not workflow_run_id:
        raise ValueError("workflow run ID is required")
    return workflow_run_id
