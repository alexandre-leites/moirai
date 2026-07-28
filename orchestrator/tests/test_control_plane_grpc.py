from datetime import UTC, datetime, timedelta
import unittest

try:
    import grpc

    from moirai.main import register_services
    from moirai.persistence.authentication import AuthenticatedSession, SessionCredentials
    from proto import control_plane_pb2, control_plane_pb2_grpc, runner_control_pb2_grpc
except ModuleNotFoundError:
    grpc = None


NOW = datetime(2026, 1, 1, tzinfo=UTC)


class FakeControlPlane:
    """Implements moirai.grpc.protocol.ControlPlane for tests."""

    def __init__(self) -> None:
        self.token_labels: tuple[str, ...] | None = None
        self.token_expiry: datetime | None = None

    async def login(self, username: str, password: str, now: datetime) -> SessionCredentials:
        if username != "admin" or password != "correct":
            raise PermissionError()
        self.login_at = now
        return SessionCredentials(session_token="session-token", csrf_token="csrf-token", user_id="user-1", expires_at=now)

    async def validate_session(
        self, session_token: str, csrf_token: str | None, now: datetime, require_csrf: bool
    ) -> AuthenticatedSession:
        del csrf_token, require_csrf
        if session_token == "admin-session":
            return AuthenticatedSession(
                id="session-1", user_id="00000000-0000-0000-0000-000000000099", username="admin", role="admin", expires_at=now
            )
        if session_token == "viewer-session":
            return AuthenticatedSession(
                id="session-2", user_id="00000000-0000-0000-0000-000000000098", username="viewer", role="viewer", expires_at=now
            )
        raise PermissionError()

    async def list_projects(self) -> list[dict[str, object]]:
        return [{"id": "project-1", "name": "Example", "enabled": True}]

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
    ) -> dict[str, object]:
        del repository_mode, repository_url, local_repository_path, default_branch, labels, now, actor_user_id
        return {"id": "project-2", "name": name, "enabled": True}

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
    ) -> dict[str, object]:
        del repository_mode, repository_url, local_repository_path, default_branch, labels, now, actor_user_id
        return {"id": project_id, "name": name, "enabled": True}

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

    async def list_workflows(self) -> list[dict[str, str]]:
        return [
            {
                "id": "workflow-1",
                "project_id": "project-1",
                "status": "preparing",
                "phase": "prepare_workspace",
            }
        ]

    async def list_runners(self) -> list[dict[str, object]]:
        return []


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
        register_services(self.server, self.control_plane, now=lambda: NOW)
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
        self.assertIsNotNone(self.runner_client.Connect)

    async def test_maps_typed_responses_and_validates_runner_token_requests(self) -> None:
        projects = await self.client.ListProjects(
            control_plane_pb2.ListProjectsRequest(), metadata=(("x-loop-session", "admin-session"),)
        )
        self.assertEqual([(project.id, project.name, project.enabled) for project in projects.projects], [("project-1", "Example", True)])
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
            metadata=(("x-loop-session", "admin-session"),),
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
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(updated.project.name, "Renamed")
        disabled = await self.client.SetProjectEnabled(
            control_plane_pb2.SetProjectEnabledRequest(project_id="project-2", enabled=False),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertFalse(disabled.project.enabled)
        token = await self.client.CreateRunnerRegistrationToken(
            control_plane_pb2.CreateRunnerRegistrationTokenRequest(allowed_labels=["docker", "linux"]),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(token.token, "runner-token")
        self.assertEqual(token.expires_at, (NOW + timedelta(minutes=15)).isoformat())
        self.assertEqual(self.control_plane.token_labels, ("docker", "linux"))
        self.assertEqual(self.control_plane.token_actor, "00000000-0000-0000-0000-000000000099")
        tokens = await self.client.ListRunnerRegistrationTokens(
            control_plane_pb2.ListRunnerRegistrationTokensRequest(),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(tokens.tokens[0].id, "token-1")
        revoked = await self.client.RevokeRunnerRegistrationToken(
            control_plane_pb2.RevokeRunnerRegistrationTokenRequest(token_id="token-1"),
            metadata=(("x-loop-session", "admin-session"),),
        )
        self.assertEqual(revoked.token.id, "token-1")
        self.assertEqual(self.control_plane.revoked_token[1], "00000000-0000-0000-0000-000000000099")
        workflows = await self.client.ListWorkflows(
            control_plane_pb2.ListWorkflowsRequest(), metadata=(("x-loop-session", "admin-session"),)
        )
        self.assertEqual(workflows.workflows[0].phase, "prepare_workspace")
        with self.assertRaises(grpc.aio.AioRpcError) as invalid:
            await self.client.CreateRunnerRegistrationToken(
                control_plane_pb2.CreateRunnerRegistrationTokenRequest(allowed_labels=["docker", "docker"]),
                metadata=(("x-loop-session", "admin-session"),),
            )
        self.assertEqual(invalid.exception.code(), grpc.StatusCode.INVALID_ARGUMENT)

    async def test_requires_session_and_administrator_for_control_operations(self) -> None:
        with self.assertRaises(grpc.aio.AioRpcError) as anonymous:
            await self.client.ListProjects(control_plane_pb2.ListProjectsRequest())
        self.assertEqual(anonymous.exception.code(), grpc.StatusCode.UNAUTHENTICATED)
        with self.assertRaises(grpc.aio.AioRpcError) as viewer:
            await self.client.CreateRunnerRegistrationToken(
                control_plane_pb2.CreateRunnerRegistrationTokenRequest(allowed_labels=["docker"]),
                metadata=(("x-loop-session", "viewer-session"),),
            )
        self.assertEqual(viewer.exception.code(), grpc.StatusCode.PERMISSION_DENIED)

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
                    metadata=(("x-loop-session", "admin-session"),),
                )
            self.assertEqual(unavailable.exception.code(), grpc.StatusCode.UNIMPLEMENTED)
        finally:
            await channel.close()
            await missing_server.stop(0)


if __name__ == "__main__":
    unittest.main()
