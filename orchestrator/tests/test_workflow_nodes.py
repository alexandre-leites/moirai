import unittest
from dataclasses import dataclass
from typing import Any, cast

from moirai.code_hosts import CheckStatus, GitHubCliError, PullRequest, PullRequestCheck
from moirai.workflows.issue_graph import IssueWorkflowState, route_merge
from moirai.workflows.nodes import PersistedWorkflowNodes
from moirai.workflows.policy import RetryBudget


def _request(identifier: str, role: str, created: bool = False) -> dict[str, Any]:
    return {"id": identifier, "role": role, "attempt": 1, "created": created}


class _Persistence:
    def __init__(self, open_request: dict[str, Any] | None = None) -> None:
        self.transitions: list[tuple[str, str, dict[str, object]]] = []
        self.open_request = open_request

    async def transition(self, workflow_run_id: str, status: str, updates: dict[str, object]) -> None:
        self.transitions.append((workflow_run_id, status, updates))

    async def get_open_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None:
        return self.open_request


class _Dispatcher:
    def __init__(self, lost_race_to: dict[str, Any] | None = None) -> None:
        self.dispatches: list[tuple[str, str]] = []
        self.lost_race_to = lost_race_to

    async def dispatch(self, workflow_run_id: str, role: str) -> dict[str, Any]:
        self.dispatches.append((workflow_run_id, role))
        if self.lost_race_to is not None:
            return self.lost_race_to
        return _request(f"{workflow_run_id}-{role}", role, created=True)


@dataclass
class _FakeCodeHost:
    created_prs: list[tuple[str, str, str, str, str | None]] = None
    checked_prs: list[str] = None
    merged_prs: list[tuple[str, str]] = None
    _checks_result: list[PullRequestCheck] = None
    merge_error: GitHubCliError | None = None
    pull_request_state: str = "open"
    # The state the pull request is left in by a successful merge. None models
    # the case the merge node exists for: `gh pr merge` returns cleanly and the
    # pull request is still not merged (a queued auto-merge, a protection race).
    merge_result_state: str | None = "merged"
    merged_at: str = "2026-01-01T00:00:00+00:00"
    merge_commit: str = "def456"
    # 1-based index of the get_pull_request call that fails, modelling a GitHub
    # hiccup at a chosen point in the read/merge/re-read sequence.
    read_error_on_call: int | None = None
    reads: int = 0

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
        self.reads += 1
        if self.read_error_on_call == self.reads:
            raise GitHubCliError("gh: could not read pull request")
        merged = self.pull_request_state == "merged"
        return PullRequest(
            external_id=pull_request_id,
            url="https://github.com/org/repo/pull/42",
            state=self.pull_request_state,
            head_branch="agent/42/fix",
            head_commit="abc123",
            merged_at=self.merged_at if merged else None,
            merge_commit=self.merge_commit if merged else None,
        )

    async def enable_auto_merge(self, pull_request_id: str, method: str) -> None:
        pass

    async def merge_pull_request(self, pull_request_id: str, method: str) -> None:
        if self.merge_error is not None:
            raise self.merge_error
        self.merged_prs.append((pull_request_id, method))
        if self.merge_result_state is not None:
            self.pull_request_state = self.merge_result_state


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
            (self.nodes.ci_repair, "repairer", "repairing", "ci_repair_attempts"),
        ]
        for node, role, status, counter in cases:
            update = await node(cast(IssueWorkflowState, {"workflow_run_id": "workflow-1", counter: 1, "total_agent_executions": 4}))
            self.assertEqual(update["status"], status)
            self.assertEqual(update[counter], 2)
            self.assertEqual(update["total_agent_executions"], 5)
            self.assertEqual(update["execution_id"], f"workflow-1-{role}")
        self.assertEqual(
            [role for _, role in self.dispatcher.dispatches],
            ["planner", "developer", "reviewer", "repairer", "repairer"],
        )

    async def test_each_repair_node_spends_only_its_own_budget(self) -> None:
        """`ci_repair_attempts` has exactly one writer, the `ci_repair` node,
        and a CI repair leaves the local pipeline's repair budget untouched."""
        ci = await self.nodes.ci_repair({"workflow_run_id": "workflow-1", "pipeline_repair_attempts": 2})
        local = await self.nodes.repair({"workflow_run_id": "workflow-1", "ci_repair_attempts": 2})

        self.assertEqual(ci["ci_repair_attempts"], 1)
        self.assertNotIn("pipeline_repair_attempts", ci)
        self.assertEqual(local["pipeline_repair_attempts"], 1)
        self.assertNotIn("ci_repair_attempts", local)
        # A CI repair is an agent run like any other repair: it dispatches the
        # repairer role, so it spends the global agent budget too.
        self.assertEqual(ci["total_agent_executions"], 1)
        self.assertEqual([role for _, role in self.dispatcher.dispatches], ["repairer", "repairer"])

    async def test_each_repair_node_blocks_on_its_own_exhausted_budget_only(self) -> None:
        """The node-level guard mirrors the routing gate: an exhausted CI budget
        stops CI repairs and nothing else, and vice versa.

        `ci_repair`'s own guard is defensive rather than load-bearing --
        `route_after_checks` gates on the same counter with the same limit, so
        the graph blocks on the edge before reaching the node. `repair`'s guard
        is genuinely reachable, because `route_after_human_response` routes to a
        repair without consulting any counter. Both are pinned here so a future
        routing change cannot make a repair node dispatch past its budget."""
        ci_exhausted = await self.nodes.ci_repair({"workflow_run_id": "workflow-1", "ci_repair_attempts": 3})
        self.assertEqual(ci_exhausted["status"], "blocked")
        self.assertEqual(ci_exhausted["blocking_reason"], "workflow retry budget exhausted")
        self.assertFalse(ci_exhausted["awaiting_execution"])
        self.assertEqual(self.dispatcher.dispatches, [])

        # The other counter being drained must not stop either node.
        ci_free = await self.nodes.ci_repair({"workflow_run_id": "workflow-1", "pipeline_repair_attempts": 3})
        local_free = await self.nodes.repair({"workflow_run_id": "workflow-1", "ci_repair_attempts": 3})
        self.assertEqual(ci_free["status"], "repairing")
        self.assertEqual(local_free["status"], "repairing")
        self.assertEqual([role for _, role in self.dispatcher.dispatches], ["repairer", "repairer"])

    async def test_pipeline_dispatches_a_dedicated_pipeline_execution(self) -> None:
        update = await self.nodes.pipeline(self.state)
        self.assertEqual(update["status"], "local_pipeline")
        self.assertEqual(update["execution_id"], "workflow-1-pipeline")
        self.assertEqual(self.dispatcher.dispatches, [("workflow-1", "pipeline")])

    async def test_pipeline_execution_does_not_spend_the_agent_budget(self) -> None:
        """The pipeline runs the project's commands, not an agent, so it must
        not consume `total_agent_executions`. Counting it would make the now
        mandatory pipeline halve the agent attempts a workflow gets."""
        update = await self.nodes.pipeline(
            cast(IssueWorkflowState, {"workflow_run_id": "workflow-1", "total_agent_executions": 4})
        )
        self.assertEqual(self.dispatcher.dispatches, [("workflow-1", "pipeline")])
        self.assertNotIn("total_agent_executions", update)
        self.assertNotIn("total_agent_executions", self.persistence.transitions[-1][2])

    async def test_pipeline_still_blocks_once_no_agent_run_is_affordable(self) -> None:
        """Not spending the budget is not the same as ignoring it: both of the
        pipeline's successors dispatch agents, so an exhausted budget blocks
        here rather than paying for a verdict with nowhere to route."""
        update = await self.nodes.pipeline(
            cast(IssueWorkflowState, {"workflow_run_id": "workflow-1", "total_agent_executions": 10})
        )
        self.assertEqual(update["status"], "blocked")
        self.assertEqual(update["blocking_reason"], "workflow retry budget exhausted")
        self.assertEqual(self.dispatcher.dispatches, [])

    async def test_pipeline_dispatches_even_when_the_gate_is_already_set(self) -> None:
        """The local pipeline is the deterministic completion gate, so entering
        the phase always runs it. An inherited `pipeline_passed` -- from an
        earlier pipeline run, or from any future caller that seeds the gate --
        must never skip the execution, and the node must never write the gate."""
        update = await self.nodes.pipeline({"workflow_run_id": "workflow-1", "pipeline_passed": True})
        self.assertEqual(self.dispatcher.dispatches, [("workflow-1", "pipeline")])
        self.assertTrue(update["awaiting_execution"])
        self.assertNotIn("pipeline_passed", update)
        self.assertNotIn("pipeline_passed", self.persistence.transitions[-1][2])

    async def test_dispatching_marks_the_workflow_as_awaiting_the_execution(self) -> None:
        """The gate the graph reads to end the invocation after a dispatch."""
        for node in (self.nodes.plan, self.nodes.implement, self.nodes.pipeline, self.nodes.review,
                     self.nodes.repair, self.nodes.ci_repair, self.nodes.push):
            update = await node({"workflow_run_id": "workflow-1"})
            self.assertTrue(update["awaiting_execution"], node)

    async def test_short_circuiting_nodes_clear_the_awaiting_gate(self) -> None:
        plan = await self.nodes.plan({"workflow_run_id": "workflow-1", "plan_valid": True})
        exhausted = await self.nodes.review({"workflow_run_id": "workflow-1", "review_cycles": 3})
        self.assertFalse(plan["awaiting_execution"])
        self.assertFalse(exhausted["awaiting_execution"])
        self.assertEqual(exhausted["status"], "blocked")
        self.assertEqual(self.dispatcher.dispatches, [])

    async def test_replayed_node_reuses_its_open_request_without_respending_budget(self) -> None:
        """A node re-entered while its own request is still open must not
        duplicate the agent run nor count a second attempt against the budget.

        `dispatched` counts as open: the scheduler claims a request the moment
        it offers the work, and before issue #96 a replay that landed after
        that claim queued a second request for the same role -- the same agent
        work offered twice."""
        for status in ("queued", "dispatched"):
            with self.subTest(request_status=status):
                dispatcher = _Dispatcher()
                persistence = _Persistence(open_request=_request("open-1", "developer"))
                nodes = PersistedWorkflowNodes(persistence, dispatcher)
                state: IssueWorkflowState = {
                    "workflow_run_id": "workflow-1", "implementation_attempts": 1,
                    "total_agent_executions": 2,
                }

                update = await nodes.implement(state)

                self.assertEqual(update["execution_id"], "open-1")
                self.assertEqual(update["status"], "implementing")
                self.assertTrue(update["awaiting_execution"])
                self.assertNotIn("implementation_attempts", update)
                self.assertNotIn("total_agent_executions", update)
                self.assertEqual(dispatcher.dispatches, [])
                self.assertEqual(persistence.transitions[-1][1], "implementing")

    async def test_a_replay_never_blocks_a_run_on_the_budget_it_already_spent(self) -> None:
        """The dispatch that spends the last unit of a budget writes the counter
        that makes the same node read "exhausted" on the way back in. A replay
        must therefore be recognised before any budget gate, or the run is
        blocked for retries that never happened -- the whole point of issue #96.
        """
        budget = RetryBudget()
        for node_name, role, counter in (
            ("plan", "planner", "planning_attempts"),
            ("implement", "developer", "implementation_attempts"),
            ("review", "reviewer", "review_cycles"),
            ("repair", "repairer", "pipeline_repair_attempts"),
            ("ci_repair", "repairer", "ci_repair_attempts"),
        ):
            with self.subTest(node=node_name):
                dispatcher = _Dispatcher()
                persistence = _Persistence(open_request=_request("open-1", role))
                nodes = PersistedWorkflowNodes(persistence, dispatcher)

                # Both budgets read exactly as this node's own last dispatch
                # left them: its counter at its limit and the global agent
                # budget fully spent.
                update = await getattr(nodes, node_name)(
                    cast(IssueWorkflowState, {
                        "workflow_run_id": "workflow-1",
                        counter: getattr(budget, counter),
                        "total_agent_executions": budget.total_agent_executions,
                    })
                )

                self.assertNotEqual(update["status"], "blocked")
                self.assertEqual(update["execution_id"], "open-1")
                self.assertEqual(dispatcher.dispatches, [])
                self.assertNotIn(counter, update)
                self.assertNotIn("total_agent_executions", update)

    async def test_an_open_request_for_another_role_suspends_instead_of_dispatching(self) -> None:
        """One workflow run has at most one execution in flight. A node reached
        while another phase's execution is still open can only be a replay that
        walked the graph forward, so it queues nothing, spends nothing, and
        leaves the committed phase alone -- the run belongs to the execution
        that is actually running."""
        persistence = _Persistence(open_request=_request("open-1", "planner"))
        nodes = PersistedWorkflowNodes(persistence, self.dispatcher)

        update = await nodes.implement({"workflow_run_id": "workflow-1"})

        self.assertEqual(update, {"execution_id": "open-1", "awaiting_execution": True})
        self.assertEqual(self.dispatcher.dispatches, [])
        self.assertEqual(persistence.transitions, [])

    async def test_a_dispatch_that_loses_the_insert_race_spends_no_budget(self) -> None:
        """Two deliveries of one transition can both find no open request. The
        dispatcher settles it under the workflow run's row lock and tells the
        loser it reused a row rather than creating one; the loser must not
        charge an attempt for an execution it did not queue."""
        dispatcher = _Dispatcher(lost_race_to=_request("winner-1", "developer", created=False))
        nodes = PersistedWorkflowNodes(self.persistence, dispatcher)

        update = await nodes.implement({"workflow_run_id": "workflow-1"})

        self.assertEqual(update["execution_id"], "winner-1")
        self.assertNotIn("implementation_attempts", update)
        self.assertNotIn("total_agent_executions", update)
        self.assertEqual(dispatcher.dispatches, [("workflow-1", "developer")])

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
            # Merge is the exception: with no code host there is nothing to
            # confirm the merge against, so it waits rather than reporting a
            # merge it never saw.
            (self.nodes.merge, "merging", {"merge_verification_attempts": 1, "pull_request_merged": False}),
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

    async def test_merge_records_the_verified_merge_before_the_graph_may_complete(self) -> None:
        """The merge is confirmed by a re-read, and the merge commit and
        timestamp reach the durable record before `complete` is reachable."""
        code_host = _FakeCodeHost()
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42", "merge_method": "squash"}

        update = await nodes.merge(state)

        self.assertEqual(code_host.merged_prs, [("42", "squash")])
        self.assertIs(update["pull_request_merged"], True)
        self.assertEqual(update["pull_request_state"], "merged")
        self.assertEqual(update["pull_request_merged_at"], "2026-01-01T00:00:00+00:00")
        self.assertEqual(update["pull_request_merge_commit"], "def456")
        self.assertEqual(update["pull_request_head_commit"], "abc123")
        # The read/merge/re-read sequence, not a single optimistic read.
        self.assertEqual(code_host.reads, 2)
        self.assertEqual(self.persistence.transitions[-1][2], update)
        self.assertEqual(route_merge(cast(IssueWorkflowState, {**state, **update})), "complete")

    async def test_merge_does_not_reissue_a_merge_for_an_already_merged_pull_request(self) -> None:
        """Re-entering the node for a merged pull request must not run
        `gh pr merge` again -- the transition is at-least-once, so the node is
        entered more than once as a matter of course."""
        code_host = _FakeCodeHost()
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42", "merge_method": "squash"}

        first = await nodes.merge(state)
        second = await nodes.merge(cast(IssueWorkflowState, {**state, **first}))

        self.assertEqual(code_host.merged_prs, [("42", "squash")])
        self.assertIs(second["pull_request_merged"], True)
        self.assertEqual(second["pull_request_merge_commit"], "def456")
        self.assertNotIn("merge_verification_attempts", second)
        self.assertEqual(route_merge(cast(IssueWorkflowState, {**state, **second})), "complete")

    async def test_merge_never_merges_a_pull_request_that_is_already_merged(self) -> None:
        """The same guarantee on a first entry: a pull request merged by
        someone else while the run waited is confirmed, not re-merged."""
        code_host = _FakeCodeHost(pull_request_state="merged")
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)

        update = await nodes.merge({"workflow_run_id": "wf-1", "pull_request_id": "42"})

        self.assertEqual(code_host.merged_prs, [])
        self.assertEqual(code_host.reads, 1)
        self.assertIs(update["pull_request_merged"], True)

    async def test_merge_that_is_not_confirmed_delivers_nothing_and_stays_re_enterable(self) -> None:
        """`gh pr merge` returning cleanly is not a merge: an unconfirmed merge
        must not close the issue or label it delivered."""
        code_host = _FakeCodeHost(merge_result_state=None)
        issue_tracker = _FakeIssueTracker()
        nodes = PersistedWorkflowNodes(
            self.persistence,
            self.dispatcher,
            code_host_factory=lambda project_id: code_host,
            issue_tracker_factory=lambda project_id: issue_tracker,
        )
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "issue_id": "42", "pull_request_id": "42"}

        update = await nodes.merge(state)

        self.assertEqual(update["status"], "merging")
        self.assertIs(update["pull_request_merged"], False)
        self.assertEqual(update["merge_verification_attempts"], 1)
        self.assertEqual(update["pull_request_state"], "open")
        self.assertEqual(update["pull_request_merged_at"], "")
        # The edge maps "merge" to END: the run parks, re-enterable.
        self.assertEqual(route_merge(cast(IssueWorkflowState, {**state, **update})), "merge")
        self.assertEqual(issue_tracker.closed_issues, [])
        self.assertEqual(issue_tracker.added_labels, [])

    async def test_merge_blocks_a_pull_request_closed_without_being_merged(self) -> None:
        code_host = _FakeCodeHost(pull_request_state="closed")
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42"}

        update = await nodes.merge(state)

        self.assertEqual(update["status"], "blocked")
        self.assertEqual(update["blocking_reason"], "pull request 42 was closed without being merged")
        self.assertEqual(code_host.merged_prs, [])
        self.assertEqual(update["pull_request_state"], "closed")
        self.assertEqual(route_merge(cast(IssueWorkflowState, {**state, **update})), "blocked")

    async def test_merge_waits_rather_than_blocking_when_the_confirming_read_fails(self) -> None:
        """A GitHub hiccup on the read that follows the merge says nothing
        about whether the merge landed. Blocking there would strand a pull
        request that really did merge."""
        code_host = _FakeCodeHost(read_error_on_call=2)
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42"}

        update = await nodes.merge(state)

        self.assertEqual(update["status"], "merging")
        self.assertEqual(update["merge_verification_attempts"], 1)
        self.assertEqual(code_host.merged_prs, [("42", "squash")])
        # The stale pre-merge read is not written back: "open" is no longer
        # something this path knows, so the durable record is left alone.
        self.assertNotIn("pull_request_state", update)
        self.assertEqual(route_merge(cast(IssueWorkflowState, {**state, **update})), "merge")

    async def test_merge_waits_when_the_pull_request_cannot_be_read_at_all(self) -> None:
        code_host = _FakeCodeHost(read_error_on_call=1)
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)

        update = await nodes.merge({"workflow_run_id": "wf-1", "pull_request_id": "42"})

        self.assertEqual(update["status"], "merging")
        self.assertEqual(code_host.merged_prs, [])
        self.assertNotIn("pull_request_id", update)

    async def test_merge_blocks_once_the_verification_budget_is_spent(self) -> None:
        """Waiting is bounded: a pull request that never merges must not
        re-enter the node forever."""
        budget = RetryBudget()
        code_host = _FakeCodeHost(merge_result_state=None)
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state = cast(IssueWorkflowState, {
            "workflow_run_id": "wf-1",
            "pull_request_id": "42",
            "merge_verification_attempts": budget.merge_verification_attempts - 1,
        })

        update = await nodes.merge(state)

        self.assertEqual(update["status"], "blocked")
        reason = str(update["blocking_reason"])
        self.assertIn(f"not confirmed after {budget.merge_verification_attempts} attempts", reason)
        self.assertIn("still reports it open", reason)
        self.assertEqual(route_merge(cast(IssueWorkflowState, {**state, **update})), "blocked")

    async def test_merge_without_code_host_waits_instead_of_delivering(self) -> None:
        """No adapter means no verification, and an unverified merge is not a
        merge -- the run waits rather than completing on nothing."""
        update = await self.nodes.merge({"workflow_run_id": "wf-1"})
        self.assertEqual(
            update,
            {"status": "merging", "merge_verification_attempts": 1, "pull_request_merged": False},
        )
        self.assertEqual(route_merge(cast(IssueWorkflowState, update)), "merge")

    async def test_merge_transitions_to_blocked_when_code_host_refuses(self) -> None:
        code_host = _FakeCodeHost(merge_error=GitHubCliError("refusing to merge pull request"))
        nodes = PersistedWorkflowNodes(self.persistence, self.dispatcher, code_host_factory=lambda project_id: code_host)
        state: IssueWorkflowState = {"workflow_run_id": "wf-1", "pull_request_id": "42", "merge_method": "squash"}
        update = await nodes.merge(state)
        self.assertEqual(update["status"], "blocked")
        self.assertIn("refusing to merge", str(update["blocking_reason"]))
        self.assertEqual(code_host.merged_prs, [])
        self.assertEqual(route_merge(cast(IssueWorkflowState, {**state, **update})), "blocked")

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
