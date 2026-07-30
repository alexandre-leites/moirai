from __future__ import annotations

import logging
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from typing import Any

import grpc

from moirai.grpc.protocol import (
    ControlPlane,
    ProjectRecord,
    RegistrationTokenRecord,
    RunnerRecord,
    WorkflowDetailRecord,
    WorkflowEventRecord,
    WorkflowRecord,
)
from moirai.persistence.authentication import AuthenticatedSession
from proto import control_plane_pb2, control_plane_pb2_grpc

_SESSION_METADATA_KEY = "x-loop-session"
_CSRF_METADATA_KEY = "x-loop-csrf"


class ControlPlaneService(control_plane_pb2_grpc.ControlPlaneServicer):
    def __init__(
        self,
        control_plane: ControlPlane,
        now: Callable[[], datetime] | None = None,
        registration_token_ttl: timedelta = timedelta(minutes=15),
        workflow_runtime: Any | None = None,
        runner_control: Any | None = None,
    ) -> None:
        if registration_token_ttl <= timedelta():
            raise ValueError("registration token TTL must be positive")
        self._control_plane = control_plane
        self._now = now or (lambda: datetime.now(UTC))
        self._registration_token_ttl = registration_token_ttl
        self._workflow_runtime = workflow_runtime
        self._runner_control = runner_control

    async def Login(
        self,
        request: control_plane_pb2.LoginRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.LoginResponse:
        username = request.username.strip()
        if not username or not request.password:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "login request is invalid")
        try:
            credentials = await self._control_plane.login(username, request.password, self._now())
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "login is unavailable")
        except (PermissionError, ValueError):
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "login was rejected")
        if not credentials.session_token or not credentials.user_id:
            await context.abort(grpc.StatusCode.INTERNAL, "login could not be completed")
        return control_plane_pb2.LoginResponse(
            session_token=credentials.session_token,
            user_id=credentials.user_id,
            csrf_token=credentials.csrf_token,
        )

    async def WhoAmI(
        self,
        request: control_plane_pb2.WhoAmIRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.WhoAmIResponse:
        del request
        session = await self._require_session(context)
        return control_plane_pb2.WhoAmIResponse(
            user_id=session.user_id,
            username=session.username,
            role=session.role,
        )

    async def ListProjects(
        self,
        request: control_plane_pb2.ListProjectsRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListProjectsResponse:
        del request
        await self._require_session(context)
        try:
            projects = await self._control_plane.list_projects()
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "projects are unavailable")
        return control_plane_pb2.ListProjectsResponse(
            projects=[_project_message(project) for project in projects]
        )

    async def CreateProject(
        self,
        request: control_plane_pb2.CreateProjectRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.CreateProjectResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        try:
            project = await self._control_plane.create_project(
                *_project_arguments(request.project),
                self._now(),
                session.user_id or None,
            )
        except ValueError:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project configuration is invalid")
        return control_plane_pb2.CreateProjectResponse(project=_project_message(project))

    async def UpdateProject(
        self,
        request: control_plane_pb2.UpdateProjectRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.UpdateProjectResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        if not request.project_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project ID is required")
        try:
            project = await self._control_plane.update_project(
                request.project_id,
                *_project_arguments(request.project),
                self._now(),
                session.user_id or None,
            )
        except ValueError:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project configuration is invalid")
        return control_plane_pb2.UpdateProjectResponse(project=_project_message(project))

    async def SetProjectEnabled(
        self,
        request: control_plane_pb2.SetProjectEnabledRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SetProjectEnabledResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        if not request.project_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project ID is required")
        try:
            project = await self._control_plane.set_project_enabled(
                request.project_id,
                request.enabled,
                self._now(),
                session.user_id or None,
            )
        except ValueError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "project is unknown")
        return control_plane_pb2.SetProjectEnabledResponse(project=_project_message(project))

    async def CreateRunnerRegistrationToken(
        self,
        request: control_plane_pb2.CreateRunnerRegistrationTokenRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.CreateRunnerRegistrationTokenResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        labels = tuple(label.strip() for label in request.allowed_labels)
        if not labels or any(not label for label in labels) or len(set(labels)) != len(labels):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "runner token request is invalid")
        expires_at = self._now() + self._registration_token_ttl
        try:
            token = await self._control_plane.create_registration_token(
                labels,
                expires_at,
                session.user_id or None,
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
            tokens = await self._control_plane.list_registration_tokens()
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "runner token administration is unavailable")
        return control_plane_pb2.ListRunnerRegistrationTokensResponse(
            tokens=[_registration_token_message(token) for token in tokens]
        )

    async def RevokeRunnerRegistrationToken(
        self,
        request: control_plane_pb2.RevokeRunnerRegistrationTokenRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.RevokeRunnerRegistrationTokenResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        if not request.token_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "registration token ID is required")
        try:
            token = await self._control_plane.revoke_registration_token(
                request.token_id,
                session.user_id or None,
                self._now(),
            )
        except ValueError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "registration token is unknown or inactive")
        return control_plane_pb2.RevokeRunnerRegistrationTokenResponse(
            token=_registration_token_message(token)
        )

    async def Logout(
        self,
        request: control_plane_pb2.LogoutRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.LogoutResponse:
        del request
        now = self._now()
        metadata = context.invocation_metadata() or ()
        raw_token = next((value for key, value in metadata if key.lower() == _SESSION_METADATA_KEY), "")
        token = raw_token if isinstance(raw_token, str) else raw_token.decode("utf-8", errors="replace")
        if token:
            try:
                session = await self._control_plane.validate_session(token, None, now, False)
            except (NotImplementedError, PermissionError, ValueError):
                session = None
            if session is not None:
                await self._control_plane.revoke_session(token, now)
                await self._control_plane.append_audit(
                    actor_user_id=session.user_id,
                    action="user.logout",
                    resource_type="user",
                    resource_id=session.user_id,
                    outcome="succeeded",
                    now=now,
                )
        return control_plane_pb2.LogoutResponse()

    async def ListWorkflows(
        self,
        request: control_plane_pb2.ListWorkflowsRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListWorkflowsResponse:
        del request
        await self._require_session(context)
        try:
            workflows = await self._control_plane.list_workflows()
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "workflows are unavailable")
        return control_plane_pb2.ListWorkflowsResponse(
            workflows=[_workflow_message(workflow) for workflow in workflows]
        )

    async def GetWorkflow(
        self,
        request: control_plane_pb2.GetWorkflowRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.GetWorkflowResponse:
        await self._require_session(context)
        if not request.workflow_run_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "workflow run ID is required")
        try:
            workflow = await self._control_plane.get_workflow(request.workflow_run_id)
        except ValueError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "workflow run is unknown")
        if workflow is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "workflow run is unknown")
        return control_plane_pb2.GetWorkflowResponse(workflow=_workflow_detail_message(workflow))

    async def ListWorkflowEvents(
        self,
        request: control_plane_pb2.ListWorkflowEventsRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListWorkflowEventsResponse:
        await self._require_session(context)
        if not request.workflow_run_id or request.after_id < 0 or request.limit < 0 or request.limit > 100:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "workflow event request is invalid")
        try:
            workflow = await self._control_plane.get_workflow(request.workflow_run_id)
        except ValueError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "workflow run is unknown")
        if workflow is None:
            await context.abort(grpc.StatusCode.NOT_FOUND, "workflow run is unknown")
        events = await self._control_plane.list_workflow_events(
            request.workflow_run_id, request.after_id, request.limit or 100
        )
        return control_plane_pb2.ListWorkflowEventsResponse(
            events=[_workflow_event_message(event) for event in events],
            next_cursor=events[-1]["id"] if len(events) == (request.limit or 100) else "",
        )

    async def ListRunners(
        self,
        request: control_plane_pb2.ListRunnersRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListRunnersResponse:
        del request
        await self._require_session(context)
        try:
            runners = await self._control_plane.list_runners()
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "runners are unavailable")
        return control_plane_pb2.ListRunnersResponse(
            runners=[_runner_message(runner) for runner in runners]
        )

    async def SetRunnerState(
        self,
        request: control_plane_pb2.SetRunnerStateRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SetRunnerStateResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        if not request.runner_id or request.state not in {"enable", "drain", "revoke"}:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "runner state request is invalid")
        try:
            runner = await self._control_plane.set_runner_state(
                request.runner_id, request.state, session.user_id or None, self._now()
            )
        except ValueError:
            await context.abort(grpc.StatusCode.NOT_FOUND, "runner is unknown")
        if self._runner_control is not None:
            try:
                await self._runner_control.set_draining(request.runner_id, request.state != "enable")
                if request.state == "revoke":
                    await self._runner_control.revoke_runner(request.runner_id)
            except Exception:
                logging.getLogger(__name__).exception("runner state delivery failed")
        return control_plane_pb2.SetRunnerStateResponse(runner=_runner_message(runner))

    async def SubmitHumanDecision(
        self,
        request: control_plane_pb2.SubmitHumanDecisionRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SubmitHumanDecisionResponse:
        session = await self._require_session(context, require_csrf=True)
        decision = request.decision
        if not request.workflow_run_id or decision not in ("approved", "changes_requested"):
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "human decision request is invalid")
        try:
            await self._control_plane.record_human_decision(
                request.workflow_run_id,
                decision,
                request.comment or None,
                session.user_id or None,
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

    async def RetryWorkflow(
        self, request: control_plane_pb2.RetryWorkflowRequest, context: grpc.aio.ServicerContext
    ) -> control_plane_pb2.RetryWorkflowResponse:
        result = await self._control_workflow("retry", request, context)
        if self._workflow_runtime is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "workflow resumption is unavailable")
        try:
            state = await self._workflow_runtime.run(request.workflow_run_id, {"status": "recovering"})
        except Exception:
            logging.getLogger(__name__).exception("workflow retry failed")
            await context.abort(grpc.StatusCode.INTERNAL, "workflow could not be resumed")
        return control_plane_pb2.RetryWorkflowResponse(workflow=_workflow_message_from_state(request.workflow_run_id, result, state))

    async def CancelWorkflow(
        self, request: control_plane_pb2.CancelWorkflowRequest, context: grpc.aio.ServicerContext
    ) -> control_plane_pb2.CancelWorkflowResponse:
        result = await self._control_workflow("cancel", request, context)
        await self._cancel_runner_execution(result)
        return control_plane_pb2.CancelWorkflowResponse(workflow=_workflow_message_from_result(result))

    async def BlockWorkflow(
        self, request: control_plane_pb2.BlockWorkflowRequest, context: grpc.aio.ServicerContext
    ) -> control_plane_pb2.BlockWorkflowResponse:
        result = await self._control_workflow("block", request, context)
        await self._cancel_runner_execution(result)
        return control_plane_pb2.BlockWorkflowResponse(workflow=_workflow_message_from_result(result))

    async def _control_workflow(
        self, action: str, request: Any, context: grpc.aio.ServicerContext
    ) -> dict[str, object]:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        if not request.workflow_run_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "workflow run ID is required")
        try:
            method = getattr(self._control_plane, f"{action}_workflow")
            return await method(request.workflow_run_id, request.reason or None, session.user_id or None, self._now())
        except ValueError as error:
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, str(error))
        raise AssertionError("context.abort must not return")

    async def _cancel_runner_execution(self, result: dict[str, object]) -> None:
        cancellation = result.get("cancellation")
        if not isinstance(cancellation, dict) or self._runner_control is None:
            return
        try:
            await self._runner_control.cancel_execution(
                str(cancellation["runner_id"]),
                str(cancellation["execution_id"]),
                int(cancellation["lease_generation"]),
            )
        except Exception:
            logging.getLogger(__name__).exception("runner cancellation delivery failed")

    async def _require_session(
        self,
        context: grpc.aio.ServicerContext,
        administrator: bool = False,
        require_csrf: bool = False,
    ) -> AuthenticatedSession:
        metadata = context.invocation_metadata() or ()
        raw_token = next((value for key, value in metadata if key.lower() == _SESSION_METADATA_KEY), "")
        raw_csrf = next((value for key, value in metadata if key.lower() == _CSRF_METADATA_KEY), "")
        token = raw_token if isinstance(raw_token, str) else raw_token.decode("utf-8", errors="replace")
        csrf_token = raw_csrf if isinstance(raw_csrf, str) else raw_csrf.decode("utf-8", errors="replace")
        if not token:
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "session is required")
        try:
            session = await self._control_plane.validate_session(token, csrf_token or None, self._now(), require_csrf)
        except (NotImplementedError, PermissionError, ValueError):
            await context.abort(grpc.StatusCode.UNAUTHENTICATED, "session is invalid")
        if administrator and session.role != "admin":
            await context.abort(grpc.StatusCode.PERMISSION_DENIED, "administrator access is required")
        return session


def _project_arguments(
    project: control_plane_pb2.ProjectConfiguration,
) -> tuple[str, str, str | None, str | None, str, tuple[str, ...]]:
    return (
        project.name,
        project.repository_mode,
        project.repository_url or None,
        project.local_repository_path or None,
        project.default_branch,
        tuple(project.required_runner_labels),
    )


def _project_message(project: ProjectRecord) -> control_plane_pb2.Project:
    return control_plane_pb2.Project(
        id=project["id"],
        name=project["name"],
        enabled=project["enabled"],
    )


def _registration_token_message(token: RegistrationTokenRecord) -> control_plane_pb2.RunnerRegistrationToken:
    return control_plane_pb2.RunnerRegistrationToken(
        id=token["id"],
        allowed_labels=token["allowed_labels"],
        created_at=token["created_at"].isoformat(),
        expires_at=token["expires_at"].isoformat(),
        used_at=_optional_timestamp(token["used_at"]),
        revoked_at=_optional_timestamp(token["revoked_at"]),
    )


def _text(value: object) -> str:
    """Coerce an untyped workflow-state value into a protobuf string field.

    Graph state is `dict[str, object]`, so a missing key yields None, which
    protobuf rejects. Absent values become the empty string.
    """
    return "" if value is None else str(value)


def _optional_timestamp(value: datetime | None) -> str:
    return value.isoformat() if value is not None else ""


def _runner_message(runner: RunnerRecord) -> control_plane_pb2.Runner:
    return control_plane_pb2.Runner(
        id=runner["id"],
        name=runner["name"],
        enabled=runner["enabled"],
        draining=runner["draining"],
        status=runner["status"],
        labels=runner["labels"],
        last_seen_at=_optional_timestamp(runner["last_seen_at"]),
    )


def _workflow_message(workflow: WorkflowRecord) -> control_plane_pb2.Workflow:
    return control_plane_pb2.Workflow(
        id=workflow["id"],
        project_id=workflow["project_id"],
        status=workflow["status"],
        phase=workflow["phase"],
    )


def _workflow_detail_message(workflow: WorkflowDetailRecord) -> control_plane_pb2.Workflow:
    return control_plane_pb2.Workflow(
        id=workflow["id"],
        project_id=workflow["project_id"],
        status=workflow["status"],
        phase=workflow["phase"],
        issue_external_id=workflow["issue_external_id"],
        issue_title=workflow["issue_title"],
        branch_name=workflow["branch_name"] or "",
        pull_request_external_id=workflow["pull_request_external_id"] or "",
        pull_request_url=workflow["pull_request_url"] or "",
        pull_request_state=workflow["pull_request_state"] or "",
        blocking_reason=workflow["blocking_reason"] or "",
        planning_attempts=workflow["planning_attempts"],
        implementation_attempts=workflow["implementation_attempts"],
        pipeline_repair_attempts=workflow["pipeline_repair_attempts"],
        ci_repair_attempts=workflow["ci_repair_attempts"],
        review_cycles=workflow["review_cycles"],
        created_at=workflow["created_at"].isoformat(),
        updated_at=workflow["updated_at"].isoformat(),
    )


def _workflow_event_message(event: WorkflowEventRecord) -> control_plane_pb2.WorkflowEvent:
    return control_plane_pb2.WorkflowEvent(
        id=event["id"],
        event_type=event["event_type"],
        created_at=event["created_at"].isoformat(),
        payload_json=event["payload_json"],
    )


def _workflow_message_from_result(result: dict[str, object]) -> control_plane_pb2.Workflow:
    return control_plane_pb2.Workflow(
        id=_text(result.get("id")),
        project_id=_text(result.get("project_id")),
        status=_text(result.get("status")),
        phase=_text(result.get("phase")),
    )


def _workflow_message_from_state(
    workflow_run_id: str, result: dict[str, object], state: dict[str, object]
) -> control_plane_pb2.Workflow:
    return control_plane_pb2.Workflow(
        id=workflow_run_id,
        project_id=_text(state.get("project_id") or result.get("project_id")),
        status=_text(state.get("status") or result.get("status")),
        phase=_text(state.get("status") or result.get("phase")),
    )
