from google.protobuf.internal import containers as _containers
from google.protobuf import descriptor as _descriptor
from google.protobuf import message as _message
from collections.abc import Iterable as _Iterable, Mapping as _Mapping
from typing import ClassVar as _ClassVar, Optional as _Optional, Union as _Union

DESCRIPTOR: _descriptor.FileDescriptor

class LoginRequest(_message.Message):
    __slots__ = ("username", "password")
    USERNAME_FIELD_NUMBER: _ClassVar[int]
    PASSWORD_FIELD_NUMBER: _ClassVar[int]
    username: str
    password: str
    def __init__(self, username: _Optional[str] = ..., password: _Optional[str] = ...) -> None: ...

class LoginResponse(_message.Message):
    __slots__ = ("session_token", "user_id", "csrf_token")
    SESSION_TOKEN_FIELD_NUMBER: _ClassVar[int]
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    CSRF_TOKEN_FIELD_NUMBER: _ClassVar[int]
    session_token: str
    user_id: str
    csrf_token: str
    def __init__(self, session_token: _Optional[str] = ..., user_id: _Optional[str] = ..., csrf_token: _Optional[str] = ...) -> None: ...

class WhoAmIRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class WhoAmIResponse(_message.Message):
    __slots__ = ("user_id", "username", "role", "email", "display_name")
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    USERNAME_FIELD_NUMBER: _ClassVar[int]
    ROLE_FIELD_NUMBER: _ClassVar[int]
    EMAIL_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    user_id: str
    username: str
    role: str
    email: str
    display_name: str
    def __init__(self, user_id: _Optional[str] = ..., username: _Optional[str] = ..., role: _Optional[str] = ..., email: _Optional[str] = ..., display_name: _Optional[str] = ...) -> None: ...

class UpdateAccountRequest(_message.Message):
    __slots__ = ("current_password", "new_password", "new_email", "display_name")
    CURRENT_PASSWORD_FIELD_NUMBER: _ClassVar[int]
    NEW_PASSWORD_FIELD_NUMBER: _ClassVar[int]
    NEW_EMAIL_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    current_password: str
    new_password: str
    new_email: str
    display_name: str
    def __init__(self, current_password: _Optional[str] = ..., new_password: _Optional[str] = ..., new_email: _Optional[str] = ..., display_name: _Optional[str] = ...) -> None: ...

class UpdateAccountResponse(_message.Message):
    __slots__ = ("user_id", "username", "role", "email", "display_name")
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    USERNAME_FIELD_NUMBER: _ClassVar[int]
    ROLE_FIELD_NUMBER: _ClassVar[int]
    EMAIL_FIELD_NUMBER: _ClassVar[int]
    DISPLAY_NAME_FIELD_NUMBER: _ClassVar[int]
    user_id: str
    username: str
    role: str
    email: str
    display_name: str
    def __init__(self, user_id: _Optional[str] = ..., username: _Optional[str] = ..., role: _Optional[str] = ..., email: _Optional[str] = ..., display_name: _Optional[str] = ...) -> None: ...

class ListProjectsRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class PipelineStep(_message.Message):
    __slots__ = ("command", "timeout_seconds", "position", "required")
    COMMAND_FIELD_NUMBER: _ClassVar[int]
    TIMEOUT_SECONDS_FIELD_NUMBER: _ClassVar[int]
    POSITION_FIELD_NUMBER: _ClassVar[int]
    REQUIRED_FIELD_NUMBER: _ClassVar[int]
    command: str
    timeout_seconds: int
    position: int
    required: bool
    def __init__(self, command: _Optional[str] = ..., timeout_seconds: _Optional[int] = ..., position: _Optional[int] = ..., required: _Optional[bool] = ...) -> None: ...

class Project(_message.Message):
    __slots__ = ("id", "name", "enabled", "repository_mode", "repository_url", "local_repository_path", "default_branch", "required_runner_labels", "pipeline_steps")
    ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    ENABLED_FIELD_NUMBER: _ClassVar[int]
    REPOSITORY_MODE_FIELD_NUMBER: _ClassVar[int]
    REPOSITORY_URL_FIELD_NUMBER: _ClassVar[int]
    LOCAL_REPOSITORY_PATH_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_BRANCH_FIELD_NUMBER: _ClassVar[int]
    REQUIRED_RUNNER_LABELS_FIELD_NUMBER: _ClassVar[int]
    PIPELINE_STEPS_FIELD_NUMBER: _ClassVar[int]
    id: str
    name: str
    enabled: bool
    repository_mode: str
    repository_url: str
    local_repository_path: str
    default_branch: str
    required_runner_labels: _containers.RepeatedScalarFieldContainer[str]
    pipeline_steps: _containers.RepeatedCompositeFieldContainer[PipelineStep]
    def __init__(self, id: _Optional[str] = ..., name: _Optional[str] = ..., enabled: _Optional[bool] = ..., repository_mode: _Optional[str] = ..., repository_url: _Optional[str] = ..., local_repository_path: _Optional[str] = ..., default_branch: _Optional[str] = ..., required_runner_labels: _Optional[_Iterable[str]] = ..., pipeline_steps: _Optional[_Iterable[_Union[PipelineStep, _Mapping]]] = ...) -> None: ...

class ListProjectsResponse(_message.Message):
    __slots__ = ("projects",)
    PROJECTS_FIELD_NUMBER: _ClassVar[int]
    projects: _containers.RepeatedCompositeFieldContainer[Project]
    def __init__(self, projects: _Optional[_Iterable[_Union[Project, _Mapping]]] = ...) -> None: ...

class ProjectConfiguration(_message.Message):
    __slots__ = ("name", "repository_mode", "repository_url", "local_repository_path", "default_branch", "required_runner_labels", "pipeline_steps")
    NAME_FIELD_NUMBER: _ClassVar[int]
    REPOSITORY_MODE_FIELD_NUMBER: _ClassVar[int]
    REPOSITORY_URL_FIELD_NUMBER: _ClassVar[int]
    LOCAL_REPOSITORY_PATH_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_BRANCH_FIELD_NUMBER: _ClassVar[int]
    REQUIRED_RUNNER_LABELS_FIELD_NUMBER: _ClassVar[int]
    PIPELINE_STEPS_FIELD_NUMBER: _ClassVar[int]
    name: str
    repository_mode: str
    repository_url: str
    local_repository_path: str
    default_branch: str
    required_runner_labels: _containers.RepeatedScalarFieldContainer[str]
    pipeline_steps: _containers.RepeatedCompositeFieldContainer[PipelineStep]
    def __init__(self, name: _Optional[str] = ..., repository_mode: _Optional[str] = ..., repository_url: _Optional[str] = ..., local_repository_path: _Optional[str] = ..., default_branch: _Optional[str] = ..., required_runner_labels: _Optional[_Iterable[str]] = ..., pipeline_steps: _Optional[_Iterable[_Union[PipelineStep, _Mapping]]] = ...) -> None: ...

class CreateProjectRequest(_message.Message):
    __slots__ = ("project",)
    PROJECT_FIELD_NUMBER: _ClassVar[int]
    project: ProjectConfiguration
    def __init__(self, project: _Optional[_Union[ProjectConfiguration, _Mapping]] = ...) -> None: ...

class CreateProjectResponse(_message.Message):
    __slots__ = ("project",)
    PROJECT_FIELD_NUMBER: _ClassVar[int]
    project: Project
    def __init__(self, project: _Optional[_Union[Project, _Mapping]] = ...) -> None: ...

class UpdateProjectRequest(_message.Message):
    __slots__ = ("project_id", "project")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    project: ProjectConfiguration
    def __init__(self, project_id: _Optional[str] = ..., project: _Optional[_Union[ProjectConfiguration, _Mapping]] = ...) -> None: ...

class UpdateProjectResponse(_message.Message):
    __slots__ = ("project",)
    PROJECT_FIELD_NUMBER: _ClassVar[int]
    project: Project
    def __init__(self, project: _Optional[_Union[Project, _Mapping]] = ...) -> None: ...

class SetProjectEnabledRequest(_message.Message):
    __slots__ = ("project_id", "enabled")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    ENABLED_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    enabled: bool
    def __init__(self, project_id: _Optional[str] = ..., enabled: _Optional[bool] = ...) -> None: ...

class SetProjectEnabledResponse(_message.Message):
    __slots__ = ("project",)
    PROJECT_FIELD_NUMBER: _ClassVar[int]
    project: Project
    def __init__(self, project: _Optional[_Union[Project, _Mapping]] = ...) -> None: ...

class SetProjectCredentialRequest(_message.Message):
    __slots__ = ("project_id", "kind", "value")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    VALUE_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    kind: str
    value: str
    def __init__(self, project_id: _Optional[str] = ..., kind: _Optional[str] = ..., value: _Optional[str] = ...) -> None: ...

class ClearProjectCredentialRequest(_message.Message):
    __slots__ = ("project_id", "kind")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    KIND_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    kind: str
    def __init__(self, project_id: _Optional[str] = ..., kind: _Optional[str] = ...) -> None: ...

class ListProjectCredentialsRequest(_message.Message):
    __slots__ = ("project_id",)
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    def __init__(self, project_id: _Optional[str] = ...) -> None: ...

class ProjectCredential(_message.Message):
    __slots__ = ("kind", "created_at", "updated_at")
    KIND_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    UPDATED_AT_FIELD_NUMBER: _ClassVar[int]
    kind: str
    created_at: str
    updated_at: str
    def __init__(self, kind: _Optional[str] = ..., created_at: _Optional[str] = ..., updated_at: _Optional[str] = ...) -> None: ...

class SetProjectCredentialResponse(_message.Message):
    __slots__ = ("credentials",)
    CREDENTIALS_FIELD_NUMBER: _ClassVar[int]
    credentials: _containers.RepeatedCompositeFieldContainer[ProjectCredential]
    def __init__(self, credentials: _Optional[_Iterable[_Union[ProjectCredential, _Mapping]]] = ...) -> None: ...

class ClearProjectCredentialResponse(_message.Message):
    __slots__ = ("credentials",)
    CREDENTIALS_FIELD_NUMBER: _ClassVar[int]
    credentials: _containers.RepeatedCompositeFieldContainer[ProjectCredential]
    def __init__(self, credentials: _Optional[_Iterable[_Union[ProjectCredential, _Mapping]]] = ...) -> None: ...

class ListProjectCredentialsResponse(_message.Message):
    __slots__ = ("credentials",)
    CREDENTIALS_FIELD_NUMBER: _ClassVar[int]
    credentials: _containers.RepeatedCompositeFieldContainer[ProjectCredential]
    def __init__(self, credentials: _Optional[_Iterable[_Union[ProjectCredential, _Mapping]]] = ...) -> None: ...

class CreateRunnerRegistrationTokenRequest(_message.Message):
    __slots__ = ("allowed_labels",)
    ALLOWED_LABELS_FIELD_NUMBER: _ClassVar[int]
    allowed_labels: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, allowed_labels: _Optional[_Iterable[str]] = ...) -> None: ...

class CreateRunnerRegistrationTokenResponse(_message.Message):
    __slots__ = ("token", "expires_at")
    TOKEN_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_FIELD_NUMBER: _ClassVar[int]
    token: str
    expires_at: str
    def __init__(self, token: _Optional[str] = ..., expires_at: _Optional[str] = ...) -> None: ...

class RunnerRegistrationToken(_message.Message):
    __slots__ = ("id", "allowed_labels", "created_at", "expires_at", "used_at", "revoked_at")
    ID_FIELD_NUMBER: _ClassVar[int]
    ALLOWED_LABELS_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    EXPIRES_AT_FIELD_NUMBER: _ClassVar[int]
    USED_AT_FIELD_NUMBER: _ClassVar[int]
    REVOKED_AT_FIELD_NUMBER: _ClassVar[int]
    id: str
    allowed_labels: _containers.RepeatedScalarFieldContainer[str]
    created_at: str
    expires_at: str
    used_at: str
    revoked_at: str
    def __init__(self, id: _Optional[str] = ..., allowed_labels: _Optional[_Iterable[str]] = ..., created_at: _Optional[str] = ..., expires_at: _Optional[str] = ..., used_at: _Optional[str] = ..., revoked_at: _Optional[str] = ...) -> None: ...

class ListRunnerRegistrationTokensRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class ListRunnerRegistrationTokensResponse(_message.Message):
    __slots__ = ("tokens",)
    TOKENS_FIELD_NUMBER: _ClassVar[int]
    tokens: _containers.RepeatedCompositeFieldContainer[RunnerRegistrationToken]
    def __init__(self, tokens: _Optional[_Iterable[_Union[RunnerRegistrationToken, _Mapping]]] = ...) -> None: ...

class RevokeRunnerRegistrationTokenRequest(_message.Message):
    __slots__ = ("token_id",)
    TOKEN_ID_FIELD_NUMBER: _ClassVar[int]
    token_id: str
    def __init__(self, token_id: _Optional[str] = ...) -> None: ...

class RevokeRunnerRegistrationTokenResponse(_message.Message):
    __slots__ = ("token",)
    TOKEN_FIELD_NUMBER: _ClassVar[int]
    token: RunnerRegistrationToken
    def __init__(self, token: _Optional[_Union[RunnerRegistrationToken, _Mapping]] = ...) -> None: ...

class ListWorkflowsRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class Workflow(_message.Message):
    __slots__ = ("id", "project_id", "status", "phase", "issue_external_id", "issue_title", "branch_name", "pull_request_external_id", "pull_request_url", "pull_request_state", "blocking_reason", "planning_attempts", "implementation_attempts", "pipeline_repair_attempts", "ci_repair_attempts", "review_cycles", "created_at", "updated_at", "total_agent_executions")
    ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    PHASE_FIELD_NUMBER: _ClassVar[int]
    ISSUE_EXTERNAL_ID_FIELD_NUMBER: _ClassVar[int]
    ISSUE_TITLE_FIELD_NUMBER: _ClassVar[int]
    BRANCH_NAME_FIELD_NUMBER: _ClassVar[int]
    PULL_REQUEST_EXTERNAL_ID_FIELD_NUMBER: _ClassVar[int]
    PULL_REQUEST_URL_FIELD_NUMBER: _ClassVar[int]
    PULL_REQUEST_STATE_FIELD_NUMBER: _ClassVar[int]
    BLOCKING_REASON_FIELD_NUMBER: _ClassVar[int]
    PLANNING_ATTEMPTS_FIELD_NUMBER: _ClassVar[int]
    IMPLEMENTATION_ATTEMPTS_FIELD_NUMBER: _ClassVar[int]
    PIPELINE_REPAIR_ATTEMPTS_FIELD_NUMBER: _ClassVar[int]
    CI_REPAIR_ATTEMPTS_FIELD_NUMBER: _ClassVar[int]
    REVIEW_CYCLES_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    UPDATED_AT_FIELD_NUMBER: _ClassVar[int]
    TOTAL_AGENT_EXECUTIONS_FIELD_NUMBER: _ClassVar[int]
    id: str
    project_id: str
    status: str
    phase: str
    issue_external_id: str
    issue_title: str
    branch_name: str
    pull_request_external_id: str
    pull_request_url: str
    pull_request_state: str
    blocking_reason: str
    planning_attempts: int
    implementation_attempts: int
    pipeline_repair_attempts: int
    ci_repair_attempts: int
    review_cycles: int
    created_at: str
    updated_at: str
    total_agent_executions: int
    def __init__(self, id: _Optional[str] = ..., project_id: _Optional[str] = ..., status: _Optional[str] = ..., phase: _Optional[str] = ..., issue_external_id: _Optional[str] = ..., issue_title: _Optional[str] = ..., branch_name: _Optional[str] = ..., pull_request_external_id: _Optional[str] = ..., pull_request_url: _Optional[str] = ..., pull_request_state: _Optional[str] = ..., blocking_reason: _Optional[str] = ..., planning_attempts: _Optional[int] = ..., implementation_attempts: _Optional[int] = ..., pipeline_repair_attempts: _Optional[int] = ..., ci_repair_attempts: _Optional[int] = ..., review_cycles: _Optional[int] = ..., created_at: _Optional[str] = ..., updated_at: _Optional[str] = ..., total_agent_executions: _Optional[int] = ...) -> None: ...

class ListWorkflowsResponse(_message.Message):
    __slots__ = ("workflows",)
    WORKFLOWS_FIELD_NUMBER: _ClassVar[int]
    workflows: _containers.RepeatedCompositeFieldContainer[Workflow]
    def __init__(self, workflows: _Optional[_Iterable[_Union[Workflow, _Mapping]]] = ...) -> None: ...

class GetWorkflowRequest(_message.Message):
    __slots__ = ("workflow_run_id",)
    WORKFLOW_RUN_ID_FIELD_NUMBER: _ClassVar[int]
    workflow_run_id: str
    def __init__(self, workflow_run_id: _Optional[str] = ...) -> None: ...

class GetWorkflowResponse(_message.Message):
    __slots__ = ("workflow",)
    WORKFLOW_FIELD_NUMBER: _ClassVar[int]
    workflow: Workflow
    def __init__(self, workflow: _Optional[_Union[Workflow, _Mapping]] = ...) -> None: ...

class WorkflowEvent(_message.Message):
    __slots__ = ("id", "event_type", "created_at", "payload_json")
    ID_FIELD_NUMBER: _ClassVar[int]
    EVENT_TYPE_FIELD_NUMBER: _ClassVar[int]
    CREATED_AT_FIELD_NUMBER: _ClassVar[int]
    PAYLOAD_JSON_FIELD_NUMBER: _ClassVar[int]
    id: str
    event_type: str
    created_at: str
    payload_json: str
    def __init__(self, id: _Optional[str] = ..., event_type: _Optional[str] = ..., created_at: _Optional[str] = ..., payload_json: _Optional[str] = ...) -> None: ...

class ListWorkflowEventsRequest(_message.Message):
    __slots__ = ("workflow_run_id", "after_id", "limit")
    WORKFLOW_RUN_ID_FIELD_NUMBER: _ClassVar[int]
    AFTER_ID_FIELD_NUMBER: _ClassVar[int]
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    workflow_run_id: str
    after_id: int
    limit: int
    def __init__(self, workflow_run_id: _Optional[str] = ..., after_id: _Optional[int] = ..., limit: _Optional[int] = ...) -> None: ...

class ListWorkflowEventsResponse(_message.Message):
    __slots__ = ("events", "next_cursor")
    EVENTS_FIELD_NUMBER: _ClassVar[int]
    NEXT_CURSOR_FIELD_NUMBER: _ClassVar[int]
    events: _containers.RepeatedCompositeFieldContainer[WorkflowEvent]
    next_cursor: str
    def __init__(self, events: _Optional[_Iterable[_Union[WorkflowEvent, _Mapping]]] = ..., next_cursor: _Optional[str] = ...) -> None: ...

class ListRunnersRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class Runner(_message.Message):
    __slots__ = ("id", "name", "enabled", "draining", "status", "labels", "last_seen_at")
    ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    ENABLED_FIELD_NUMBER: _ClassVar[int]
    DRAINING_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    LABELS_FIELD_NUMBER: _ClassVar[int]
    LAST_SEEN_AT_FIELD_NUMBER: _ClassVar[int]
    id: str
    name: str
    enabled: bool
    draining: bool
    status: str
    labels: _containers.RepeatedScalarFieldContainer[str]
    last_seen_at: str
    def __init__(self, id: _Optional[str] = ..., name: _Optional[str] = ..., enabled: _Optional[bool] = ..., draining: _Optional[bool] = ..., status: _Optional[str] = ..., labels: _Optional[_Iterable[str]] = ..., last_seen_at: _Optional[str] = ...) -> None: ...

class ListRunnersResponse(_message.Message):
    __slots__ = ("runners",)
    RUNNERS_FIELD_NUMBER: _ClassVar[int]
    runners: _containers.RepeatedCompositeFieldContainer[Runner]
    def __init__(self, runners: _Optional[_Iterable[_Union[Runner, _Mapping]]] = ...) -> None: ...

class ListQueueRequest(_message.Message):
    __slots__ = ("limit",)
    LIMIT_FIELD_NUMBER: _ClassVar[int]
    limit: int
    def __init__(self, limit: _Optional[int] = ...) -> None: ...

class QueueEntry(_message.Message):
    __slots__ = ("project_id", "project_name", "external_id", "title", "priority", "blocked_reason")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_NAME_FIELD_NUMBER: _ClassVar[int]
    EXTERNAL_ID_FIELD_NUMBER: _ClassVar[int]
    TITLE_FIELD_NUMBER: _ClassVar[int]
    PRIORITY_FIELD_NUMBER: _ClassVar[int]
    BLOCKED_REASON_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    project_name: str
    external_id: str
    title: str
    priority: int
    blocked_reason: str
    def __init__(self, project_id: _Optional[str] = ..., project_name: _Optional[str] = ..., external_id: _Optional[str] = ..., title: _Optional[str] = ..., priority: _Optional[int] = ..., blocked_reason: _Optional[str] = ...) -> None: ...

class ListQueueResponse(_message.Message):
    __slots__ = ("entries",)
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[QueueEntry]
    def __init__(self, entries: _Optional[_Iterable[_Union[QueueEntry, _Mapping]]] = ...) -> None: ...

class SyncNowRequest(_message.Message):
    __slots__ = ("project_id",)
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    def __init__(self, project_id: _Optional[str] = ...) -> None: ...

class ProjectSyncResult(_message.Message):
    __slots__ = ("project_id", "synced_issues", "error")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    SYNCED_ISSUES_FIELD_NUMBER: _ClassVar[int]
    ERROR_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    synced_issues: int
    error: str
    def __init__(self, project_id: _Optional[str] = ..., synced_issues: _Optional[int] = ..., error: _Optional[str] = ...) -> None: ...

class SyncNowResponse(_message.Message):
    __slots__ = ("results",)
    RESULTS_FIELD_NUMBER: _ClassVar[int]
    results: _containers.RepeatedCompositeFieldContainer[ProjectSyncResult]
    def __init__(self, results: _Optional[_Iterable[_Union[ProjectSyncResult, _Mapping]]] = ...) -> None: ...

class IssueSyncStatusRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class IssueSyncStatusEntry(_message.Message):
    __slots__ = ("project_id", "project_name", "enabled", "issue_count", "eligible_count", "last_synced_at", "consecutive_failures", "next_retry_at", "last_error", "backing_off")
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_NAME_FIELD_NUMBER: _ClassVar[int]
    ENABLED_FIELD_NUMBER: _ClassVar[int]
    ISSUE_COUNT_FIELD_NUMBER: _ClassVar[int]
    ELIGIBLE_COUNT_FIELD_NUMBER: _ClassVar[int]
    LAST_SYNCED_AT_FIELD_NUMBER: _ClassVar[int]
    CONSECUTIVE_FAILURES_FIELD_NUMBER: _ClassVar[int]
    NEXT_RETRY_AT_FIELD_NUMBER: _ClassVar[int]
    LAST_ERROR_FIELD_NUMBER: _ClassVar[int]
    BACKING_OFF_FIELD_NUMBER: _ClassVar[int]
    project_id: str
    project_name: str
    enabled: bool
    issue_count: int
    eligible_count: int
    last_synced_at: str
    consecutive_failures: int
    next_retry_at: str
    last_error: str
    backing_off: bool
    def __init__(self, project_id: _Optional[str] = ..., project_name: _Optional[str] = ..., enabled: _Optional[bool] = ..., issue_count: _Optional[int] = ..., eligible_count: _Optional[int] = ..., last_synced_at: _Optional[str] = ..., consecutive_failures: _Optional[int] = ..., next_retry_at: _Optional[str] = ..., last_error: _Optional[str] = ..., backing_off: _Optional[bool] = ...) -> None: ...

class IssueSyncStatusResponse(_message.Message):
    __slots__ = ("entries",)
    ENTRIES_FIELD_NUMBER: _ClassVar[int]
    entries: _containers.RepeatedCompositeFieldContainer[IssueSyncStatusEntry]
    def __init__(self, entries: _Optional[_Iterable[_Union[IssueSyncStatusEntry, _Mapping]]] = ...) -> None: ...

class SetRunnerStateRequest(_message.Message):
    __slots__ = ("runner_id", "state")
    RUNNER_ID_FIELD_NUMBER: _ClassVar[int]
    STATE_FIELD_NUMBER: _ClassVar[int]
    runner_id: str
    state: str
    def __init__(self, runner_id: _Optional[str] = ..., state: _Optional[str] = ...) -> None: ...

class SetRunnerStateResponse(_message.Message):
    __slots__ = ("runner",)
    RUNNER_FIELD_NUMBER: _ClassVar[int]
    runner: Runner
    def __init__(self, runner: _Optional[_Union[Runner, _Mapping]] = ...) -> None: ...

class LogoutRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class LogoutResponse(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class SubmitHumanDecisionRequest(_message.Message):
    __slots__ = ("workflow_run_id", "decision", "comment")
    WORKFLOW_RUN_ID_FIELD_NUMBER: _ClassVar[int]
    DECISION_FIELD_NUMBER: _ClassVar[int]
    COMMENT_FIELD_NUMBER: _ClassVar[int]
    workflow_run_id: str
    decision: str
    comment: str
    def __init__(self, workflow_run_id: _Optional[str] = ..., decision: _Optional[str] = ..., comment: _Optional[str] = ...) -> None: ...

class SubmitHumanDecisionResponse(_message.Message):
    __slots__ = ("workflow",)
    WORKFLOW_FIELD_NUMBER: _ClassVar[int]
    workflow: Workflow
    def __init__(self, workflow: _Optional[_Union[Workflow, _Mapping]] = ...) -> None: ...

class RetryWorkflowRequest(_message.Message):
    __slots__ = ("workflow_run_id", "reason")
    WORKFLOW_RUN_ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    workflow_run_id: str
    reason: str
    def __init__(self, workflow_run_id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class RetryWorkflowResponse(_message.Message):
    __slots__ = ("workflow",)
    WORKFLOW_FIELD_NUMBER: _ClassVar[int]
    workflow: Workflow
    def __init__(self, workflow: _Optional[_Union[Workflow, _Mapping]] = ...) -> None: ...

class CancelWorkflowRequest(_message.Message):
    __slots__ = ("workflow_run_id", "reason")
    WORKFLOW_RUN_ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    workflow_run_id: str
    reason: str
    def __init__(self, workflow_run_id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class CancelWorkflowResponse(_message.Message):
    __slots__ = ("workflow",)
    WORKFLOW_FIELD_NUMBER: _ClassVar[int]
    workflow: Workflow
    def __init__(self, workflow: _Optional[_Union[Workflow, _Mapping]] = ...) -> None: ...

class BlockWorkflowRequest(_message.Message):
    __slots__ = ("workflow_run_id", "reason")
    WORKFLOW_RUN_ID_FIELD_NUMBER: _ClassVar[int]
    REASON_FIELD_NUMBER: _ClassVar[int]
    workflow_run_id: str
    reason: str
    def __init__(self, workflow_run_id: _Optional[str] = ..., reason: _Optional[str] = ...) -> None: ...

class BlockWorkflowResponse(_message.Message):
    __slots__ = ("workflow",)
    WORKFLOW_FIELD_NUMBER: _ClassVar[int]
    workflow: Workflow
    def __init__(self, workflow: _Optional[_Union[Workflow, _Mapping]] = ...) -> None: ...

class GetSchedulerMetricsRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class GetSchedulerMetricsResponse(_message.Message):
    __slots__ = ("queue_depth", "active_workflows", "scheduled_jobs")
    QUEUE_DEPTH_FIELD_NUMBER: _ClassVar[int]
    ACTIVE_WORKFLOWS_FIELD_NUMBER: _ClassVar[int]
    SCHEDULED_JOBS_FIELD_NUMBER: _ClassVar[int]
    queue_depth: int
    active_workflows: int
    scheduled_jobs: int
    def __init__(self, queue_depth: _Optional[int] = ..., active_workflows: _Optional[int] = ..., scheduled_jobs: _Optional[int] = ...) -> None: ...
