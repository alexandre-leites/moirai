from __future__ import annotations

from datetime import datetime
from typing import TYPE_CHECKING, Protocol, TypedDict

from moirai.persistence.authentication import (
    AccountProfile,
    AuthenticatedSession,
    SessionCredentials,
)

if TYPE_CHECKING:
    from moirai.persistence.control_plane import AsyncpgControlPlane


class ProjectRecord(TypedDict):
    id: str
    name: str
    enabled: bool
    repository_mode: str
    repository_url: str | None
    local_repository_path: str | None
    default_branch: str
    required_runner_labels: list[str]


class RegistrationTokenRecord(TypedDict):
    id: str
    allowed_labels: list[str]
    created_at: datetime
    expires_at: datetime
    used_at: datetime | None
    revoked_at: datetime | None


class WorkflowRecord(TypedDict):
    id: str
    project_id: str
    status: str
    phase: str
    pull_request_external_id: str | None
    pull_request_url: str | None
    blocking_reason: str | None
    planning_attempts: int
    implementation_attempts: int
    pipeline_repair_attempts: int
    review_cycles: int
    ci_repair_attempts: int
    total_agent_executions: int


class WorkflowDetailRecord(WorkflowRecord):
    issue_external_id: str
    issue_title: str
    branch_name: str | None
    pull_request_state: str | None
    created_at: datetime
    updated_at: datetime


class WorkflowEventRecord(TypedDict):
    id: str
    event_type: str
    payload_json: str
    created_at: datetime


class RunnerRecord(TypedDict):
    id: str
    name: str
    enabled: bool
    draining: bool
    status: str
    labels: list[str]
    last_seen_at: datetime | None


class QueueEntryRecord(TypedDict):
    project_id: str
    project_name: str
    external_id: str
    title: str
    priority: int
    blocked_reason: str


class IssueSyncStatusRecord(TypedDict):
    project_id: str
    project_name: str
    enabled: bool
    issue_count: int
    eligible_count: int
    last_synced_at: datetime | None
    consecutive_failures: int
    next_retry_at: datetime | None
    last_error: str | None
    backing_off: bool


class ControlPlane(Protocol):
    """The control-plane surface ControlPlaneService depends on.

    Declaring this explicitly (instead of dispatching by method-name string)
    means a renamed or missing method is a `make typecheck` failure, not a
    runtime UNIMPLEMENTED status discovered by whoever calls the RPC.
    """

    async def login(self, username: str, password: str, now: datetime) -> SessionCredentials: ...

    async def validate_session(
        self, session_token: str, csrf_token: str | None, now: datetime, require_csrf: bool
    ) -> AuthenticatedSession: ...

    async def update_account(
        self,
        user_id: str,
        keep_session_id: str,
        current_password: str,
        new_password: str,
        new_email: str,
        display_name: str,
        now: datetime,
    ) -> AccountProfile: ...

    async def list_projects(self) -> list[ProjectRecord]: ...

    async def create_project(
        self,
        name: str,
        repository_mode: str,
        repository_url: str | None,
        local_repository_path: str | None,
        default_branch: str,
        required_runner_labels: tuple[str, ...],
        now: datetime,
        actor_user_id: str | None,
    ) -> ProjectRecord: ...

    async def update_project(
        self,
        project_id: str,
        name: str,
        repository_mode: str,
        repository_url: str | None,
        local_repository_path: str | None,
        default_branch: str,
        required_runner_labels: tuple[str, ...],
        now: datetime,
        actor_user_id: str | None,
    ) -> ProjectRecord: ...

    async def set_project_enabled(
        self, project_id: str, enabled: bool, now: datetime, actor_user_id: str | None
    ) -> ProjectRecord: ...

    async def create_registration_token(
        self, allowed_labels: tuple[str, ...], expires_at: datetime, actor_user_id: str | None, now: datetime
    ) -> str: ...

    async def list_registration_tokens(self) -> list[RegistrationTokenRecord]: ...

    async def revoke_registration_token(
        self, token_id: str, actor_user_id: str | None, now: datetime
    ) -> RegistrationTokenRecord: ...

    async def list_workflows(self) -> list[WorkflowDetailRecord]: ...

    async def get_workflow(self, workflow_run_id: str) -> WorkflowDetailRecord | None: ...

    async def list_workflow_events(
        self, workflow_run_id: str, after_id: int, limit: int
    ) -> list[WorkflowEventRecord]: ...

    async def record_human_decision(
        self,
        workflow_run_id: str,
        decision: str,
        comment: str | None,
        actor_user_id: str | None,
        now: datetime,
    ) -> dict[str, object]: ...

    async def retry_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]: ...

    async def cancel_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]: ...

    async def block_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]: ...

    async def list_runners(self) -> list[RunnerRecord]: ...

    async def list_queue(self, now: datetime, limit: int) -> list[QueueEntryRecord]: ...

    async def issue_sync_status(self, now: datetime) -> list[IssueSyncStatusRecord]: ...

    # Scheduler gauges for GetSchedulerMetrics: queue depth, active workflows,
    # scheduled jobs and the oldest runner heartbeat age.
    async def metrics_snapshot(self, now: datetime) -> dict[str, float]: ...

    # Per-project credentials. `describe_` reports which kinds are configured
    # and when; it never returns a value, and there is deliberately no protocol
    # method that returns one to a caller outside the orchestrator.
    async def set_project_credential(
        self, project_id: str, kind: str, value: str, actor_user_id: str | None, now: datetime
    ) -> None: ...

    async def clear_project_credential(
        self, project_id: str, kind: str, actor_user_id: str | None, now: datetime
    ) -> bool: ...

    async def describe_project_credentials(self, project_id: str) -> list[dict[str, object]]: ...

    # The runner-facing resolver. Returns (value, delivery), or None when the
    # project has no credential of that kind; raises StaleLeaseError when the
    # runner does not hold the job at that generation.
    async def resolve_job_secret(
        self, runner_id: str, job_id: str, generation: int, name: str, now: datetime
    ) -> tuple[str, str] | None: ...

    async def revoke_session(self, session_token: str, now: datetime) -> None: ...

    async def append_audit(
        self,
        actor_user_id: str | None,
        action: str,
        resource_type: str,
        resource_id: str,
        outcome: str,
        now: datetime,
    ) -> None: ...

    async def set_runner_state(
        self, runner_id: str, state: str, actor_user_id: str | None, now: datetime
    ) -> RunnerRecord: ...


if TYPE_CHECKING:
    # Forces `make typecheck` to fail if AsyncpgControlPlane (the production
    # control plane) drifts from this Protocol -- e.g. a renamed or deleted
    # method -- instead of that only surfacing as UNIMPLEMENTED at runtime.
    def _verify_asyncpg_control_plane_satisfies_protocol(control_plane: AsyncpgControlPlane) -> ControlPlane:
        return control_plane
