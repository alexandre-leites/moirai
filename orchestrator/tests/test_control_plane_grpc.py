import unittest
from datetime import UTC, datetime, timedelta

try:
    import grpc

    from moirai.domain.control_plane import AuthenticationError
    from moirai.main import register_services
    from moirai.persistence.authentication import (
        AccountProfile,
        AuthenticatedSession,
        SessionCredentials,
    )
    from proto import control_plane_pb2, control_plane_pb2_grpc, runner_control_pb2_grpc
except ModuleNotFoundError:
    grpc = None


NOW = datetime(2026, 1, 1, tzinfo=UTC)


class FakeControlPlane:
    """Implements moirai.grpc.protocol.ControlPlane for tests."""

    def __init__(self) -> None:
        self.token_labels: tuple[str, ...] | None = None
        self.token_expiry: datetime | None = None
        self.revoked_session_token: str | None = None
        self.revoked_session_at: datetime | None = None

    async def login(self, username: str, password: str, now: datetime) -> SessionCredentials:
        if username != "admin" or password != "correct":
            raise PermissionError()
        self.login_at = now
        return SessionCredentials(session_token="session-token", csrf_token="csrf-token", user_id="user-1", expires_at=now)

    async def validate_session(
        self, session_token: str, csrf_token: str | None, now: datetime, require_csrf: bool
    ) -> AuthenticatedSession:
        if require_csrf and csrf_token != "csrf-token":
            raise PermissionError()
        if session_token == "admin-session":
            return AuthenticatedSession(
                id="session-1", user_id="00000000-0000-0000-0000-000000000099", username="admin", role="admin", expires_at=now
            )
        if session_token == "viewer-session":
            return AuthenticatedSession(
                id="session-2", user_id="00000000-0000-0000-0000-000000000098", username="viewer", role="viewer", expires_at=now
            )
        raise PermissionError()

    async def update_account(
        self,
        user_id: str,
        keep_session_id: str,
        current_password: str,
        new_password: str,
        new_email: str,
        display_name: str,
        now: datetime,
    ) -> object:
        if user_id != "00000000-0000-0000-0000-000000000099" or current_password != "correct":
            raise AuthenticationError("current password is incorrect")
        return AccountProfile(
            user_id=user_id,
            username="admin",
            role="admin",
            email=new_email or "admin@example.com",
            display_name=display_name or "Admin",
        )

    async def list_projects(self) -> list[dict[str, object]]:
        return [
            {
                "id": "project-1",
                "name": "Example",
                "enabled": True,
                "repository_mode": "managed_clone",
                "repository_url": "https://example.test/repo.git",
                "local_repository_path": None,
                "default_branch": "main",
                "required_runner_labels": ["docker", "linux"],
                "pipeline_steps": [],
            }
        ]

    async def create_project(
        self,
        name: str,
        repository_mode: str,
        repository_url: str | None,
        local_repository_path: str | None,
        default_branch: str,
        labels: tuple[str, ...],
        now: datetime,
        actor_user_id: str | None,
        pipeline_steps: tuple[dict[str, object], ...] = (),
        execution_image: str = "",
    ) -> dict[str, object]:
        del repository_mode, repository_url, local_repository_path, default_branch, labels, now, actor_user_id, pipeline_steps
        return {"id": "project-2", "name": name, "enabled": True, "pipeline_steps": [], "execution_image": execution_image}

    async def update_project(
        self,
        project_id: str,
        name: str,
        repository_mode: str,
        repository_url: str | None,
        local_repository_path: str | None,
        default_branch: str,
        labels: tuple[str, ...],
        now: datetime,
        actor_user_id: str | None,
        pipeline_steps: tuple[dict[str, object], ...] = (),
        execution_image: str = "",
    ) -> dict[str, object]:
        del repository_mode, repository_url, local_repository_path, default_branch, labels, now, actor_user_id, pipeline_steps
        return {"id": project_id, "name": name, "enabled": True, "pipeline_steps": [], "execution_image": execution_image}

    async def set_project_enabled(
        self, project_id: str, enabled: bool, now: datetime, actor_user_id: str | None
    ) -> dict[str, object]:
        del now, actor_user_id
        return {"id": project_id, "name": "Example", "enabled": enabled}

    async def create_registration_token(
        self, labels: tuple[str, ...], expires_at: datetime, actor_user_id: str | None, now: datetime
    ) -> str:
        self.token_labels = labels
        self.token_expiry = expires_at
        self.token_actor = actor_user_id
        self.token_created_at = now
        return "runner-token"

    async def list_registration_tokens(self) -> list[dict[str, object]]:
        return [
            {
                "id": "token-1",
                "allowed_labels": ["docker"],
                "created_at": NOW,
                "expires_at": NOW + timedelta(minutes=15),
                "used_at": None,
                "revoked_at": None,
            }
        ]

    async def revoke_registration_token(
        self, token_id: str, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        self.revoked_token = (token_id, actor_user_id, now)
        return {
            "id": token_id,
            "allowed_labels": ["docker"],
            "created_at": NOW,
            "expires_at": NOW + timedelta(minutes=15),
            "used_at": None,
            "revoked_at": now,
        }

    async def list_workflows(self) -> list[dict[str, object]]:
        return [
            {
                "id": "workflow-1", "project_id": "project-1", "status": "preparing",
                "phase": "prepare_workspace", "issue_external_id": "41",
                "issue_title": "Prepare the workspace", "branch_name": "agent/41/prepare",
                "pull_request_external_id": None, "pull_request_url": None,
                "pull_request_state": None, "blocking_reason": None, "planning_attempts": 1,
                "implementation_attempts": 0, "pipeline_repair_attempts": 0, "review_cycles": 0,
                "ci_repair_attempts": 0, "total_agent_executions": 1,
                "created_at": NOW, "updated_at": NOW,
            }
        ]

    async def get_workflow(self, workflow_run_id: str) -> dict[str, object] | None:
        if workflow_run_id != "workflow-1":
            return None
        return {
            "id": "workflow-1", "project_id": "project-1", "status": "blocked", "phase": "blocked",
            "issue_external_id": "42", "issue_title": "Fix it", "branch_name": "agent/42/fix",
            "pull_request_external_id": "7", "pull_request_url": "https://example.test/pull/7",
            "pull_request_state": "open", "blocking_reason": "needs help", "planning_attempts": 1,
            "implementation_attempts": 2, "pipeline_repair_attempts": 3, "review_cycles": 4,
            "ci_repair_attempts": 5, "total_agent_executions": 6, "created_at": NOW, "updated_at": NOW,
        }

    async def list_workflow_events(
        self, workflow_run_id: str, after_id: int, limit: int
    ) -> list[dict[str, object]]:
        self.event_request = (workflow_run_id, after_id, limit)
        return [{"id": "8", "event_type": "log", "payload_json": '{"message":"agent output"}', "created_at": NOW}]

    async def stream_events(self, last_event_id: str):
        self.last_event_id = last_event_id
        yield {
            "id": "11",
            "event_type": "workflow",
            "workflow": await self.get_workflow("workflow-1"),
            "runner": None,
        }

    async def record_human_decision(
        self, workflow_run_id: str, decision: str, comment: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        del now
        if workflow_run_id != "workflow-waiting":
            raise ValueError("workflow run is not awaiting human approval")
        self.recorded_decision = (workflow_run_id, decision, comment, actor_user_id)
        return {"id": workflow_run_id, "project_id": "project-1", "status": "waiting_human"}

    async def retry_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        self.workflow_control = ("retry", workflow_run_id, reason, actor_user_id, now)
        return {"id": workflow_run_id, "project_id": "project-1", "status": "recovering", "phase": "recovering"}

    async def cancel_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        self.workflow_control = ("cancel", workflow_run_id, reason, actor_user_id, now)
        return {"id": workflow_run_id, "project_id": "project-1", "status": "cancelled", "phase": "cancelled"}

    async def block_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        self.workflow_control = ("block", workflow_run_id, reason, actor_user_id, now)
        return {"id": workflow_run_id, "project_id": "project-1", "status": "blocked", "phase": "blocked"}

    async def list_runners(self) -> list[dict[str, object]]:
        return []

    async def list_queue(self, now: datetime, limit: int) -> list[dict[str, object]]:
        del now
        self.queue_request = ("list_queue", limit)
        return [
            {
                "project_id": "project-1",
                "project_name": "Example",
                "external_id": "42",
                "title": "Implement queue",
                "priority": 100,
                "blocked_reason": "",
            }
        ]

    async def revoke_session(self, session_token: str, now: datetime) -> None:
        self.revoked_session_token = session_token
        self.revoked_session_at = now

    async def append_audit(
        self,
        actor_user_id: str | None,
        action: str,
        resource_type: str,
        resource_id: str,
        outcome: str,
        now: datetime,
    ) -> None:
        self.last_audit = (actor_user_id, action, resource_type, resource_id, outcome, now)

    async def set_runner_state(
        self, runner_id: str, state: str, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        self.runner_state = (runner_id, state, actor_user_id, now)
        return {
            "id": runner_id,
            "name": "runner-a",
            "enabled": state != "revoke",
            "draining": state != "enable",
            "status": "offline" if state == "revoke" else "online",
            "labels": ["docker"],
            "last_seen_at": NOW,
        }


class _FakeWorkflowRuntime:
    def __init__(self) -> None:
        self.resumed_with: tuple[str, dict[str, object]] | None = None

    async def run(self, workflow_run_id: str, state_updates: dict[str, object]) -> dict[str, object]:
        self.resumed_with = (workflow_run_id, state_updates)
        status = "merging" if state_updates.get("human_approved") else "repairing"
        return {"project_id": "project-1", "status": status}


@unittest.skipIf(grpc is None, "grpcio is not installed")
class _UnsupportedListProjects(FakeControlPlane):
    """Fully implements ControlPlane but declines list_projects explicitly.

    Distinguishes "this control plane intentionally doesn't support this
    operation" (a NotImplementedError raised from a real, typed method) from
    a renamed/missing method, which is now a `make typecheck` failure instead
    of a silent runtime UNIMPLEMENTED.
    """

    async def list_projects(self) -> list[dict[str, object]]:
        raise NotImplementedError("list_projects")


@unittest.skipIf(grpc is None, "grpcio is not installed")
class ControlPlaneGrpcTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.control_plane = FakeControlPlane()
        self.server = grpc.aio.server()
        self.runner_service = register_services(self.server, self.control_plane, now=lambda: NOW)
        port = self.server.add_insecure_port("127.0.0.1:0")
        await self.server.start()
        self.channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
        self.client = control_plane_pb2_grpc.ControlPlaneStub(self.channel)
        self.runner_client = runner_control_pb2_grpc.RunnerControlStub(self.channel)

    async def asyncTearDown(self) -> None:
        await self.channel.close()
        await self.server.stop(0)

    async def test_registers_control_plane_and_runner_control_services(self) -> None:
        response = await self.client.Login(
            control_plane_pb2.LoginRequest(username="admin", password="correct")
        )
        self.assertEqual(response.session_token, "session-token")
        self.assertEqual(response.user_id, "user-1")
        self.assertEqual(response.csrf_token, "csrf-token")
        self.assertIsNotNone(self.runner_client.Connect)

    async def test_who_am_i_returns_the_authenticated_session(self) -> None:
        response = await self.client.WhoAmI(
            control_plane_pb2.WhoAmIRequest(), metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(response.user_id, "00000000-0000-0000-0000-000000000099")
        self.assertEqual(response.username, "admin")
        self.assertEqual(response.role, "admin")
        with self.assertRaises(grpc.aio.AioRpcError) as anonymous:
            await self.client.WhoAmI(control_plane_pb2.WhoAmIRequest())
        self.assertEqual(anonymous.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_update_account_updates_the_profile(self) -> None:
        response = await self.client.UpdateAccount(
            control_plane_pb2.UpdateAccountRequest(
                current_password="correct",
                new_password="",
                new_email="new@example.com",
                display_name="New Name",
            ),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(response.user_id, "00000000-0000-0000-0000-000000000099")
        self.assertEqual(response.email, "new@example.com")
        self.assertEqual(response.display_name, "New Name")
        with self.assertRaises(grpc.aio.AioRpcError) as wrong_password:
            await self.client.UpdateAccount(
                control_plane_pb2.UpdateAccountRequest(current_password="wrong", new_password="x1Y!abcdef"),
                metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
            )
        self.assertEqual(wrong_password.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)
        with self.assertRaises(grpc.aio.AioRpcError) as anonymous:
            await self.client.UpdateAccount(control_plane_pb2.UpdateAccountRequest())
        self.assertEqual(anonymous.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_maps_typed_responses_and_validates_runner_token_requests(self) -> None:
        projects = await self.client.ListProjects(
            control_plane_pb2.ListProjectsRequest(), metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual([(project.id, project.name, project.enabled) for project in projects.projects], [("project-1", "Example", True)])
        self.assertEqual(
            [
                (
                    projects.projects[0].repository_mode,
                    projects.projects[0].repository_url,
                    projects.projects[0].local_repository_path,
                    projects.projects[0].default_branch,
                    list(projects.projects[0].required_runner_labels),
                )
            ],
            [("managed_clone", "https://example.test/repo.git", "", "main", ["docker", "linux"])],
        )
        created = await self.client.CreateProject(
            control_plane_pb2.CreateProjectRequest(
                project=control_plane_pb2.ProjectConfiguration(
                    name="Created",
                    repository_mode="managed_clone",
                    repository_url="https://example.test/repo.git",
                    default_branch="main",
                    required_runner_labels=["docker"],
                )
            ),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual((created.project.id, created.project.name, created.project.enabled), ("project-2", "Created", True))
        updated = await self.client.UpdateProject(
            control_plane_pb2.UpdateProjectRequest(
                project_id="project-2",
                project=control_plane_pb2.ProjectConfiguration(
                    name="Renamed",
                    repository_mode="existing_path",
                    local_repository_path="/repositories/example",
                    default_branch="main",
                    required_runner_labels=["linux"],
                ),
            ),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(updated.project.name, "Renamed")
        disabled = await self.client.SetProjectEnabled(
            control_plane_pb2.SetProjectEnabledRequest(project_id="project-2", enabled=False),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertFalse(disabled.project.enabled)
        token = await self.client.CreateRunnerRegistrationToken(
            control_plane_pb2.CreateRunnerRegistrationTokenRequest(allowed_labels=["docker", "linux"]),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(token.token, "runner-token")
        self.assertEqual(token.expires_at, (NOW + timedelta(minutes=15)).isoformat())
        self.assertEqual(self.control_plane.token_labels, ("docker", "linux"))
        self.assertEqual(self.control_plane.token_actor, "00000000-0000-0000-0000-000000000099")
        tokens = await self.client.ListRunnerRegistrationTokens(
            control_plane_pb2.ListRunnerRegistrationTokensRequest(),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(tokens.tokens[0].id, "token-1")
        revoked = await self.client.RevokeRunnerRegistrationToken(
            control_plane_pb2.RevokeRunnerRegistrationTokenRequest(token_id="token-1"),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(revoked.token.id, "token-1")
        self.assertEqual(self.control_plane.revoked_token[1], "00000000-0000-0000-0000-000000000099")
        workflows = await self.client.ListWorkflows(
            control_plane_pb2.ListWorkflowsRequest(), metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(workflows.workflows[0].phase, "prepare_workspace")
        # The list carries the same detail the console renders per row.
        self.assertEqual(workflows.workflows[0].issue_title, "Prepare the workspace")
        self.assertEqual(workflows.workflows[0].branch_name, "agent/41/prepare")
        self.assertEqual(workflows.workflows[0].total_agent_executions, 1)
        workflow = await self.client.GetWorkflow(
            control_plane_pb2.GetWorkflowRequest(workflow_run_id="workflow-1"),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(workflow.workflow.issue_title, "Fix it")
        events = await self.client.ListWorkflowEvents(
            control_plane_pb2.ListWorkflowEventsRequest(workflow_run_id="workflow-1", after_id=9, limit=2),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(events.events[0].event_type, "log")
        self.assertEqual(self.control_plane.event_request, ("workflow-1", 9, 2))
        with self.assertRaises(grpc.aio.AioRpcError) as missing:
            await self.client.GetWorkflow(control_plane_pb2.GetWorkflowRequest(workflow_run_id="missing"))
        self.assertEqual(missing.exception.code(), grpc.StatusCode.UNAUTHENTICATED)
        with self.assertRaises(grpc.aio.AioRpcError) as invalid:
            await self.client.CreateRunnerRegistrationToken(
                control_plane_pb2.CreateRunnerRegistrationTokenRequest(allowed_labels=["docker", "docker"]),
                metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
            )
        self.assertEqual(invalid.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    async def test_stream_events_forwards_workflow_notifications(self) -> None:
        stream = self.client.StreamEvents(
            control_plane_pb2.StreamEventsRequest(last_event_id="10"),
            metadata=(("x-loop-session", "admin-session"),),
        )
        event = await stream.read()
        self.assertEqual(event.id, "11")
        self.assertEqual(event.event_type, "workflow")
        self.assertEqual(event.workflow.status, "blocked")
        self.assertEqual(self.control_plane.last_event_id, "10")

    async def test_requires_session_and_administrator_for_control_operations(self) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as anonymous:
            await self.client.ListProjects(control_plane_pb2.ListProjectsRequest())
        self.assertEqual(anonymous.exception.code(), grpc.StatusCode.UNAUTHENTICATED)
        with self.assertRaises(grpc.aio.AioRpcError) as missing_csrf:
            await self.client.CreateRunnerRegistrationToken(
                control_plane_pb2.CreateRunnerRegistrationTokenRequest(allowed_labels=["docker"]),
                metadata=(("x-loop-session", "admin-session"),),
            )
        self.assertEqual(missing_csrf.exception.code(), grpc.StatusCode.UNAUTHENTICATED)
        with self.assertRaises(grpc.aio.AioRpcError) as viewer:
            await self.client.CreateRunnerRegistrationToken(
                control_plane_pb2.CreateRunnerRegistrationTokenRequest(allowed_labels=["docker"]),
                metadata=(("x-loop-session", "viewer-session"), ("x-loop-csrf", "csrf-token")),
            )
        self.assertEqual(viewer.exception.code(), grpc.StatusCode.PERMISSION_DENIED)

    async def test_list_queue_returns_entries_and_enforces_limits(self) -> None:
        response = await self.client.ListQueue(
            control_plane_pb2.ListQueueRequest(limit=10),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(self.control_plane.queue_request, ("list_queue", 10))
        self.assertEqual(len(response.entries), 1)
        self.assertEqual(response.entries[0].project_id, "project-1")
        self.assertEqual(response.entries[0].priority, 100)
        self.assertEqual(response.entries[0].blocked_reason, "")

        with self.assertRaises(grpc.aio.AioRpcError) as too_many:
            await self.client.ListQueue(
                control_plane_pb2.ListQueueRequest(limit=101),
                metadata=(("x-loop-session", "admin-session"),),
            )
        self.assertEqual(too_many.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

        with self.assertRaises(grpc.aio.AioRpcError) as anonymous:
            await self.client.ListQueue(control_plane_pb2.ListQueueRequest())
        self.assertEqual(anonymous.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_runner_controls_require_admin_csrf_and_persist_actor(self) -> None:
        session = await self.runner_service._sessions.connect("runner-1")
        response = await self.client.SetRunnerState(
            control_plane_pb2.SetRunnerStateRequest(runner_id="runner-1", state="drain"),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertTrue(response.runner.draining)
        self.assertIsNotNone((await session.next_message()).drain)
        self.assertEqual(
            self.control_plane.runner_state,
            ("runner-1", "drain", "00000000-0000-0000-0000-000000000099", NOW),
        )
        with self.assertRaises(grpc.aio.AioRpcError) as viewer:
            await self.client.SetRunnerState(
                control_plane_pb2.SetRunnerStateRequest(runner_id="runner-1", state="drain"),
                metadata=(("x-loop-session", "viewer-session"), ("x-loop-csrf", "csrf-token")),
            )
        self.assertEqual(viewer.exception.code(), grpc.StatusCode.PERMISSION_DENIED)
        with self.assertRaises(grpc.aio.AioRpcError) as missing_csrf:
            await self.client.SetRunnerState(
                control_plane_pb2.SetRunnerStateRequest(runner_id="runner-1", state="drain"),
                metadata=(("x-loop-session", "admin-session"),),
            )
        self.assertEqual(missing_csrf.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_logout_revokes_session_and_records_audit(self) -> None:
        await self.client.Logout(
            control_plane_pb2.LogoutRequest(),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(self.control_plane.revoked_session_token, "admin-session")
        self.assertEqual(self.control_plane.revoked_session_at, NOW)
        self.assertEqual(self.control_plane.last_audit[1], "user.logout")

    async def test_logout_idempotent_without_session(self) -> None:
        await self.client.Logout(control_plane_pb2.LogoutRequest())
        self.assertIsNone(self.control_plane.revoked_session_token)

    async def test_logout_does_not_revoke_unknown_session(self) -> None:
        await self.client.Logout(
            control_plane_pb2.LogoutRequest(),
            metadata=(("x-loop-session", "unknown-session"),),
        )
        self.assertIsNone(self.control_plane.revoked_session_token)

    async def test_maps_login_failures_and_declined_capabilities_to_typed_errors(self) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as rejected:
            await self.client.Login(control_plane_pb2.LoginRequest(username="admin", password="wrong"))
        self.assertEqual(rejected.exception.code(), grpc.StatusCode.UNAUTHENTICATED)
        missing_server = grpc.aio.server()
        register_services(missing_server, _UnsupportedListProjects())
        port = missing_server.add_insecure_port("127.0.0.1:0")
        await missing_server.start()
        channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
        missing_client = control_plane_pb2_grpc.ControlPlaneStub(channel)
        try:
            with self.assertRaises(grpc.aio.AioRpcError) as unavailable:
                await missing_client.ListProjects(
                    control_plane_pb2.ListProjectsRequest(),
                    metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
                )
            self.assertEqual(unavailable.exception.code(), grpc.StatusCode.UNIMPLEMENTED)
        finally:
            await channel.close()
            await missing_server.stop(0)


@unittest.skipIf(grpc is None, "grpcio is not installed")
class SubmitHumanDecisionGrpcTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        self.control_plane = FakeControlPlane()
        self.workflow_runtime = _FakeWorkflowRuntime()
        self.server = grpc.aio.server()
        register_services(self.server, self.control_plane, now=lambda: NOW, workflow_runtime=self.workflow_runtime)
        port = self.server.add_insecure_port("127.0.0.1:0")
        await self.server.start()
        self.channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
        self.client = control_plane_pb2_grpc.ControlPlaneStub(self.channel)

    async def asyncTearDown(self) -> None:
        await self.channel.close()
        await self.server.stop(0)

    async def test_approval_persists_the_decision_and_resumes_the_graph_toward_merge(self) -> None:
        response = await self.client.SubmitHumanDecision(
            control_plane_pb2.SubmitHumanDecisionRequest(
                workflow_run_id="workflow-waiting", decision="approved", comment="ship it",
            ),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(
            self.control_plane.recorded_decision,
            ("workflow-waiting", "approved", "ship it", "00000000-0000-0000-0000-000000000099"),
        )
        self.assertEqual(self.workflow_runtime.resumed_with, ("workflow-waiting", {
            "human_approved": True, "human_changes_requested": False,
        }))
        self.assertEqual(response.workflow.status, "merging")

    async def test_requesting_changes_resumes_the_graph_toward_repair(self) -> None:
        response = await self.client.SubmitHumanDecision(
            control_plane_pb2.SubmitHumanDecisionRequest(
                workflow_run_id="workflow-waiting", decision="changes_requested",
            ),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(self.workflow_runtime.resumed_with, ("workflow-waiting", {
            "human_approved": False, "human_changes_requested": True,
        }))
        self.assertEqual(response.workflow.status, "repairing")

    async def test_operator_controls_require_admin_csrf_and_persist_actor(self) -> None:
        response = await self.client.BlockWorkflow(
            control_plane_pb2.BlockWorkflowRequest(workflow_run_id="workflow-other", reason="GitHub unavailable"),
            metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
        )
        self.assertEqual(response.workflow.status, "blocked")
        self.assertEqual(
            self.control_plane.workflow_control,
            ("block", "workflow-other", "GitHub unavailable", "00000000-0000-0000-0000-000000000099", NOW),
        )
        with self.assertRaises(grpc.aio.AioRpcError) as viewer:
            await self.client.CancelWorkflow(
                control_plane_pb2.CancelWorkflowRequest(workflow_run_id="workflow-other"),
                metadata=(("x-loop-session", "viewer-session"), ("x-loop-csrf", "csrf-token")),
            )
        self.assertEqual(viewer.exception.code(), grpc.StatusCode.PERMISSION_DENIED)
        with self.assertRaises(grpc.aio.AioRpcError) as missing_csrf:
            await self.client.RetryWorkflow(
                control_plane_pb2.RetryWorkflowRequest(workflow_run_id="workflow-other"),
                metadata=(("x-loop-session", "admin-session"),),
            )
        self.assertEqual(missing_csrf.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_rejects_an_invalid_decision_value(self) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as invalid:
            await self.client.SubmitHumanDecision(
                control_plane_pb2.SubmitHumanDecisionRequest(workflow_run_id="workflow-waiting", decision="maybe"),
                metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
            )
        self.assertEqual(invalid.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    async def test_requires_a_session(self) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as unauthenticated:
            await self.client.SubmitHumanDecision(
                control_plane_pb2.SubmitHumanDecisionRequest(workflow_run_id="workflow-waiting", decision="approved"),
            )
        self.assertEqual(unauthenticated.exception.code(), grpc.StatusCode.UNAUTHENTICATED)

    async def test_rejects_a_workflow_not_awaiting_approval(self) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as failed_precondition:
            await self.client.SubmitHumanDecision(
                control_plane_pb2.SubmitHumanDecisionRequest(workflow_run_id="workflow-other", decision="approved"),
                metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
            )
        self.assertEqual(failed_precondition.exception.code(), grpc.StatusCode.FAILED_PRECONDITION)

    async def test_returns_unimplemented_without_a_workflow_runtime(self) -> None:
        server = grpc.aio.server()
        register_services(server, self.control_plane, now=lambda: NOW)
        port = server.add_insecure_port("127.0.0.1:0")
        await server.start()
        channel = grpc.aio.insecure_channel(f"127.0.0.1:{port}")
        client = control_plane_pb2_grpc.ControlPlaneStub(channel)
        try:
            with self.assertRaises(grpc.aio.AioRpcError) as unimplemented:
                await client.SubmitHumanDecision(
                    control_plane_pb2.SubmitHumanDecisionRequest(
                        workflow_run_id="workflow-waiting", decision="approved"
                    ),
                    metadata=(("x-loop-session", "admin-session"), ("x-loop-csrf", "csrf-token")),
                )
            self.assertEqual(unimplemented.exception.code(), grpc.StatusCode.UNIMPLEMENTED)
        finally:
            await channel.close()
            await server.stop(0)


if __name__ == "__main__":
    unittest.main()
