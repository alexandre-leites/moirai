from __future__ import annotations

from collections.abc import Awaitable, Callable
from dataclasses import dataclass
from typing import Any, Protocol

from moirai.code_hosts import CodeHost
from moirai.issue_trackers import IssueTracker

from .issue_graph import IssueWorkflowNodes, IssueWorkflowState, WorkflowUpdate


CodeHostFactory = Callable[[str], "CodeHost | None | Awaitable[CodeHost | None]"]
IssueTrackerFactory = Callable[[str], "IssueTracker | None | Awaitable[IssueTracker | None]"]

_MAX_PLANNING_ATTEMPTS = 2
_MAX_IMPLEMENTATION_ATTEMPTS = 3
_MAX_PIPELINE_REPAIR_ATTEMPTS = 3
_MAX_REVIEW_CYCLES = 3
_MAX_TOTAL_AGENT_EXECUTIONS = 10


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
            return await self._transition(state, "planning", {"status": "planning", "plan_valid": True})
        if int(state.get("planning_attempts", 0)) >= _MAX_PLANNING_ATTEMPTS:
            return await self._transition(state, "blocked", {"status": "blocked", "blocking_reason": "workflow retry budget exhausted"})
        return await self._dispatch(state, "planner", "planning", "planning_attempts")

    async def implement(self, state: IssueWorkflowState) -> WorkflowUpdate:
        if int(state.get("implementation_attempts", 0)) >= _MAX_IMPLEMENTATION_ATTEMPTS:
            return await self._transition(state, "blocked", {"status": "blocked", "blocking_reason": "workflow retry budget exhausted"})
        return await self._dispatch(state, "developer", "implementing", "implementation_attempts")

    async def pipeline(self, state: IssueWorkflowState) -> WorkflowUpdate:
        return await self._transition(state, "local_pipeline", {"status": "local_pipeline"})

    async def review(self, state: IssueWorkflowState) -> WorkflowUpdate:
        if int(state.get("review_cycles", 0)) >= _MAX_REVIEW_CYCLES:
            return await self._transition(state, "blocked", {"status": "blocked", "blocking_reason": "workflow retry budget exhausted"})
        return await self._dispatch(state, "reviewer", "ai_review", "review_cycles")

    async def repair(self, state: IssueWorkflowState) -> WorkflowUpdate:
        if int(state.get("pipeline_repair_attempts", 0)) >= _MAX_PIPELINE_REPAIR_ATTEMPTS:
            return await self._transition(state, "blocked", {"status": "blocked", "blocking_reason": "workflow retry budget exhausted"})
        return await self._dispatch(state, "repairer", "repairing", "pipeline_repair_attempts")

    async def push(self, state: IssueWorkflowState) -> WorkflowUpdate:
        if int(state.get("ci_repair_attempts", 0)) >= _MAX_PIPELINE_REPAIR_ATTEMPTS:
            return await self._transition(state, "blocked", {"status": "blocked", "blocking_reason": "workflow retry budget exhausted"})
        return await self._dispatch(state, "developer", "pushing", "ci_repair_attempts")

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
        })

    async def wait_for_checks(self, state: IssueWorkflowState) -> WorkflowUpdate:
        code_host = await self._resolve_code_host(state)
        if code_host is None or not state.get("pull_request_id"):
            return await self._transition(state, "waiting_github_checks", {"status": "waiting_github_checks"})
        checks = await code_host.required_checks(state["pull_request_id"])
        all_passing = all(
            check.status.value in {"passing", "skipped"}
            for check in checks
        )
        return await self._transition(state, "waiting_github_checks", {
            "status": "waiting_github_checks",
            "checks_passed": all_passing,
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
            await code_host.merge_pull_request(state["pull_request_id"], method)
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

    async def _dispatch(
        self,
        state: IssueWorkflowState,
        role: str,
        status: str,
        attempt_counter: str | None,
    ) -> WorkflowUpdate:
        workflow_run_id = _workflow_run_id(state)
        if int(state.get("total_agent_executions", 0)) >= _MAX_TOTAL_AGENT_EXECUTIONS:
            updates: WorkflowUpdate = {"status": "blocked", "blocking_reason": "workflow retry budget exhausted"}
            await _await(self.persistence.transition(workflow_run_id, "blocked", updates))
            return updates
        existing = await _await(self.persistence.get_queued_execution_request(workflow_run_id))
        if existing is not None and existing["role"] == role:
            execution_id = existing["id"]
        else:
            execution_id = await _await(self.dispatcher.dispatch(workflow_run_id, role))
        updates: WorkflowUpdate = {"status": status, "execution_id": execution_id}
        if attempt_counter is not None:
            updates[attempt_counter] = int(state.get(attempt_counter, 0)) + 1
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
