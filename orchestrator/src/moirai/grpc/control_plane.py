from __future__ import annotations

from collections.abc import Callable, Iterable
from datetime import UTC, datetime, timedelta
import inspect
import logging
from typing import Any

import grpc

from proto import control_plane_pb2, control_plane_pb2_grpc


_SESSION_METADATA_KEY = "x-loop-session"


async def _await_if_needed(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


class ControlPlaneService(control_plane_pb2_grpc.ControlPlaneServicer):
    def __init__(
        self,
        control_plane: Any,
        now: Callable[[], datetime] | None = None,
        registration_token_ttl: timedelta = timedelta(minutes=15),
        workflow_runtime: Any | None = None,
    ) -> None:
        if registration_token_ttl <= timedelta():
            raise ValueError("registration token TTL must be positive")
        self._control_plane = control_plane
        self._now = now or (lambda: datetime.now(UTC))
        self._registration_token_ttl = registration_token_ttl
        self._workflow_runtime = workflow_runtime

    async def Login(
        self,
        request: control_plane_pb2.LoginRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.LoginResponse:
        username = request.username.strip()
        if not username or not request.password:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "login request is invalid")
        try:
            result = await self._invoke("login", username, request.password, self._now())
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "login is unavailable")
        except (PermissionError, ValueError):
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "login was rejected")
        session_token, user_id = _login_values(result)
        if not session_token or not user_id:
            await context.abort(grpc.StatusCode.INTERNAL, "login could not be completed")
        return control_plane_pb2.LoginResponse(session_token=session_token, user_id=user_id)

    async def WhoAmI(
        self,
        request: control_plane_pb2.WhoAmIRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.WhoAmIResponse:
        del request
        session = await self._require_session(context)
        return control_plane_pb2.WhoAmIResponse(
            user_id=_text(_value(session, "user_id")),
            username=_text(_value(session, "username")),
            role=_text(_value(session, "role")),
        )

    async def ListProjects(
        self,
        request: control_plane_pb2.ListProjectsRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListProjectsResponse:
        del request
        await self._require_session(context)
        try:
            projects = await self._invoke("list_projects")
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "projects are unavailable")
        return control_plane_pb2.ListProjectsResponse(
            projects=[_project_message(project) for project in _iterable(projects, "projects")]
        )

    async def CreateProject(
        self,
        request: control_plane_pb2.CreateProjectRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.CreateProjectResponse:
        session = await self._require_session(context, administrator=True)
        try:
            project = await self._invoke(
                "create_project",
                *_project_arguments(request.project),
                self._now(),
                _text(_value(session, "user_id")) or None,
            )
        except ValueError:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project configuration is invalid")
        return control_plane_pb2.CreateProjectResponse(project=_project_message(project))

    async def UpdateProject(
        self,
        request: control_plane_pb2.UpdateProjectRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.UpdateProjectResponse:
        session = await self._require_session(context, administrator=True)
        if not request.project_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project ID is required")
        try:
            project = await self._invoke(
                "update_project",
                request.project_id,
                *_project_arguments(request.project),
                self._now(),
                _text(_value(session, "user_id")) or None,
            )
        except ValueError:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project configuration is invalid")
        return control_plane_pb2.UpdateProjectResponse(project=_project_message(project))

    async def SetProjectEnabled(
        self,
        request: control_plane_pb2.SetProjectEnabledRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SetProjectEnabledResponse:
        session = await self._require_session(context, administrator=True)
        if not request.project_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project ID is required")
        try:
            project = await self._invoke(
                "set_project_enabled",
                request.project_id,
                request.enabled,
                self._now(),
                _text(_value(session, "user_id")) or None,
            )
        except ValueError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "project is unknown")
        return control_plane_pb2.SetProjectEnabledResponse(project=_project_message(project))

    async def CreateRunnerRegistrationToken(
        self,
        request: control_plane_pb2.CreateRunnerRegistrationTokenRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.CreateRunnerRegistrationTokenResponse:
        session = await self._require_session(context, administrator=True)
        labels = tuple(label.strip() for label in request.allowed_labels)
        if not labels or any(not label for label in labels) or len(set(labels)) != len(labels):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "runner token request is invalid")
        expires_at = self._now() + self._registration_token_ttl
        try:
            token = await self._invoke(
                "create_registration_token",
                labels,
                expires_at,
                _text(_value(session, "user_id")) or None,
                self._now(),
            )
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "runner token administration is unavailable")
        except (PermissionError, ValueError):
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, "runner token could not be created")
        if not isinstance(token, str) or not token:
            await context.abort(grpc.StatusCode.INTERNAL, "runner token could not be created")
        return control_plane_pb2.CreateRunnerRegistrationTokenResponse(
            token=token,
            expires_at=expires_at.isoformat(),
        )

    async def ListRunnerRegistrationTokens(
        self,
        request: control_plane_pb2.ListRunnerRegistrationTokensRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListRunnerRegistrationTokensResponse:
        del request
        await self._require_session(context, administrator=True)
        try:
            tokens = await self._invoke("list_registration_tokens")
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "runner token administration is unavailable")
        return control_plane_pb2.ListRunnerRegistrationTokensResponse(
            tokens=[_registration_token_message(token) for token in _iterable(tokens, "registration tokens")]
        )

    async def RevokeRunnerRegistrationToken(
        self,
        request: control_plane_pb2.RevokeRunnerRegistrationTokenRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.RevokeRunnerRegistrationTokenResponse:
        session = await self._require_session(context, administrator=True)
        if not request.token_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "registration token ID is required")
        try:
            token = await self._invoke(
                "revoke_registration_token",
                request.token_id,
                _text(_value(session, "user_id")) or None,
                self._now(),
            )
        except ValueError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "registration token is unknown or inactive")
        return control_plane_pb2.RevokeRunnerRegistrationTokenResponse(
            token=_registration_token_message(token)
        )

    async def ListWorkflows(
        self,
        request: control_plane_pb2.ListWorkflowsRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListWorkflowsResponse:
        del request
        await self._require_session(context)
        try:
            workflows = await self._invoke("list_workflows")
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "workflows are unavailable")
        return control_plane_pb2.ListWorkflowsResponse(
            workflows=[_workflow_message(workflow) for workflow in _iterable(workflows, "workflows")]
        )

    async def ListRunners(
        self,
        request: control_plane_pb2.ListRunnersRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListRunnersResponse:
        del request
        await self._require_session(context)
        try:
            runners = await self._invoke("list_runners")
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "runners are unavailable")
        return control_plane_pb2.ListRunnersResponse(
            runners=[_runner_message(runner) for runner in _iterable(runners, "runners")]
        )

    async def SubmitHumanDecision(
        self,
        request: control_plane_pb2.SubmitHumanDecisionRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SubmitHumanDecisionResponse:
        session = await self._require_session(context)
        decision = request.decision
        if not request.workflow_run_id or decision not in ("approved", "changes_requested"):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "human decision request is invalid")
        try:
            await self._invoke(
                "record_human_decision",
                request.workflow_run_id,
                decision,
                request.comment or None,
                _text(_value(session, "user_id")) or None,
                self._now(),
            )
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "human approval is unavailable")
        except ValueError:
            await context.abort(
                grpc.StatusCode.FAILED_PRECONDITION, "workflow run is not awaiting human approval"
            )
        if self._workflow_runtime is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "workflow resumption is unavailable")
        try:
            state = await self._workflow_runtime.run(
                request.workflow_run_id,
                {
                    "human_approved": decision == "approved",
                    "human_changes_requested": decision == "changes_requested",
                },
            )
        except Exception:
            logging.getLogger(__name__).exception("workflow resumption after human decision failed")
            await context.abort(grpc.StatusCode.INTERNAL, "workflow could not be resumed")
        return control_plane_pb2.SubmitHumanDecisionResponse(
            workflow=control_plane_pb2.Workflow(
                id=request.workflow_run_id,
                project_id=_text(state.get("project_id")),
                status=_text(state.get("status")),
                phase=_text(state.get("status")),
            )
        )

    async def _require_session(
        self, context: grpc.aio.ServicerContext, administrator: bool = False
    ) -> Any:
        token = next(
            (value for key, value in (context.invocation_metadata() or ()) if key.lower() == _SESSION_METADATA_KEY),
            "",
        )
        if not token:
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "session is required")
        try:
            session = await self._invoke("validate_session", token, None, self._now(), False)
        except (NotImplementedError, PermissionError, ValueError):
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "session is invalid")
        if administrator and _text(_value(session, "role")) != "admin":
            await context.abort(grpc.StatusCode.PERMISSION_DENIED, "administrator access is required")
        return session

    async def _invoke(self, method_name: str, *args: Any) -> Any:
        method = getattr(self._control_plane, method_name, None)
        if method is None:
            raise NotImplementedError(method_name)
        return await _await_if_needed(method(*args))


def _login_values(value: Any) -> tuple[str, str]:
    if isinstance(value, tuple) and len(value) == 2:
        return _text(value[0]), _text(value[1])
    return _text(_value(value, "session_token")), _text(_value(value, "user_id"))


def _project_arguments(project: Any) -> tuple[str, str, str | None, str | None, str, tuple[str, ...]]:
    repository_url = _text(_value(project, "repository_url")) or None
    local_repository_path = _text(_value(project, "local_repository_path")) or None
    return (
        _text(_value(project, "name")),
        _text(_value(project, "repository_mode")),
        repository_url,
        local_repository_path,
        _text(_value(project, "default_branch")),
        tuple(_text(label) for label in _iterable(_value(project, "required_runner_labels"), "runner labels")),
    )


def _project_message(project: Any) -> control_plane_pb2.Project:
    return control_plane_pb2.Project(
        id=_text(_value(project, "id")),
        name=_text(_value(project, "name")),
        enabled=bool(_value(project, "enabled")),
    )


def _registration_token_message(token: Any) -> control_plane_pb2.RunnerRegistrationToken:
    return control_plane_pb2.RunnerRegistrationToken(
        id=_text(_value(token, "id")),
        allowed_labels=[_text(label) for label in _iterable(_value(token, "allowed_labels"), "token labels")],
        created_at=_timestamp(_value(token, "created_at")),
        expires_at=_timestamp(_value(token, "expires_at")),
        used_at=_timestamp(_value(token, "used_at")),
        revoked_at=_timestamp(_value(token, "revoked_at")),
    )


def _timestamp(value: Any) -> str:
    return value.isoformat() if isinstance(value, datetime) else _text(value)


def _runner_message(runner: Any) -> control_plane_pb2.Runner:
    return control_plane_pb2.Runner(
        id=_text(_value(runner, "id")),
        name=_text(_value(runner, "name")),
        enabled=bool(_value(runner, "enabled")),
        draining=bool(_value(runner, "draining")),
        status=_text(_value(runner, "status")),
        labels=[_text(label) for label in _iterable(_value(runner, "labels"), "runner labels")],
        last_seen_at=_timestamp(_value(runner, "last_seen_at")),
    )

def _workflow_message(workflow: Any) -> control_plane_pb2.Workflow:
    status = _text(_value(workflow, "status"))
    return control_plane_pb2.Workflow(
        id=_text(_value(workflow, "id")),
        project_id=_text(_value(workflow, "project_id")),
        status=status,
        phase=_text(_value(workflow, "phase", default=status)),
    )


def _text(value: Any) -> str:
    if value is None:
        return ""
    return str(value)


def _value(value: Any, field: str, default: Any = None) -> Any:
    if isinstance(value, dict):
        return value.get(field, default)
    return getattr(value, field, default)


def _iterable(value: Any, name: str) -> Iterable[Any]:
    if isinstance(value, (str, bytes, dict)):
        raise TypeError(f"{name} must be an iterable of records")
    try:
        return iter(value)
    except TypeError as error:
        raise TypeError(f"{name} must be an iterable of records") from error
