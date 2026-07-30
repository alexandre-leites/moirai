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
    __slots__ = ("user_id", "username", "role")
    USER_ID_FIELD_NUMBER: _ClassVar[int]
    USERNAME_FIELD_NUMBER: _ClassVar[int]
    ROLE_FIELD_NUMBER: _ClassVar[int]
    user_id: str
    username: str
    role: str
    def __init__(self, user_id: _Optional[str] = ..., username: _Optional[str] = ..., role: _Optional[str] = ...) -> None: ...

class ListProjectsRequest(_message.Message):
    __slots__ = ()
    def __init__(self) -> None: ...

class Project(_message.Message):
    __slots__ = ("id", "name", "enabled")
    ID_FIELD_NUMBER: _ClassVar[int]
    NAME_FIELD_NUMBER: _ClassVar[int]
    ENABLED_FIELD_NUMBER: _ClassVar[int]
    id: str
    name: str
    enabled: bool
    def __init__(self, id: _Optional[str] = ..., name: _Optional[str] = ..., enabled: _Optional[bool] = ...) -> None: ...

class ListProjectsResponse(_message.Message):
    __slots__ = ("projects",)
    PROJECTS_FIELD_NUMBER: _ClassVar[int]
    projects: _containers.RepeatedCompositeFieldContainer[Project]
    def __init__(self, projects: _Optional[_Iterable[_Union[Project, _Mapping]]] = ...) -> None: ...

class ProjectConfiguration(_message.Message):
    __slots__ = ("name", "repository_mode", "repository_url", "local_repository_path", "default_branch", "required_runner_labels")
    NAME_FIELD_NUMBER: _ClassVar[int]
    REPOSITORY_MODE_FIELD_NUMBER: _ClassVar[int]
    REPOSITORY_URL_FIELD_NUMBER: _ClassVar[int]
    LOCAL_REPOSITORY_PATH_FIELD_NUMBER: _ClassVar[int]
    DEFAULT_BRANCH_FIELD_NUMBER: _ClassVar[int]
    REQUIRED_RUNNER_LABELS_FIELD_NUMBER: _ClassVar[int]
    name: str
    repository_mode: str
    repository_url: str
    local_repository_path: str
    default_branch: str
    required_runner_labels: _containers.RepeatedScalarFieldContainer[str]
    def __init__(self, name: _Optional[str] = ..., repository_mode: _Optional[str] = ..., repository_url: _Optional[str] = ..., local_repository_path: _Optional[str] = ..., default_branch: _Optional[str] = ..., required_runner_labels: _Optional[_Iterable[str]] = ...) -> None: ...

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
    __slots__ = ("id", "project_id", "status", "phase")
    ID_FIELD_NUMBER: _ClassVar[int]
    PROJECT_ID_FIELD_NUMBER: _ClassVar[int]
    STATUS_FIELD_NUMBER: _ClassVar[int]
    PHASE_FIELD_NUMBER: _ClassVar[int]
    id: str
    project_id: str
    status: str
    phase: str
    def __init__(self, id: _Optional[str] = ..., project_id: _Optional[str] = ..., status: _Optional[str] = ..., phase: _Optional[str] = ...) -> None: ...

class ListWorkflowsResponse(_message.Message):
    __slots__ = ("workflows",)
    WORKFLOWS_FIELD_NUMBER: _ClassVar[int]
    workflows: _containers.RepeatedCompositeFieldContainer[Workflow]
    def __init__(self, workflows: _Optional[_Iterable[_Union[Workflow, _Mapping]]] = ...) -> None: ...

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
