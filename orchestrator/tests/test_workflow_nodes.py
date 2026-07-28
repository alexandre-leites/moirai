import unittest
from dataclasses import dataclass
from typing import Any, cast

from moirai.code_hosts import CheckStatus, GitHubCliError, PullRequest, PullRequestCheck
from moirai.workflows.issue_graph import IssueWorkflowState
from moirai.workflows.nodes import PersistedWorkflowNodes


class _Persistence:
    def __init__(self) -> None:
        self.transitions: list[tuple[str, str, dict[str, object]]] = []

    async def transition(self, workflow_run_id: str, status: str, updates: dict[str, object]) -> None:
        self.transitions.append((workflow_run_id, status, updates))

    async def get_queued_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None:
        return None


class _Dispatcher:
    def __init__(self) -> None:
        self.dispatches: list[tuple[str, str]] = []

    async def dispatch(self, workflow_run_id: str, role: str) -> str:
        self.dispatches.append((workflow_run_id, role))
        return f"{workflow_run_id}-{role}"


@dataclass
class _FakeCodeHost:
    created_prs: list[tuple[str, str, str, str, str | None]] = None
    checked_prs: list[str] = None
    merged_prs: list[tuple[str, str]] = None
    _checks_result: list[PullRequestCheck] = None
    merge_error: GitHubCliError | None = None
    pull_request_state: str = "open"

    def __post_init__(self) -> None:
        if self.created_prs is None:
            self.created_prs = []
        if self.checked_prs is None:
            self.checked_prs = []
        if self.merged_prs is None:
            self.merged_prs = []

    async def create_or_find_pull_request(
        self, workflow_id: str, branch: str, base_branch: str, title: str, issue_number: str | None = None
    ) -> PullRequest:
        self.created_prs.append((workflow_id, branch, base_branch, title, issue_number))
        return PullRequest(external_id="42", url="https://github.com/org/repo/pull/42", state="open", head_branch=branch, head_commit="abc123")

    async def required_checks(self, pull_request_id: str) -> list[PullRequestCheck]:
        self.checked_prs.append(pull_request_id)
        if self._checks_result is not None:
            return self._checks_result
        return [PullRequestCheck(name="test", status=CheckStatus.PASSING)]

    async def get_pull_request(self, pull_request_id: str) -> PullRequest:
        return PullRequest(external_id=pull_request_id, url="https://github.com/org/repo/pull/42", state=self.pull_request_state, head_branch="agent/42/fix", head_commit="abc123")

    async def enable_auto_merge(self, pull_request_id: str, method: str) -> None:
        pass

    async def merge_pull_request(self, pull_request_id: str, method: str) -> None:
        if self.merge_error is not None:
            raise self.merge_error
        self.merged_prs.append((pull_request_id, method))


@dataclass
class _FakeIssueTracker:
    closed_issues: list[str] = None
    added_labels: list[tuple[str, list[str]]] = None

    def __post_init__(self) -> None:
        if self.closed_issues is None:
            self.closed_issues = []
        if self.added_labels is None:
            self.added_labels = []

    async def close_issue(self, external_issue_id: str) -> None:
        self.closed_issues.append(external_issue_id)

    async def add_labels(self, external_issue_id: str, labels: list[str]) -> None:
        self.added_labels.append((external_issue_id, labels))


class PersistedWorkflowNodesTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self.persistence = _Persistence()
        self.dispatcher = _Dispatcher()
        self.nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher)
        self.state: IssueWorkflowState = {"workflow_run_id": "workflow-1"}

    async def test_prepare_persists_planning_phase(self) -> None:
        update = await self.nodes.prepare(self.state)
        self.assertEqual(update, {"status": "planning"})
        self.assertEqual(self.persistence.transitions, [("workflow-1", "planning", update)])

    async def test_dispatch_nodes_increment_the_matching_attempt_and_total_budget(self) -> None:
        cases = [
            (self.nodes.plan, "planner", "planning", "planning_attempts"),
            (self.nodes.implement, "developer", "implementing", "implementation_attempts"),
            (self.nodes.review, "reviewer", "ai_review", "review_cycles"),
            (self.nodes.repair, "repairer", "repairing", "pipeline_repair_attempts"),
        ]
        for node, role, status, counter in cases:
            update = await node(cast(IssueWorkflowState, {"workflow_run_id": "workflow-1", counter: 1, "total_agent_executions": 4}))
            self.assertEqual(update["status"], status)
            self.assertEqual(update[counter], 2)
            self.assertEqual(update["total_agent_executions"], 5)
            self.assertEqual(update["execution_id"], f"workflow-1-{role}")
        self.assertEqual([role for _, role in self.dispatcher.dispatches], ["planner", "developer", "reviewer", "repairer"])

    async def test_pipeline_dispatches_a_dedicated_pipeline_execution(self) -> None:
        update = await self.nodes.pipeline(self.state)
        self.assertEqual(update["status"], "local_pipeline")
        self.assertEqual(update["execution_id"], "workflow-1-pipeline")
        self.assertEqual(self.dispatcher.dispatches, [("workflow-1", "pipeline")])

    async def test_push_and_blocked_persist_deterministic_terminal_states(self) -> None:
        push = await self.nodes.push(self.state)
        blocked = await self.nodes.blocked({"workflow_run_id": "workflow-1"})
        self.assertEqual(push["status"], "pushing")
        self.assertEqual(push["execution_id"], "workflow-1-developer")
        self.assertNotIn("ci_repair_attempts", push)
        self.assertEqual(push["total_agent_executions"], 1)
        self.assertEqual(blocked, {"status": "blocked", "blocking_reason": "workflow retry budget exhausted"})

    async def test_pr_checks_human_merge_and_completion_fallback_without_adapters(self) -> None:
        for node, status, extra_fields in (
            (self.nodes.create_pull_request, "pr_created", {}),
            (self.nodes.wait_for_checks, "waiting_github_checks", {}),
            (self.nodes.wait_for_human, "waiting_human", {"human_approved": False}),
            (self.nodes.merge, "merging", {}),
            (self.nodes.complete, "completed", {}),
        ):
            expected = {"status": status, **extra_fields}
            result = await node(self.state)
            self.assertEqual(result, expected)
            self.assertEqual(result["status"], status)
        self.assertEqual([status for _, status, _ in self.persistence.transitions[-5:]], [
            "pr_created", "waiting_github_checks", "waiting_human", "merging", "completed",
        ])

    async def test_nodes_reject_missing_workflow_id_before_side_effects(self) -> None:
        with self.assertRaisesRegex(ValueError, "workflow run ID"):
            await self.nodes.plan({})
        self.assertEqual(self.dispatcher.dispatches, [])
        self.assertEqual(self.persistence.transitions, [])

    async def test_create_pull_request_calls_code_host_and_stores_pr_details(self) -> None:
        code_host = _FakeCodeHost()
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {
            "workflow_run_id": "wf-1", "branch_name": "agent/42/fix", "base_branch": "main", "issue_id": "42"
        }
        update = await nodes.create_pull_request(state)
        self.assertEqual(update["status"], "pr_created")
        self.assertEqual(update["pull_request_id"], "42")
        self.assertEqual(update["pull_request_url"], "https://github.com/org/repo/pull/42")
        self.assertEqual(len(code_host.created_prs), 1)
        self.assertEqual(code_host.created_prs[0][1], "agent/42/fix")

    async def test_create_pull_request_without_code_host_uses_fallback(self) -> None:
        update = await self.nodes.create_pull_request(self.state)
        self.assertEqual(update, {"status": "pr_created"})

    async def test_wait_for_checks_polls_code_host_and_sets_checks_passed(self) -> None:
        code_host = _FakeCodeHost()
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42"}
        update = await nodes.wait_for_checks(state)
        self.assertEqual(update["status"], "waiting_github_checks")
        self.assertTrue(update.get("checks_passed"))
        self.assertEqual(code_host.checked_prs, ["42"])

    async def test_wait_for_checks_marks_pending_checks_without_consuming_repair_budget(self) -> None:
        code_host = _FakeCodeHost(_checks_result=[PullRequestCheck(name="test", status=CheckStatus.PENDING)])
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        update = await nodes.wait_for_checks({"workflow_run_id": "wf-1", "pull_request_id": "42"})
        self.assertTrue(update["checks_pending"])
        self.assertFalse(update["checks_passed"])
        self.assertNotIn("ci_repair_attempts", update)

    async def test_wait_for_checks_reports_failing_checks(self) -> None:
        code_host = _FakeCodeHost(_checks_result=[
            PullRequestCheck(name="test", status=CheckStatus.FAILING),
        ])
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42"}
        update = await nodes.wait_for_checks(state)
        self.assertFalse(update.get("checks_passed"))

    async def test_wait_for_checks_without_pull_request_id_uses_fallback(self) -> None:
        update = await self.nodes.wait_for_checks(self.state)
        self.assertEqual(update, {"status": "waiting_github_checks"})

    async def test_wait_for_checks_treats_empty_check_list_as_not_passed(self) -> None:
        code_host = _FakeCodeHost(_checks_result=[])
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42"}
        update = await nodes.wait_for_checks(state)
        self.assertFalse(update.get("checks_passed"))

    async def test_wait_for_checks_treats_skipped_required_check_as_not_passed(self) -> None:
        code_host = _FakeCodeHost(_checks_result=[
            PullRequestCheck(name="test", status=CheckStatus.SKIPPED, required=True),
        ])
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42"}
        update = await nodes.wait_for_checks(state)
        self.assertFalse(update.get("checks_passed"))

    async def test_wait_for_checks_treats_skipped_optional_check_as_passed(self) -> None:
        code_host = _FakeCodeHost(_checks_result=[
            PullRequestCheck(name="test", status=CheckStatus.SKIPPED, required=False),
        ])
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42"}
        update = await nodes.wait_for_checks(state)
        self.assertTrue(update.get("checks_passed"))

    async def test_merge_calls_code_host_and_transitions(self) -> None:
        code_host = _FakeCodeHost()
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42", "merge_method": "squash"}
        update = await nodes.merge(state)
        self.assertEqual(update["status"], "merging")
        self.assertEqual(code_host.merged_prs, [("42", "squash")])

    async def test_merge_treats_an_already_merged_pull_request_as_success(self) -> None:
        code_host = _FakeCodeHost(pull_request_state="MERGED")
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        update = await nodes.merge({"workflow_run_id": "wf-1", "pull_request_id": "42"})
        self.assertEqual(update, {"status": "merging"})
        self.assertEqual(code_host.merged_prs, [])

    async def test_merge_without_code_host_uses_fallback(self) -> None:
        state: IssueWorkflowState = {"workflow_run_id": "wf-1"}
        update = await self.nodes.merge(state)
        self.assertEqual(update, {"status": "merging"})

    async def test_merge_transitions_to_blocked_when_code_host_refuses(self) -> None:
        code_host = _FakeCodeHost(merge_error=GitHubCliError("refusing to merge pull request"))
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42", "merge_method": "squash"}
        update = await nodes.merge(state)
        self.assertEqual(update["status"], "blocked")
        self.assertIn("refusing to merge", str(update["blocking_reason"]))
        self.assertEqual(code_host.merged_prs, [])

    async def test_complete_closes_issue_and_adds_delivered_label(self) -> None:
        issue_tracker = _FakeIssueTracker()
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, issue_tracker_factory=lambda project_id: issue_tracker)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "issue_id": "42"}
        update = await nodes.complete(state)
        self.assertEqual(update["status"], "completed")
        self.assertEqual(issue_tracker.closed_issues, ["42"])
        self.assertEqual(issue_tracker.added_labels, [("42", ["agent:delivered"])])

    async def test_complete_without_issue_tracker_uses_fallback(self) -> None:
        update = await self.nodes.complete(self.state)
        self.assertEqual(update, {"status": "completed"})


if __name__ == "__main__":
    unittest.main()
