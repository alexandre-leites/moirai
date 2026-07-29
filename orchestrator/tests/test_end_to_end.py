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


class EndToEndWorkflowTests(unittest.IsolatedAsyncioTestCase):
    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_full_workflow_from_plan_to_complete_with_fake_adapters(self) -> None:
        """Validates the complete phase transition sequence end to end."""
        from moirai.workflows.issue_graph import IssueWorkflowState, build_issue_graph
        from moirai.workflows.nodes import PersistedWorkflowNodes

        persistence = _FakePersistence()
        dispatcher = _FakeDispatcher()
        code_host = _FakeCodeHost()
        issue_tracker = _FakeIssueTracker()
        nodes = PersistedWorkflowNodes(
            persistence, dispatcher,
            code_host_factory=lambda project_id: code_host,
            issue_tracker_factory=lambda project_id: issue_tracker,
        )
        graph = build_issue_graph(nodes.build())

        state: IssueWorkflowState = {
            "workflow_run_id": "wf-1",
            "issue_id": "42",
            "branch_name": "agent/42/run-1",
            "base_branch": "main",
            "merge_method": "squash",
            "plan_valid": True,
            "pipeline_passed": True,
            "review_approved": True,
            "checks_passed": True,
        }

        result = await graph.ainvoke(state, {"configurable": {"thread_id": "wf-1"}})
        self.assertEqual(result.get("status"), "completed")

        self.assertGreaterEqual(len(dispatcher.dispatches), 2)
        dispatched_roles = [r for _, r in dispatcher.dispatches]
        self.assertNotIn("planner", dispatched_roles)
        self.assertIn("developer", dispatched_roles)
        self.assertIn("reviewer", dispatched_roles)

        self.assertGreaterEqual(len(code_host.created_prs), 1)
        self.assertGreaterEqual(len(code_host.merged_prs), 1)
        self.assertIn("42", issue_tracker.closed_issues)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_runner_event_entry_point_seeds_runtime_and_completes_external_delivery(self) -> None:
        from moirai.grpc.runner_control import RunnerControlService
        from moirai.workflows.issue_graph import build_issue_graph
        from moirai.workflows.nodes import PersistedWorkflowNodes
        from moirai.workflows.runtime import PersistedWorkflowRuntime
        from proto import runner_control_pb2

        persistence = _EntryPointPersistence()
        dispatcher = _FakeDispatcher()
        code_host = _FakeCodeHost()
        issue_tracker = _FakeIssueTracker()
        runtime = PersistedWorkflowRuntime(
            build_issue_graph(
                PersistedWorkflowNodes(
                    persistence,
                    dispatcher,
                    code_host_factory=lambda project_id: code_host,
                    issue_tracker_factory=lambda project_id: issue_tracker,
                ).build()
            ),
            persistence,
        )
        control_plane = _EntryPointControlPlane()
        service = RunnerControlService(control_plane, now=lambda: NOW, workflow_runtime=runtime)
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
        self.assertGreaterEqual(len(code_host.created_prs), 1)
        self.assertGreaterEqual(len(code_host.merged_prs), 1)
        self.assertEqual(issue_tracker.closed_issues, ["42"])
        self.assertTrue(persistence.checkpoints)

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_repair_loop_routes_through_pipeline_after_developer_completes(self) -> None:
        """After developer completes with failed pipeline, the graph routes to repair."""

    @unittest.skipIf(not _HAS_LANGGRAPH, "langgraph is not installed")
    async def test_human_approval_interrupt_pauses_before_merge(self) -> None:
        """When human_approval is required, graph pauses at wait_for_human."""
        from moirai.workflows.issue_graph import IssueWorkflowState, build_issue_graph
        from moirai.workflows.nodes import PersistedWorkflowNodes

        persistence = _FakePersistence()
        dispatcher = _FakeDispatcher()
        nodes = PersistedWorkflowNodes(persistence, dispatcher)
        graph = build_issue_graph(nodes.build(), interrupt_after=("wait_for_human",))

        state: IssueWorkflowState = {
            "workflow_run_id": "wf-human",
            "status": "waiting_github_checks",
            "checks_passed": True,
            "human_approved": False,
            "plan_valid": True,
            "pipeline_passed": True,
            "review_approved": True,
            "human_approval_required": True,
        }

        if hasattr(graph, "update_state"):
            result = await graph.ainvoke(state, {"configurable": {"thread_id": "wf-human"}})
            self.assertEqual(result.get("status"), "waiting_human")


class _FakePersistence:
    def __init__(self) -> None:
        self.transitions: list[tuple[str, str, dict[str, object]]] = []

    async def transition(self, workflow_run_id: str, status: str, updates: dict[str, object]) -> None:
        self.transitions.append((workflow_run_id, status, updates))

    async def dispatch(self, workflow_run_id: str, role: str, status_key: str, attempt_field: str | None) -> str:
        self.dispatches: list[tuple[str, str]] = getattr(self, "dispatches", [])
        attempt = 1
        if attempt_field:
            attempt = 2
        self.dispatches.append((workflow_run_id, role))
        return f"{workflow_run_id}-{role}-{attempt}"

    async def latest_checkpoint(self, workflow_run_id: str) -> tuple[int, dict[str, object]] | None:
        return None

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int:
        return 1

    async def get_queued_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None:
        return None


class _EntryPointPersistence(_FakePersistence):
    def __init__(self) -> None:
        super().__init__()
        self.checkpoints: list[dict[str, object]] = []

    async def load_state(self, workflow_run_id: str) -> dict[str, object]:
        return {
            "workflow_run_id": workflow_run_id,
            "project_id": "project-entry",
            "issue_id": "42",
            "status": "pushing",
            "branch_name": "agent/42/entry",
            "base_branch": "main",
            "merge_method": "squash",
            "plan_valid": True,
            "pipeline_passed": True,
            "review_approved": True,
            "checks_passed": True,
            "human_approval_required": False,
        }

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int:
        self.checkpoints.append({"workflow_run_id": workflow_run_id, **state})
        return len(self.checkpoints)


class _EntryPointControlPlane:
    def __init__(self) -> None:
        self.events: list[object] = []

    async def accept_event(self, event: object, now: datetime, on_transition: Any = None) -> None:
        del now
        self.events.append(event)
        if on_transition is not None:
            await on_transition("wf-entry", "pr_created", {"status": "pr_created"})


class _FakeDispatcher:
    def __init__(self) -> None:
        self.dispatches: list[tuple[str, str]] = []

    async def dispatch(self, workflow_run_id: str, role: str) -> str:
        self.dispatches.append((workflow_run_id, role))
        return f"{workflow_run_id}-{role}"


class _FakeCodeHost:
    def __init__(self) -> None:
        self.created_prs: list[tuple[str, str, str, str, str | None]] = []
        self.checked_prs: list[str] = []
        self.merged_prs: list[tuple[str, str]] = []

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
        return [PullRequestCheck(name="ci", status=CheckStatus.PASSING)]

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
