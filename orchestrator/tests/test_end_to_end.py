from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta
from typing import Any, Self

from moirai.domain.control_plane import InMemoryControlPlane
from moirai.domain.models import Issue, Project, Runner
from moirai.scheduler import Scheduler

try:
    import langgraph  # noqa: F401
    _HAS_LANGGRAPH = True
except ModuleNotFoundError:
    _HAS_LANGGRAPH = False


NOW = datetime(2026, 6, 15, 12, 0, tzinfo=UTC)


class _Transaction:
    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None


class _DurableConnection:
    def __init__(self, pool: _ExecutionPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "workflow_execution_requests AS request" in query:
            if self.pool.execution_request_status != "queued":
                return None
            self.pool.execution_request_claimed = True
            return {
                "request_id": self.pool.execution_request_id,
                "job_id": self.pool.job_id,
                "workflow_run_id": self.pool.workflow_id,
                "issue_id": self.pool.issue_id,
                "project_id": self.pool.project_id,
                "external_id": "42",
                "priority": 100,
                "external_created_at": NOW - timedelta(days=1),
                "last_synced_at": NOW - timedelta(hours=1),
                "runner_id": self.pool.runner_id,
                "labels": ["linux"],
                "runner_enabled": True,
                "draining": False,
                "status": "online",
            }
        if "SET runner_id = $2, status = 'offered'" in query:
            self.pool.job_status = "offered"
            self.pool.last_event_sequence = 0
            return {"lease_generation": 2}
        if "AS execution_role" in query and "jobs AS j" in query:
            return {
                "job_id": self.pool.job_id,
                "external_id": "42",
                "title": "Test issue",
                "body": "Test body",
                "project_id": self.pool.project_id,
                "repository_mode": "managed_clone",
                "repository_url": "https://github.com/example/test.git",
                "local_repository_path": None,
                "default_branch": "main",
                "execution_request_id": self.pool.execution_request_id,
                "execution_role": "developer",
            }
        raise AssertionError(f"unexpected fetchrow: {query[:80]}")

    async def execute(self, query: str, *arguments: object) -> str:
        self.pool.queries.append(query[:100])
        if "UPDATE app.workflow_execution_requests" in query:
            if self.pool.execution_request_status != "queued":
                return "UPDATE 0"
            self.pool.execution_request_status = "dispatched"
            return "UPDATE 1"
        if "INSERT INTO app.job_offers" in query:
            self.pool.offer_created = True
            return "INSERT 0 1"
        if "INSERT INTO app.audit_events" in query:
            return "INSERT 0 1"
        return "INSERT 0 1"


class _ExecutionPool:
    def __init__(self) -> None:
        self.workflow_id = "00000000-0000-0000-0000-000000000001"
        self.project_id = "00000000-0000-0000-0000-000000000002"
        self.job_id = "00000000-0000-0000-0000-000000000003"
        self.runner_id = "00000000-0000-0000-0000-000000000004"
        self.issue_id = "00000000-0000-0000-0000-000000000005"
        self.execution_request_id = "00000000-0000-0000-0000-000000000006"
        self.execution_request_status = "queued"
        self.execution_request_claimed = False
        self.job_status = "completed"
        self.last_event_sequence = 0
        self.offer_created = False
        self.queries: list[str] = []

    def acquire(self) -> _DurableConnection:
        return _DurableConnection(self)

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        return await _DurableConnection(self).fetchrow(query, *arguments)


class _ExecutionFakeControlPlane:
    def __init__(self) -> None:
        self.pool = _ExecutionPool()

    async def schedule_execution(self, now: datetime, offer_ttl: timedelta) -> object:
        from moirai.persistence.control_plane import AsyncpgControlPlane
        return await AsyncpgControlPlane(self.pool).schedule_execution(now, offer_ttl)

    async def expire_offers(self, now: datetime) -> tuple[str, ...]:
        return ()

    async def expire_leases(self, now: datetime) -> tuple[str, ...]:
        return ()

    async def schedule(self, now: datetime, offer_ttl: timedelta) -> object:
        return None

    async def build_task_packet(self, scheduled: object) -> dict[str, object]:
        return {"role": "test"}

    async def reject_offer(self, job_id: str, runner_id: str, now: datetime) -> None:
        pass


class EndToEndExecutionFlowTests(unittest.IsolatedAsyncioTestCase):
    async def test_scheduler_claims_execution_request_with_asyncpg_fake(self) -> None:
        """Scheduler.tick calls schedule_execution which claims a queued execution request."""
        control_plane = _ExecutionFakeControlPlane()
        delivered: list[object] = []

        async def deliver(offer: object, packet: dict[str, object]) -> bool:
            delivered.append(offer)
            return True

        scheduler = Scheduler(
            control_plane, deliver, control_plane.build_task_packet, timedelta(seconds=30),
        )
        result = await scheduler.tick(NOW)

        self.assertEqual(len(result), 1)
        self.assertTrue(control_plane.pool.execution_request_claimed)
        self.assertTrue(control_plane.pool.offer_created)
        self.assertEqual(control_plane.pool.execution_request_status, "dispatched")
        self.assertEqual(len(delivered), 1)

    async def test_scheduler_falls_back_to_recovery_when_no_execution_request(self) -> None:
        """When no queued execution request exists, scheduler falls back to recovery then schedule."""
        control_plane = InMemoryControlPlane()
        control_plane.add_project(Project("project-1", True, frozenset({"linux"})))
        control_plane.add_issue(
            Issue("issue-1", "project-1", "42", 10, NOW - timedelta(days=1), NOW, True),
        )
        control_plane._runners["runner-1"] = Runner("runner-1", frozenset({"linux"}), True, True, False, True)

        delivered: list[tuple[str, dict[str, object]]] = []

        async def deliver(offer: object, packet: dict[str, object]) -> bool:
            delivered.append((offer.job_id, packet))
            return True

        scheduler = Scheduler(
            control_plane, deliver, lambda s: {"jobId": s.offer.job_id}, timedelta(seconds=30),
        )
        result = await scheduler.tick(NOW)

        self.assertEqual(len(result), 1)
        self.assertEqual(len(delivered), 1)

    async def test_accept_event_with_on_transition_callback(self) -> None:
        """accept_event invokes the on_transition callback for terminal events."""
        from moirai.domain.control_plane import InMemoryControlPlane
        from moirai.domain.models import ExecutionEvent

        control_plane = InMemoryControlPlane()
        control_plane.add_project(Project("project-1", True, frozenset({"linux"})))
        control_plane.add_issue(
            Issue("issue-1", "project-1", "42", 10, NOW - timedelta(days=1), NOW, True),
        )
        control_plane._runners["runner-1"] = Runner("runner-1", frozenset({"linux"}), True, True, False, True)
        scheduled = control_plane.schedule(NOW, timedelta(seconds=30))
        assert scheduled is not None

        control_plane.accept_offer(scheduled.offer.job_id, scheduled.offer.runner_id, NOW)
        transition_calls: list[tuple[str, str, dict[str, object]]] = []

        def on_transition(workflow_run_id: str, new_status: str, state_updates: dict[str, object]) -> None:
            transition_calls.append((workflow_run_id, new_status, state_updates))

        started_lease = control_plane.accept_event(
            ExecutionEvent(
                job_id=scheduled.offer.job_id,
                runner_id=scheduled.offer.runner_id,
                lease_generation=1,
                event_sequence=1,
                event_type="started",
                execution_id=f"{scheduled.offer.job_id}-plan",
                payload={"status": "running"},
            ),
            NOW,
            on_transition=on_transition,
        )
        self.assertEqual(started_lease.last_event_sequence, 1)
        self.assertEqual(len(transition_calls), 0)

        terminal_lease = control_plane.accept_event(
            ExecutionEvent(
                job_id=scheduled.offer.job_id,
                runner_id=scheduled.offer.runner_id,
                lease_generation=1,
                event_sequence=2,
                event_type="completed",
                execution_id=f"{scheduled.offer.job_id}-plan",
                payload={"status": "completed", "exitCode": 0, "changedFiles": [], "commandsRun": []},
            ),
            NOW,
            on_transition=on_transition,
        )
        self.assertEqual(terminal_lease.last_event_sequence, 2)
        self.assertEqual(len(transition_calls), 1)

    async def test_task_packet_role_matches_execution_request(self) -> None:
        """build_task_packet generates role-scoped packets based on execution request role."""
        pool = _ExecutionPool()
        pool.execution_request_status = "queued"

        from moirai.persistence.control_plane import AsyncpgControlPlane
        control_plane = AsyncpgControlPlane(pool)

        scheduled = await control_plane.schedule_execution(NOW, timedelta(seconds=30))
        self.assertIsNotNone(scheduled)
        assert scheduled is not None

        packet = await control_plane.build_task_packet(scheduled)

        self.assertEqual(packet["role"], "developer")
        self.assertTrue(packet["executionId"].endswith("-implement"))
        self.assertEqual(packet["constraints"]["mayModifyFiles"], True)
        self.assertEqual(packet["constraints"]["mayPush"], True)
        self.assertEqual(packet["constraints"]["mayMerge"], False)
        self.assertEqual(
            [reference["name"] for reference in packet["environmentRefs"]],
            ["GITHUB_TOKEN"],
        )


# Counters that app.workflow_runs stores as columns; _WorkflowStore mirrors the
# subset the graph reads back through load_state().
_DURABLE_COUNTERS = (
    "planning_attempts",
    "implementation_attempts",
    "pipeline_repair_attempts",
    "review_cycles",
    "ci_repair_attempts",
    "total_agent_executions",
    "blocking_reason",
    "branch_name",
    "pull_request_id",
    "pull_request_url",
)

_PLANNER_READY: dict[str, Any] = {
    "status": "ready",
    "summary": "plan ready",
    "assumptions": [],
    "questions": [],
    "risk": "low",
    "acceptanceCriteria": [],
    "steps": [],
}
_REVIEW_APPROVED: dict[str, Any] = {"verdict": "approved", "acceptanceCriteria": [], "findings": []}


class _WorkflowStore:
    """In-memory stand-in for the tables the persisted runtime writes through:
    app.workflow_runs, app.workflow_execution_requests, app.workflow_checkpoints."""

    def __init__(self, human_approval_required: bool = False) -> None:
        self.workflow_run_id = "wf-1"
        self.transitions: list[tuple[str, dict[str, object]]] = []
        self.requests: list[dict[str, str]] = []
        self.checkpoints: list[dict[str, object]] = []
        self.durable: dict[str, object] = {
            "workflow_run_id": self.workflow_run_id,
            "project_id": "project-1",
            "issue_id": "42",
            "status": "preparing",
            "branch_name": "agent/42/run-1",
            "base_branch": "main",
            "merge_method": "squash",
            "human_approval_required": human_approval_required,
            "planning_attempts": 0,
            "implementation_attempts": 0,
            "pipeline_repair_attempts": 0,
            "review_cycles": 0,
            "ci_repair_attempts": 0,
            "total_agent_executions": 0,
        }

    async def transition(self, workflow_run_id: str, status: str, updates: dict[str, object]) -> None:
        del workflow_run_id
        self.transitions.append((status, dict(updates)))
        self.durable["status"] = status
        for key in _DURABLE_COUNTERS:
            if key in updates:
                self.durable[key] = updates[key]

    async def get_queued_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None:
        del workflow_run_id
        for request in self.requests:
            if request["status"] == "queued":
                return {"id": request["id"], "role": request["role"], "attempt": 1}
        return None

    async def dispatch(self, workflow_run_id: str, role: str) -> str:
        request = {
            "id": f"{workflow_run_id}-{role}-{len(self.requests) + 1}",
            "role": role,
            "status": "queued",
        }
        self.requests.append(request)
        return request["id"]

    async def load_state(self, workflow_run_id: str) -> dict[str, object]:
        del workflow_run_id
        return dict(self.durable)

    async def latest_checkpoint(self, workflow_run_id: str) -> tuple[int, dict[str, object]] | None:
        del workflow_run_id
        if not self.checkpoints:
            return None
        return len(self.checkpoints), dict(self.checkpoints[-1])

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int:
        del workflow_run_id
        self.checkpoints.append(dict(state))
        return len(self.checkpoints)

    @property
    def status(self) -> str:
        return str(self.durable["status"])

    @property
    def roles(self) -> list[str]:
        return [request["role"] for request in self.requests]

    def claim_queued(self) -> None:
        """Simulates the scheduler claiming every queued execution request."""
        for request in self.requests:
            if request["status"] == "queued":
                request["status"] = "dispatched"


class _EventDrivenWorkflow:
    """Drives the real issue graph the way production does: one runner terminal
    event at a time, through the same translation and resume path as
    AsyncpgControlPlane.accept_event -> RunnerControlService._advance_workflow."""

    def __init__(
        self,
        human_approval_required: bool = False,
        code_host: Any = None,
        issue_tracker: Any = None,
    ) -> None:
        from langgraph.checkpoint.memory import InMemorySaver

        from moirai.workflows.issue_graph import build_issue_graph
        from moirai.workflows.nodes import PersistedWorkflowNodes
        from moirai.workflows.runtime import PersistedWorkflowRuntime

        self.store = _WorkflowStore(human_approval_required=human_approval_required)
        self.code_host = code_host
        self.issue_tracker = issue_tracker
        nodes = PersistedWorkflowNodes(
            self.store,
            self.store,
            code_host_factory=None if code_host is None else (lambda project_id: code_host),
            issue_tracker_factory=None if issue_tracker is None else (lambda project_id: issue_tracker),
        )
        self.graph = build_issue_graph(
            nodes.build(), checkpointer=InMemorySaver(), interrupt_before=("wait_for_human",)
        )
        self.runtime = PersistedWorkflowRuntime(self.graph, self.store, has_checkpointer=True)

    async def start(self) -> dict[str, object]:
        return await self.runtime.run(self.store.workflow_run_id, {"status": "preparing"})

    async def deliver(
        self,
        role: str,
        execution_suffix: str,
        result: dict[str, Any] | None = None,
        event_type: str = "completed",
        exit_code: int = 0,
    ) -> dict[str, object]:
        from moirai.workflows.runner_events import (
            RunnerEventSummary,
            workflow_transition_for_terminal_event,
        )

        self.store.claim_queued()
        summary = RunnerEventSummary(
            event_type=event_type,
            execution_id=f"job-1-{execution_suffix}",
            exit_code=exit_code,
            changed_files=[],
            commands_run=[],
            terminal=True,
            result=result,
        )
        transition = workflow_transition_for_terminal_event(summary, self.store.status, role=role)
        if transition is None:
            raise AssertionError(f"terminal {role} event produced no workflow transition")
        return await self.runtime.run(
            self.store.workflow_run_id,
            {"status": transition.new_status, **transition.state_updates},
        )

    async def pending_nodes(self) -> tuple[str, ...]:
        snapshot = await self.graph.aget_state(
            {"configurable": {"thread_id": self.store.workflow_run_id}}
        )
        return tuple(snapshot.next)


class EndToEndWorkflowTests(unittest.IsolatedAsyncioTestCase):
    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_sequential_runner_events_drive_the_workflow_to_completed(self) -> None:
        """The full phase sequence, driven only by simulated runner events: no
        gate boolean is ever pre-seeded, so every transition is earned by an
        execution that actually reported back."""
        workflow = _EventDrivenWorkflow(code_host=_FakeCodeHost(), issue_tracker=_FakeIssueTracker())

        state = await workflow.start()
        self.assertEqual(state["status"], "planning")
        self.assertEqual(workflow.store.roles, ["planner"])

        state = await workflow.deliver("planner", "plan", result=_PLANNER_READY)
        self.assertEqual(state["status"], "implementing")
        self.assertEqual(workflow.store.roles, ["planner", "developer"])

        # A clean developer exit queues the local pipeline; it never satisfies
        # the pipeline gate on its own.
        state = await workflow.deliver("developer", "implement")
        self.assertEqual(state["status"], "local_pipeline")
        self.assertNotIn("pipeline_passed", state)
        self.assertEqual(workflow.store.roles, ["planner", "developer", "pipeline"])

        state = await workflow.deliver("pipeline", "pipeline")
        self.assertEqual(state["status"], "ai_review")
        self.assertIs(state["pipeline_passed"], True)
        self.assertEqual(workflow.store.roles, ["planner", "developer", "pipeline", "reviewer"])

        state = await workflow.deliver("reviewer", "review", result=_REVIEW_APPROVED)
        self.assertEqual(state["status"], "pushing")
        self.assertEqual(
            workflow.store.roles, ["planner", "developer", "pipeline", "reviewer", "developer"]
        )

        state = await workflow.deliver("developer", "implement")
        self.assertEqual(state["status"], "completed")
        self.assertEqual(
            workflow.store.roles, ["planner", "developer", "pipeline", "reviewer", "developer"]
        )
        self.assertEqual(len(workflow.code_host.created_prs), 1)
        self.assertEqual(workflow.code_host.merged_prs, [("42", "squash")])
        self.assertEqual(workflow.issue_tracker.closed_issues, ["42"])
        # Five executions ran, but the deterministic pipeline is not an agent
        # run and does not spend the agent budget.
        self.assertEqual(workflow.store.roles.count("pipeline"), 1)
        self.assertEqual(workflow.store.durable["total_agent_executions"], 4)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_one_terminal_event_dispatches_one_execution_and_ends_the_invocation(self) -> None:
        """Regression for the run-to-completion graph: a planner success must
        queue exactly one developer execution and stop, instead of walking
        pipeline/repair against gates the developer has not produced yet."""
        workflow = _EventDrivenWorkflow()
        await workflow.start()

        state = await workflow.deliver("planner", "plan", result=_PLANNER_READY)

        self.assertEqual(state["status"], "implementing")
        self.assertTrue(state["awaiting_execution"])
        self.assertEqual(workflow.store.roles, ["planner", "developer"])
        self.assertEqual([request["status"] for request in workflow.store.requests], ["dispatched", "queued"])
        self.assertEqual(await workflow.pending_nodes(), ())
        # Phases that never ran must not have consumed any budget.
        self.assertEqual(workflow.store.durable["planning_attempts"], 1)
        self.assertEqual(workflow.store.durable["implementation_attempts"], 1)
        self.assertEqual(workflow.store.durable["pipeline_repair_attempts"], 0)
        self.assertEqual(workflow.store.durable["review_cycles"], 0)
        self.assertEqual(workflow.store.durable["total_agent_executions"], 2)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_a_clean_developer_exit_cannot_reach_review_without_the_pipeline(self) -> None:
        """Regression for the inferred pipeline gate: a developer that exits 0
        without changing anything used to satisfy `pipeline_passed` and sail
        into AI review. The deterministic pipeline must run first and its
        verdict alone must decide where the workflow goes next."""
        workflow = _EventDrivenWorkflow()
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)

        state = await workflow.deliver("developer", "implement", exit_code=0)

        self.assertEqual(state["status"], "local_pipeline")
        self.assertTrue(state["awaiting_execution"])
        self.assertNotIn("pipeline_passed", state)
        self.assertEqual(workflow.store.roles, ["planner", "developer", "pipeline"])
        self.assertNotIn("reviewer", workflow.store.roles)
        # Suspended on the pipeline execution: nothing advances until it reports.
        self.assertEqual(await workflow.pending_nodes(), ())

        state = await workflow.deliver("pipeline", "pipeline", event_type="failed", exit_code=1)

        self.assertEqual(state["status"], "repairing")
        self.assertIs(state["pipeline_passed"], False)
        self.assertEqual(workflow.store.roles, ["planner", "developer", "pipeline", "repairer"])
        self.assertNotIn("reviewer", workflow.store.roles)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_a_repair_gets_its_own_pipeline_execution_despite_an_earlier_pass(self) -> None:
        """A repair triggered by AI review re-enters the pipeline phase while an
        earlier `pipeline_passed = True` is still in state. That stale gate must
        not skip the run: the repaired tree gets its own pipeline verdict."""
        workflow = _EventDrivenWorkflow()
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)
        await workflow.deliver("developer", "implement")
        state = await workflow.deliver("pipeline", "pipeline")
        self.assertIs(state["pipeline_passed"], True)

        changes_requested = {"verdict": "changes_requested", "acceptanceCriteria": [], "findings": []}
        state = await workflow.deliver("reviewer", "review", result=changes_requested)
        self.assertEqual(state["status"], "repairing")

        state = await workflow.deliver("repairer", "repair")

        self.assertEqual(state["status"], "local_pipeline")
        self.assertEqual(workflow.store.roles[-1], "pipeline")
        self.assertEqual(workflow.store.roles.count("pipeline"), 2)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_full_review_cycle_still_reaches_delivery_on_the_agent_budget(self) -> None:
        """Making the pipeline mandatory added an execution to every
        review-driven repair cycle. If those pipeline runs spent
        `total_agent_executions`, the third review cycle would exhaust the
        budget and a workflow the reviewer just APPROVED would block at `push`
        with no pull request. Every review cycle the budget allows must still
        reach delivery."""
        workflow = _EventDrivenWorkflow(code_host=_FakeCodeHost(), issue_tracker=_FakeIssueTracker())
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)
        await workflow.deliver("developer", "implement")
        await workflow.deliver("pipeline", "pipeline")

        changes_requested = {"verdict": "changes_requested", "acceptanceCriteria": [], "findings": []}
        for _ in range(2):
            state = await workflow.deliver("reviewer", "review", result=changes_requested)
            self.assertEqual(state["status"], "repairing")
            await workflow.deliver("repairer", "repair")
            state = await workflow.deliver("pipeline", "pipeline")
            self.assertEqual(state["status"], "ai_review")

        state = await workflow.deliver("reviewer", "review", result=_REVIEW_APPROVED)
        self.assertEqual(state["status"], "pushing")
        self.assertNotIn("blocking_reason", state)

        state = await workflow.deliver("developer", "implement")
        self.assertEqual(state["status"], "completed")
        self.assertEqual(len(workflow.code_host.created_prs), 1)
        self.assertEqual(workflow.store.durable["review_cycles"], 3)
        self.assertEqual(workflow.store.roles.count("pipeline"), 3)
        self.assertEqual(workflow.store.durable["total_agent_executions"], 8)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_repair_cycle_blocks_only_after_every_counted_attempt_really_ran(self) -> None:
        """Each counted repair attempt is preceded by a real repairer execution,
        and each pipeline verdict comes from a pipeline execution that reported
        back -- the budget can no longer be spent by phantom traversals."""
        workflow = _EventDrivenWorkflow()
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)

        state = await workflow.deliver("developer", "implement")
        self.assertEqual(state["status"], "local_pipeline")
        self.assertEqual(workflow.store.roles[-1], "pipeline")

        for expected_attempts in (1, 2, 3):
            state = await workflow.deliver("pipeline", "pipeline", event_type="failed", exit_code=1)
            self.assertEqual(state["status"], "repairing")
            self.assertEqual(workflow.store.roles[-1], "repairer")
            self.assertEqual(workflow.store.durable["pipeline_repair_attempts"], expected_attempts)
            state = await workflow.deliver("repairer", "repair")
            self.assertEqual(state["status"], "local_pipeline")
            self.assertEqual(workflow.store.roles[-1], "pipeline")

        state = await workflow.deliver("pipeline", "pipeline", event_type="failed", exit_code=1)
        self.assertEqual(state["status"], "blocked")
        self.assertEqual(state["blocking_reason"], "workflow retry budget exhausted")
        self.assertEqual(workflow.store.roles.count("repairer"), 3)
        self.assertEqual(workflow.store.durable["pipeline_repair_attempts"], 3)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_failing_checks_spend_the_ci_budget_and_leave_the_pipeline_budget_alone(self) -> None:
        """Both repair sources in one run: a failing local pipeline spends
        `pipeline_repair_attempts`, a failing GitHub check spends
        `ci_repair_attempts`, and neither touches the other. The CI repair used
        to increment the pipeline counter, so the two were indistinguishable and
        `ci_repair_attempts` never left 0."""
        code_host = _FakeCodeHost()
        workflow = _EventDrivenWorkflow(code_host=code_host, issue_tracker=_FakeIssueTracker())
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)
        await workflow.deliver("developer", "implement")

        state = await workflow.deliver("pipeline", "pipeline", event_type="failed", exit_code=1)
        self.assertEqual(state["status"], "repairing")
        self.assertEqual(workflow.store.durable["pipeline_repair_attempts"], 1)
        self.assertEqual(workflow.store.durable["ci_repair_attempts"], 0)

        await workflow.deliver("repairer", "repair")
        await workflow.deliver("pipeline", "pipeline")
        await workflow.deliver("reviewer", "review", result=_REVIEW_APPROVED)

        code_host.checks_pass = False
        state = await workflow.deliver("developer", "implement")

        self.assertEqual(state["status"], "repairing")
        self.assertEqual(workflow.store.roles[-1], "repairer")
        self.assertEqual(workflow.store.durable["ci_repair_attempts"], 1)
        self.assertEqual(workflow.store.durable["pipeline_repair_attempts"], 1)
        self.assertEqual(code_host.merged_prs, [])

        # The CI repair rejoins the workflow where a local repair does: its own
        # pipeline verdict on the repaired tree, not a straight re-push.
        state = await workflow.deliver("repairer", "repair")
        self.assertEqual(state["status"], "local_pipeline")
        self.assertEqual(workflow.store.roles[-1], "pipeline")

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_the_ci_repair_budget_stops_the_failing_check_loop(self) -> None:
        """`ci_repair_attempts` is enforceable: the failing-check loop stops
        because the CI counter reached its bound, with the local repair budget
        untouched. While CI repairs incremented the pipeline counter this was
        impossible -- the CI counter never left 0, so the bound it gates could
        never be what stopped anything.

        The starting counters are deliberately synthetic: `ci_repair_attempts`
        is seeded ahead so that the CI bound, and not `total_agent_executions`
        or `review_cycles`, is the first bound the loop meets. A real workflow
        row with two CI repairs would also carry roughly eight agent executions,
        and the global budget would stop the loop first -- which is why
        `orchestrator/README.md` documents the CI bound as a knob that only
        binds once the other budgets are raised, and why this test isolates it
        rather than pretending the default configuration reaches it."""
        code_host = _FakeCodeHost(checks_pass=False)
        workflow = _EventDrivenWorkflow(code_host=code_host, issue_tracker=_FakeIssueTracker())
        # Seeded on the app.workflow_runs row, which PersistedWorkflowRuntime
        # reads back into graph state on every resume.
        workflow.store.durable["ci_repair_attempts"] = 2
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)
        await workflow.deliver("developer", "implement")
        await workflow.deliver("pipeline", "pipeline")
        await workflow.deliver("reviewer", "review", result=_REVIEW_APPROVED)

        state = await workflow.deliver("developer", "implement")
        self.assertEqual(state["status"], "repairing")
        self.assertEqual(workflow.store.durable["ci_repair_attempts"], 3)

        await workflow.deliver("repairer", "repair")
        await workflow.deliver("pipeline", "pipeline")
        await workflow.deliver("reviewer", "review", result=_REVIEW_APPROVED)
        state = await workflow.deliver("developer", "implement")

        self.assertEqual(state["status"], "blocked")
        self.assertEqual(state["blocking_reason"], "workflow retry budget exhausted")
        self.assertEqual(workflow.store.durable["ci_repair_attempts"], 3)
        self.assertEqual(workflow.store.durable["pipeline_repair_attempts"], 0)
        self.assertEqual(workflow.store.roles.count("repairer"), 1)
        # The CI bound tripped, not the global one: 7 of 10 agent runs spent,
        # and `review_cycles` (2 of 3) had room as well.
        self.assertEqual(workflow.store.durable["total_agent_executions"], 7)
        self.assertEqual(workflow.store.durable["review_cycles"], 2)
        self.assertEqual(code_host.merged_prs, [])

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_human_approval_interrupt_pauses_before_merge(self) -> None:
        """With human approval required the graph stops before wait_for_human
        and merges only once the decision resumes it."""
        workflow = _EventDrivenWorkflow(
            human_approval_required=True,
            code_host=_FakeCodeHost(),
            issue_tracker=_FakeIssueTracker(),
        )
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)
        await workflow.deliver("developer", "implement")
        await workflow.deliver("pipeline", "pipeline")
        await workflow.deliver("reviewer", "review", result=_REVIEW_APPROVED)

        state = await workflow.deliver("developer", "implement")
        self.assertEqual(state["status"], "waiting_github_checks")
        self.assertEqual(await workflow.pending_nodes(), ("wait_for_human",))
        self.assertEqual(workflow.code_host.merged_prs, [])

        state = await workflow.runtime.run(
            workflow.store.workflow_run_id,
            {"human_approved": True, "human_changes_requested": False},
        )
        self.assertEqual(state["status"], "completed")
        self.assertEqual(workflow.code_host.merged_prs, [("42", "squash")])
        self.assertEqual(workflow.issue_tracker.closed_issues, ["42"])

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_runner_event_entry_point_resumes_the_graph_and_completes_delivery(self) -> None:
        """The gRPC entry point is wired end to end: a runner terminal event
        arriving on the stream resumes the suspended graph and finishes the
        external delivery (pull request, merge, issue closure)."""
        from moirai.grpc.runner_control import RunnerControlService
        from proto import runner_control_pb2

        workflow = _EventDrivenWorkflow(code_host=_FakeCodeHost(), issue_tracker=_FakeIssueTracker())
        await workflow.start()
        await workflow.deliver("planner", "plan", result=_PLANNER_READY)
        await workflow.deliver("developer", "implement")
        await workflow.deliver("pipeline", "pipeline")
        state = await workflow.deliver("reviewer", "review", result=_REVIEW_APPROVED)
        self.assertEqual(state["status"], "pushing")
        workflow.store.claim_queued()

        control_plane = _EntryPointControlPlane(workflow.store)
        service = RunnerControlService(control_plane, now=lambda: NOW, workflow_runtime=workflow.runtime)
        message = runner_control_pb2.RunnerToOrchestrator(
            event=runner_control_pb2.ExecutionEvent(
                job_id="job-entry",
                lease_generation=1,
                event_sequence=1,
                type="completed",
                execution_id="job-entry-implement",
                payload_json='{"exitCode":0}',
            )
        )

        await service._handle_message(message, "runner-entry")

        self.assertEqual(control_plane.events[0].job_id, "job-entry")
        self.assertEqual(workflow.store.status, "completed")
        self.assertEqual(len(workflow.code_host.created_prs), 1)
        self.assertEqual(workflow.code_host.merged_prs, [("42", "squash")])
        self.assertEqual(workflow.issue_tracker.closed_issues, ["42"])
        self.assertTrue(workflow.store.checkpoints)


class _EntryPointControlPlane:
    """Mirrors AsyncpgControlPlane.accept_event: record the event, translate it
    with the shared runner-event policy, hand the transition to on_transition."""

    def __init__(self, store: _WorkflowStore) -> None:
        self._store = store
        self.events: list[Any] = []

    async def accept_event(self, event: Any, now: datetime, on_transition: Any = None) -> None:
        del now
        from moirai.workflows.runner_events import (
            validate_runner_event,
            workflow_transition_for_terminal_event,
        )

        self.events.append(event)
        summary = validate_runner_event(event.event_type, event.execution_id, event.payload)
        transition = workflow_transition_for_terminal_event(summary, self._store.status, role="developer")
        if on_transition is not None and transition is not None:
            await on_transition(
                self._store.workflow_run_id, transition.new_status, transition.state_updates
            )


class _FakeCodeHost:
    def __init__(self, checks_pass: bool = True) -> None:
        self.created_prs: list[tuple[str, str, str, str, str | None]] = []
        self.checked_prs: list[str] = []
        self.merged_prs: list[tuple[str, str]] = []
        self.checks_pass = checks_pass

    async def get_pull_request(self, pull_request_id: str) -> Any:
        from moirai.code_hosts import PullRequest
        return PullRequest(external_id=pull_request_id, url="https://example.test/pr/42", state="open", head_branch="agent/42/run-1", head_commit="abc123")

    async def create_or_find_pull_request(
        self, workflow_id: str, branch: str, base_branch: str, title: str, issue_number: str | None = None,
    ) -> Any:
        self.created_prs.append((workflow_id, branch, base_branch, title, issue_number))
        from moirai.code_hosts import PullRequest
        return PullRequest(external_id="42", url="https://example.test/pr/42", state="open", head_branch=branch, head_commit="abc123")

    async def required_checks(self, pull_request_id: str) -> list[Any]:
        self.checked_prs.append(pull_request_id)
        from moirai.code_hosts import CheckStatus, PullRequestCheck
        status = CheckStatus.PASSING if self.checks_pass else CheckStatus.FAILING
        return [PullRequestCheck(name="ci", status=status)]

    async def merge_pull_request(self, pull_request_id: str, method: str) -> None:
        self.merged_prs.append((pull_request_id, method))


class _FakeIssueTracker:
    def __init__(self) -> None:
        self.closed_issues: list[str] = []
        self.added_labels: list[tuple[str, list[str]]] = []

    async def close_issue(self, external_issue_id: str) -> None:
        self.closed_issues.append(external_issue_id)

    async def add_labels(self, external_issue_id: str, labels: list[str]) -> None:
        self.added_labels.append((external_issue_id, labels))


if __name__ == "__main__":
    unittest.main()
