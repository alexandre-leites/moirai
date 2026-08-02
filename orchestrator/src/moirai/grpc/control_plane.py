from __future__ import annotations

import asyncio
import logging
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from typing import Any, cast

import grpc

from moirai.domain.control_plane import AuthenticationError
from moirai.grpc.protocol import (
    ControlPlane,
    IssueSyncStatusRecord,
    ProjectRecord,
    QueueEntryRecord,
    RegistrationTokenRecord,
    RunnerRecord,
    WorkflowDetailRecord,
    WorkflowEventRecord,
)
from moirai.persistence.authentication import AuthenticatedSession
from moirai.persistence.secrets import SecretCipherError
from proto import control_plane_pb2, control_plane_pb2_grpc

_SESSION_METADATA_KEY = "x-loop-session"
_CSRF_METADATA_KEY = "x-loop-csrf"
_QUEUE_DEFAULT_LIMIT = 50
_QUEUE_MAX_LIMIT = 100
# Upper bound on a manual sync pass. A pass already in progress under the
# shared sync lock is waited on, so this must exceed the per-project tracker
# timeout rather than the single-project fast path.
_SYNC_NOW_TIMEOUT_SECONDS = 120


class ControlPlaneService(control_plane_pb2_grpc.ControlPlaneServicer):
    def __init__(
        self,
        control_plane: ControlPlane,
        now: Callable[[], datetime] | None = None,
        registration_token_ttl: timedelta = timedelta(minutes=15),
        workflow_runtime: Any | None = None,
        runner_control: Any | None = None,
        issue_sync: Any | None = None,
    ) -> None:
        if registration_token_ttl <= timedelta():
            raise ValueError("registration token TTL must be positive")
        self._control_plane = control_plane
        self._now = now or (lambda: datetime.now(UTC))
        self._registration_token_ttl = registration_token_ttl
        self._workflow_runtime = workflow_runtime
        self._runner_control = runner_control
        self._issue_sync = issue_sync

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
            email=session.email,
            display_name=session.display_name,
        )

    async def UpdateAccount(
        self,
        request: control_plane_pb2.UpdateAccountRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.UpdateAccountResponse:
        session = await self._require_session(context, require_csrf=True)
        try:
            profile = await self._control_plane.update_account(
                session.user_id,
                session.id,
                request.current_password,
                request.new_password,
                request.new_email,
                request.display_name,
                self._now(),
            )
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "account management is unavailable")
        except AuthenticationError:
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, "current password is incorrect")
        except ValueError:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "account update is invalid")
        return control_plane_pb2.UpdateAccountResponse(
            user_id=profile.user_id,
            username=profile.username,
            role=profile.role,
            email=profile.email,
            display_name=profile.display_name,
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
                pipeline_steps=_pipeline_steps(request.project),
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
                pipeline_steps=_pipeline_steps(request.project),
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

    async def SetProjectCredential(
        self,
        request: control_plane_pb2.SetProjectCredentialRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SetProjectCredentialResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        if not request.project_id or not request.kind:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project ID and credential kind are required")
        if not request.value.strip():
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                "credential value must not be empty; clear the credential instead",
            )
        try:
            await self._control_plane.set_project_credential(
                request.project_id, request.kind, request.value, session.user_id or None, self._now()
            )
        except ValueError as error:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(error))
        except SecretCipherError as error:
            # No key configured, or an unusable one. FAILED_PRECONDITION rather
            # than INTERNAL: the deployment is missing configuration, and the
            # message says which.
            await context.abort(grpc.StatusCode.FAILED_PRECONDITION, str(error))
        return control_plane_pb2.SetProjectCredentialResponse(
            credentials=await self._credential_messages(request.project_id)
        )

    async def ClearProjectCredential(
        self,
        request: control_plane_pb2.ClearProjectCredentialRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ClearProjectCredentialResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        if not request.project_id or not request.kind:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project ID and credential kind are required")
        try:
            await self._control_plane.clear_project_credential(
                request.project_id, request.kind, session.user_id or None, self._now()
            )
        except ValueError as error:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, str(error))
        return control_plane_pb2.ClearProjectCredentialResponse(
            credentials=await self._credential_messages(request.project_id)
        )

    async def ListProjectCredentials(
        self,
        request: control_plane_pb2.ListProjectCredentialsRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListProjectCredentialsResponse:
        # Readable by any session: it reports only which kinds are configured.
        await self._require_session(context)
        if not request.project_id:
            await context.abort(grpc.StatusCode.INVALID_ARGUMENT, "project ID is required")
        return control_plane_pb2.ListProjectCredentialsResponse(
            credentials=await self._credential_messages(request.project_id)
        )

    async def _credential_messages(
        self, project_id: str
    ) -> list[control_plane_pb2.ProjectCredential]:
        describe = getattr(self._control_plane, "describe_project_credentials", None)
        if describe is None:
            return []
        return [
            control_plane_pb2.ProjectCredential(
                kind=str(entry["kind"]),
                created_at=_isoformat(entry.get("created_at")),
                updated_at=_isoformat(entry.get("updated_at")),
            )
            for entry in await describe(project_id)
        ]

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
            workflows=[_workflow_detail_message(workflow) for workflow in workflows]
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

    async def ListQueue(
        self,
        request: control_plane_pb2.ListQueueRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.ListQueueResponse:
        await self._require_session(context)
        limit = request.limit if request.limit > 0 else _QUEUE_DEFAULT_LIMIT
        if limit > _QUEUE_MAX_LIMIT:
            await context.abort(
                grpc.StatusCode.INVALID_ARGUMENT,
                f"queue limit must be between 1 and {_QUEUE_MAX_LIMIT}",
            )
        try:
            entries = await self._control_plane.list_queue(self._now(), limit)
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "queue is unavailable")
        return control_plane_pb2.ListQueueResponse(
            entries=[_queue_entry_message(entry) for entry in entries]
        )

    async def GetSchedulerMetrics(
        self,
        request: control_plane_pb2.GetSchedulerMetricsRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.GetSchedulerMetricsResponse:
        await self._require_session(context)
        snapshot = await self._control_plane.metrics_snapshot(self._now())
        return control_plane_pb2.GetSchedulerMetricsResponse(
            queue_depth=int(snapshot["queue_depth"]),
            active_workflows=int(snapshot["active_workflows"]),
            scheduled_jobs=int(snapshot["scheduled_jobs"]),
        )

    async def SyncNow(
        self,
        request: control_plane_pb2.SyncNowRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SyncNowResponse:
        await self._require_session(context, administrator=True, require_csrf=True)
        if self._issue_sync is None:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "issue sync is unavailable")
        project_id = request.project_id.strip() or None
        try:
            results = await asyncio.wait_for(
                self._issue_sync.sync_now(self._now(), project_id),
                timeout=_SYNC_NOW_TIMEOUT_SECONDS,
            )
        except TimeoutError:
            await context.abort(grpc.StatusCode.DEADLINE_EXCEEDED, "issue sync timed out")
        except Exception:
            logging.getLogger(__name__).exception("manual issue sync failed")
            await context.abort(grpc.StatusCode.INTERNAL, "issue sync failed")
        return control_plane_pb2.SyncNowResponse(
            results=[
                _project_sync_result_message(project_id, result)
                for project_id, result in results.items()
            ]
        )

    async def IssueSyncStatus(
        self,
        request: control_plane_pb2.IssueSyncStatusRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.IssueSyncStatusResponse:
        del request
        await self._require_session(context)
        try:
            entries = await self._control_plane.issue_sync_status(self._now())
        except NotImplementedError:
            await context.abort(grpc.StatusCode.UNIMPLEMENTED, "issue sync status is unavailable")
        return control_plane_pb2.IssueSyncStatusResponse(
            entries=[_issue_sync_status_message(entry) for entry in entries]
        )

    async def SetRunnerState(
        self,
        request: control_plane_pb2.SetRunnerStateRequest,
        context: grpc.aio.ServicerContext,
    ) -> control_plane_pb2.SetRunnerStateResponse:
        session = await self._require_session(context, administrator=False, require_csrf=True)
        await self._require_admin(session, context)
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
        session = await self._require_session(context, administrator=True, require_csrf=True)
        await self._require_admin(session, context)
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
        session = await self._require_session(context, administrator=True, require_csrf=True)
        await self._require_admin(session, context)
        result = await self._control_workflow("cancel", request, context)
        await self._cancel_runner_execution(result)
        return control_plane_pb2.CancelWorkflowResponse(workflow=_workflow_message_from_result(result))

    async def BlockWorkflow(
        self, request: control_plane_pb2.BlockWorkflowRequest, context: grpc.aio.ServicerContext
    ) -> control_plane_pb2.BlockWorkflowResponse:
        session = await self._require_session(context, administrator=True, require_csrf=True)
        await self._require_admin(session, context)
        result = await self._control_workflow("block", request, context)
        await self._cancel_runner_execution(result)
        return control_plane_pb2.BlockWorkflowResponse(workflow=_workflow_message_from_result(result))

    async def _control_workflow(
        self, action: str, request: Any, context: grpc.aio.ServicerContext
    ) -> dict[str, object]:
        session = await self._require_session(context, administrator=False, require_csrf=True)
        await self._require_admin(session, context)
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

    async def _require_admin(
        self, session: AuthenticatedSession, context: grpc.aio.ServicerContext
    ) -> None:
        # `ServicerContext.abort` is a coroutine on grpc.aio. Calling it without
        # awaiting built the coroutine and dropped it, so the abort never ran and
        # every caller carried on with a non-admin session -- issue #197.
        if session.role != "admin":
            await context.abort(
                grpc.StatusCode.PERMISSION_DENIED, "administrator access is required"
            )


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


def _pipeline_steps(project: control_plane_pb2.ProjectConfiguration) -> tuple[dict[str, object], ...]:
    return tuple(
        {
            "command": step.command,
            "timeout_seconds": step.timeout_seconds,
            "position": step.position,
            "required": step.required,
        }
        for step in project.pipeline_steps
    )


def _project_message(project: ProjectRecord) -> control_plane_pb2.Project:
    return control_plane_pb2.Project(
        id=project["id"],
        name=project["name"],
        enabled=project["enabled"],
        repository_mode=project.get("repository_mode", ""),
        repository_url=project.get("repository_url") or "",
        local_repository_path=project.get("local_repository_path") or "",
        default_branch=project.get("default_branch", ""),
        required_runner_labels=list(project.get("required_runner_labels") or []),
        pipeline_steps=[
            control_plane_pb2.PipelineStep(
                command=str(step["command"]),
                timeout_seconds=cast(int, step["timeout_seconds"]),
                position=cast(int, step["position"]),
                required=bool(step["required"]),
            )
            for step in project.get("pipeline_steps", [])
        ],
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


def _queue_entry_message(entry: QueueEntryRecord) -> control_plane_pb2.QueueEntry:
    return control_plane_pb2.QueueEntry(
        project_id=entry["project_id"],
        project_name=entry["project_name"],
        external_id=entry["external_id"],
        title=entry["title"],
        priority=entry["priority"],
        blocked_reason=entry["blocked_reason"],
    )


def _project_sync_result_message(project_id: str, result: int | str) -> control_plane_pb2.ProjectSyncResult:
    if isinstance(result, int):
        return control_plane_pb2.ProjectSyncResult(project_id=project_id, synced_issues=result)
    return control_plane_pb2.ProjectSyncResult(project_id=project_id, error=result)


def _issue_sync_status_message(entry: IssueSyncStatusRecord) -> control_plane_pb2.IssueSyncStatusEntry:
    return control_plane_pb2.IssueSyncStatusEntry(
        project_id=entry["project_id"],
        project_name=entry["project_name"],
        enabled=entry["enabled"],
        issue_count=entry["issue_count"],
        eligible_count=entry["eligible_count"],
        last_synced_at=_optional_timestamp(entry["last_synced_at"]),
        consecutive_failures=entry["consecutive_failures"],
        next_retry_at=_optional_timestamp(entry["next_retry_at"]),
        last_error=entry["last_error"] or "",
        backing_off=entry["backing_off"],
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
        total_agent_executions=workflow["total_agent_executions"],
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


def _isoformat(value: Any) -> str:
    """Timestamps on the wire are ISO-8601 strings, matching the other messages."""
    return value.isoformat() if hasattr(value, "isoformat") else ""
