from __future__ import annotations

from datetime import datetime
from typing import TYPE_CHECKING, Protocol, TypedDict

from moirai.persistence.authentication import AuthenticatedSession, SessionCredentials

if TYPE_CHECKING:
    from moirai.persistence.control_plane import AsyncpgControlPlane


class ProjectRecord(TypedDict):
    id: str
    name: str
    enabled: bool


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


class RunnerRecord(TypedDict):
    id: str
    name: str
    enabled: bool
    draining: bool
    status: str
    labels: list[str]
    last_seen_at: datetime | None


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

    async def list_workflows(self) -> list[WorkflowRecord]: ...

    async def record_human_decision(
        self,
        workflow_run_id: str,
        decision: str,
        comment: str | None,
        actor_user_id: str | None,
        now: datetime,
    ) -> dict[str, object]: ...

    async def list_runners(self) -> list[RunnerRecord]: ...


if TYPE_CHECKING:
    # Forces `make typecheck` to fail if AsyncpgControlPlane (the production
    # control plane) drifts from this Protocol -- e.g. a renamed or deleted
    # method -- instead of that only surfacing as UNIMPLEMENTED at runtime.
    def _verify_asyncpg_control_plane_satisfies_protocol(control_plane: AsyncpgControlPlane) -> ControlPlane:
        return control_plane
