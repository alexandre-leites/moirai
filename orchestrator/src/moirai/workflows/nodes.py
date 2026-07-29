from __future__ import annotations

import logging
from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, NamedTuple, Protocol

from moirai.code_hosts import (
    ChecksResult,
    CodeHost,
    GitHubCliError,
    PullRequest,
    checks_result,
)
from moirai.issue_trackers import IssueTracker

from .issue_graph import IssueWorkflowNodes, IssueWorkflowState, WorkflowUpdate
from .policy import RetryBudget

_LOGGER = logging.getLogger(__name__)

CodeHostFactory = Callable[[str], "CodeHost | None | Awaitable[CodeHost | None]"]
IssueTrackerFactory = Callable[[str], "IssueTracker | None | Awaitable[IssueTracker | None]"]


class AttemptBudget(NamedTuple):
    """A phase's own retry counter and the limit that bounds it.

    One value rather than two parameters so "a counter with no limit" -- which
    would increment forever with nothing to stop it -- cannot be expressed. The
    phases that have neither (`pipeline`, `push`) pass None.
    """

    counter: str
    limit: int


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

    async def get_open_execution_request(
        self, workflow_run_id: str
    ) -> dict[str, Any] | None: ...


class ExecutionDispatcher(Protocol):
    async def dispatch(self, workflow_run_id: str, role: str) -> dict[str, Any]: ...


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
        return await self._dispatch(
            state, "planner", "planning",
            AttemptBudget("planning_attempts", _BUDGET.planning_attempts),
        )

    async def implement(self, state: IssueWorkflowState) -> WorkflowUpdate:
        return await self._dispatch(
            state, "developer", "implementing",
            AttemptBudget("implementation_attempts", _BUDGET.implementation_attempts),
        )

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
        return await self._dispatch(
            state, "reviewer", "ai_review",
            AttemptBudget("review_cycles", _BUDGET.review_cycles),
        )

    async def repair(self, state: IssueWorkflowState) -> WorkflowUpdate:
        # Repairs asked for before the work leaves the machine: a failing local
        # pipeline, an AI review requesting changes, a human requesting changes.
        return await self._dispatch(
            state, "repairer", "repairing",
            AttemptBudget("pipeline_repair_attempts", _BUDGET.pipeline_repair_attempts),
        )

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
        # identifies "my own open request" by role, so it cannot tell a
        # replayed `repair` from a replayed `ci_repair`, and adopting the other
        # node's request would skip an attempt increment. Reaching the other
        # repair node requires a terminal event for the first execution, and
        # `accept_event` closes the request row in the same transaction that
        # records that event (issue #94), so by the time the graph resumes
        # there is no open request left to adopt. Anything that lets the graph
        # reach one repair node while the other's request is still open has to
        # give the two nodes distinguishable requests.
        return await self._dispatch(
            state, "repairer", "repairing",
            AttemptBudget("ci_repair_attempts", _BUDGET.ci_repair_attempts),
        )

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
        """Merges the pull request, and reports success only once the code host
        confirms the merge happened.

        `gh pr merge` returning without an error is not a merge. GitHub may
        have queued an auto-merge behind a branch-protection rule, another
        actor may have closed the pull request while the run waited for a
        human, and a merge can be blocked by a race the CLI reports as
        success. So the node re-reads the pull request afterwards and routes on
        what the code host says, not on what the merge command did:

        - merged -- the merge commit and merge timestamp go into the durable
          record and `route_after_merge` lets the graph reach `complete`, which
          is the only place the issue is closed and `agent:delivered` applied
          (`PROJECT.md`'s "agents do not decide completion").
        - closed without a merge -- terminal, and a distinct reason from a
          merge that failed, because nothing about it will get better by
          waiting.
        - anything else, including an unreadable code host -- unverified. The
          run stays in the re-enterable `merging` status; nothing is closed and
          nothing is labelled delivered.

        Re-entry is a no-op for a pull request that is already merged: the
        state is read before anything is attempted, so `gh pr merge` is never
        issued a second time for a merge that already landed (`AGENTS.md` §12,
        idempotent external side effects).
        """
        code_host = await self._resolve_code_host(state)
        pull_request_id = state.get("pull_request_id")
        if code_host is None or not pull_request_id:
            return await self._merge_unverified(state, "no code host reported on the pull request")
        try:
            pull_request = await code_host.get_pull_request(pull_request_id)
        except GitHubCliError as error:
            return await self._merge_unverified(state, f"pull request could not be read: {error}")
        if not pull_request.merged and not pull_request.closed:
            try:
                await code_host.merge_pull_request(
                    pull_request_id, state.get("merge_method", "squash")
                )
            except GitHubCliError as error:
                return await self._transition(state, "blocked", {
                    "status": "blocked",
                    "blocking_reason": f"merge failed: {error}",
                    **_pull_request_record(pull_request),
                })
            try:
                pull_request = await code_host.get_pull_request(pull_request_id)
            except GitHubCliError as error:
                # The merge may well have landed -- this only says the
                # confirming read failed. Blocking here would strand a merged
                # pull request on a transient GitHub hiccup, so the run waits
                # and re-reads instead. The pre-merge read is deliberately not
                # passed on: writing its `open` state back would assert
                # something this branch has just established it does not know,
                # and the durable record is better left as it was.
                return await self._merge_unverified(
                    state, f"merge could not be confirmed: {error}"
                )
        if pull_request.merged:
            return await self._transition(state, "merging", {
                "status": "merging",
                "pull_request_merged": True,
                **_pull_request_record(pull_request),
            })
        if pull_request.closed:
            return await self._transition(state, "blocked", {
                "status": "blocked",
                "blocking_reason": (
                    f"pull request {pull_request_id} was closed without being merged"
                ),
                **_pull_request_record(pull_request),
            })
        return await self._merge_unverified(
            state, f"the code host still reports it {pull_request.state}", pull_request
        )

    async def _merge_unverified(
        self,
        state: IssueWorkflowState,
        detail: str,
        pull_request: PullRequest | None = None,
    ) -> WorkflowUpdate:
        """Parks the run in `merging` while the merge stays unconfirmed, under a
        bound.

        `merging` is a waiting status, not a delivered one: `route_after_merge`
        sends it to END, so the run holds its place and re-entering the node
        re-reads the pull request. The counter is what stops that from being
        unbounded -- a pull request that never merges (a permanently queued
        auto-merge, a code host that cannot be read) would otherwise re-enter
        forever, which `PROJECT.md`'s "bounded loops" forbids. Once the budget
        is spent the run blocks with the last thing the code host actually
        said, which is the part a human needs.
        """
        attempts = _attempts(state, "merge_verification_attempts") + 1
        record = {} if pull_request is None else _pull_request_record(pull_request)
        if attempts >= _BUDGET.merge_verification_attempts:
            return await self._transition(state, "blocked", {
                "status": "blocked",
                "merge_verification_attempts": attempts,
                "blocking_reason": f"merge was not confirmed after {attempts} attempts: {detail}",
                **record,
            })
        return await self._transition(state, "merging", {
            "status": "merging",
            "merge_verification_attempts": attempts,
            "pull_request_merged": False,
            **record,
        })

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
        attempts: AttemptBudget | None,
    ) -> WorkflowUpdate:
        """Queues one execution for this phase, exactly once.

        Every delivery of a workflow transition may happen twice -- the outbox
        is at-least-once by design -- so a node may be entered again with an
        execution it already queued still in flight. The replay check therefore
        comes first, ahead of the retry-budget gates: a replay spends nothing,
        so re-testing a budget the first pass already charged would block a run
        for an exhaustion that never happened (the node that dispatched the
        last affordable execution reads its own increment back as "exhausted").
        """
        workflow_run_id = _workflow_run_id(state)
        open_request = await _await(self.persistence.get_open_execution_request(workflow_run_id))
        if open_request is not None:
            return await self._reuse(workflow_run_id, open_request, role, status)
        if attempts is not None and _attempts(state, attempts.counter) >= attempts.limit:
            return await self._budget_exhausted(state)
        # The exhaustion check applies to every role, including the ones that do
        # not spend the budget: once no agent run is affordable, the pipeline's
        # verdict has nowhere to route (both of its successors dispatch agents),
        # so validating first would only cost a runner execution to reach the
        # same blocked state.
        if int(state.get("total_agent_executions", 0)) >= _BUDGET.total_agent_executions:
            return await self._budget_exhausted(state)
        request = await _await(self.dispatcher.dispatch(workflow_run_id, role))
        if not request["created"]:
            # A concurrent delivery of the same transition won the race and
            # queued the execution between the check above and this call. The
            # dispatcher decided that under the run's row lock, so exactly one
            # of the two callers is here and neither inserted a second row.
            return await self._reuse(workflow_run_id, request, role, status)
        # `awaiting_execution` suspends the graph on the outgoing edge: the
        # gates the downstream nodes read only exist once this execution
        # reports a terminal event.
        updates: WorkflowUpdate = {
            "status": status,
            "execution_id": request["id"],
            "awaiting_execution": True,
        }
        if attempts is not None:
            updates[attempts.counter] = _attempts(state, attempts.counter) + 1
        if role not in _NON_AGENT_ROLES:
            updates["total_agent_executions"] = int(state.get("total_agent_executions", 0)) + 1
        await _await(self.persistence.transition(workflow_run_id, status, updates))
        return updates

    async def _reuse(
        self, workflow_run_id: str, request: dict[str, Any], role: str, status: str
    ) -> WorkflowUpdate:
        """Adopts an execution request this run already has open.

        No counter moves: the attempt was charged when the request row was
        inserted, and charging it again is what turned a replayed transition
        into a workflow blocked "for exhausted retries" that never ran.

        When the open request is this node's own role, the phase is re-stated
        so a first pass that queued the execution and then died before writing
        the transition still lands the status it was moving to.

        A role mismatch is not a normal outcome. It means the graph walked onto
        a node whose phase never started while another phase's execution is
        still running, which the runtime's own replay gate is supposed to make
        unreachable. Suspending is the least destructive response -- the
        committed phase belongs to the execution that is actually running, and
        that execution's terminal event will resume the graph -- but it leaves
        the run parked on an edge it never earned, so it is logged rather than
        swallowed. It is not raised: the caller is the outbox, and an exception
        here would redeliver the same transition into the same state until the
        lease gave up.
        """
        reused: WorkflowUpdate = {"execution_id": request["id"], "awaiting_execution": True}
        if str(request["role"]) == role:
            reused["status"] = status
            await _await(self.persistence.transition(workflow_run_id, status, reused))
            return reused
        _LOGGER.error(
            "workflow node reached while another phase's execution is still open",
            extra={
                "workflow_run_id": workflow_run_id,
                "node_role": role,
                "node_status": status,
                "open_request_id": str(request["id"]),
                "open_request_role": str(request["role"]),
            },
        )
        return reused

    async def _transition(
        self, state: IssueWorkflowState, status: str, updates: WorkflowUpdate
    ) -> WorkflowUpdate:
        await _await(self.persistence.transition(_workflow_run_id(state), status, updates))
        return updates


def _pull_request_record(pull_request: PullRequest) -> WorkflowUpdate:
    """The pull request as the durable record should hold it.

    Every field the `app.pull_requests` upsert writes travels together, so a
    transition can never refresh one half of the row and blank the other: the
    upsert reads `pull_request_id` as its trigger and would otherwise overwrite
    `head_commit` and `state` with empty strings. Absent timestamps and commits
    are empty strings rather than None so the graph state stays JSON-plain; the
    persistence layer is what turns them back into SQL NULLs.
    """
    return {
        "pull_request_id": pull_request.external_id,
        "pull_request_url": pull_request.url,
        "pull_request_head_commit": pull_request.head_commit,
        "pull_request_state": pull_request.state,
        "pull_request_merged_at": pull_request.merged_at or "",
        "pull_request_merge_commit": pull_request.merge_commit or "",
    }


def _attempts(state: IssueWorkflowState, counter: str | None) -> int:
    if counter is None:
        return 0
    value = state.get(counter, 0)
    return value if isinstance(value, int) else 0


def _workflow_run_id(state: IssueWorkflowState) -> str:
    workflow_run_id = state.get("workflow_run_id")
    if not isinstance(workflow_run_id, str) or not workflow_run_id:
        raise ValueError("workflow run ID is required")
    return workflow_run_id
