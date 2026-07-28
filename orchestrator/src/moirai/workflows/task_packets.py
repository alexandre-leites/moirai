from __future__ import annotations

from dataclasses import dataclass, replace
from typing import Literal, cast

ExecutionRole = Literal["planner", "developer", "pipeline", "reviewer", "repairer"]


@dataclass(frozen=True)
class PipelineCommand:
    command: str
    timeout_seconds: int


@dataclass(frozen=True)
class RepositorySource:
    project_id: str
    mode: Literal["managed_clone", "existing_path"]
    default_branch: str
    branch: str
    url: str | None = None
    local_path: str | None = None

    def packet(self) -> dict[str, object]:
        packet: dict[str, object] = {
            "projectId": self.project_id,
            "mode": self.mode,
            "defaultBranch": self.default_branch,
            "branch": self.branch,
        }
        if self.mode == "managed_clone":
            if not self.url:
                raise ValueError("managed clone task packet requires a repository URL")
            packet["url"] = self.url
        elif self.mode == "existing_path":
            if not self.local_path:
                raise ValueError("existing-path task packet requires a local path")
            packet["localPath"] = self.local_path
        return packet


@dataclass(frozen=True)
class TaskExecutionRequest:
    job_id: str
    execution_id: str
    role: ExecutionRole
    objective: str
    issue_external_id: str
    issue_title: str
    issue_body: str
    repository: RepositorySource
    timeout_seconds: int
    may_modify_files: bool
    may_push: bool
    may_merge: bool
    pipeline: tuple[PipelineCommand, ...] = ()


def task_execution(
    *,
    job_id: str,
    execution_id: str,
    role: ExecutionRole,
    project_id: str,
    issue_external_id: str,
    issue_title: str,
    issue_body: str,
    repository_mode: str,
    repository_url: str | None,
    local_repository_path: str | None,
    default_branch: str,
    timeout_seconds: int = 1800,
) -> TaskExecutionRequest:
    if repository_mode not in {"managed_clone", "existing_path"}:
        raise ValueError("task packet repository mode is invalid")
    mode = cast(Literal["managed_clone", "existing_path"], repository_mode)
    if role not in {"planner", "developer", "pipeline", "reviewer", "repairer"}:
        raise ValueError("task packet execution role is invalid")
    read_only = role in {"planner", "pipeline", "reviewer"}
    return TaskExecutionRequest(
        job_id=job_id,
        execution_id=execution_id,
        role=role,
        objective=issue_title,
        issue_external_id=issue_external_id,
        issue_title=issue_title,
        issue_body=issue_body,
        repository=RepositorySource(
            project_id=project_id,
            mode=mode,
            default_branch=default_branch,
            branch=f"agent/{issue_external_id}/{job_id[:8]}",
            url=repository_url,
            local_path=local_repository_path,
        ),
        timeout_seconds=timeout_seconds,
        may_modify_files=not read_only,
        may_push=role == "developer",
        may_merge=False,
    )


def pipeline_task_execution(
    *,
    job_id: str,
    execution_id: str,
    project_id: str,
    issue_external_id: str,
    issue_title: str,
    issue_body: str,
    repository_mode: str,
    repository_url: str | None,
    local_repository_path: str | None,
    default_branch: str,
    pipeline: tuple[PipelineCommand, ...],
    timeout_seconds: int = 3600,
) -> TaskExecutionRequest:
    request = task_execution(
        job_id=job_id,
        execution_id=execution_id,
        role="pipeline",
        project_id=project_id,
        issue_external_id=issue_external_id,
        issue_title=issue_title,
        issue_body=issue_body,
        repository_mode=repository_mode,
        repository_url=repository_url,
        local_repository_path=local_repository_path,
        default_branch=default_branch,
        timeout_seconds=timeout_seconds,
    )
    return replace(request, pipeline=pipeline)


def planner_task_execution(
    *,
    job_id: str,
    project_id: str,
    issue_external_id: str,
    issue_title: str,
    issue_body: str,
    repository_mode: str,
    repository_url: str | None,
    local_repository_path: str | None,
    default_branch: str,
    timeout_seconds: int = 1800,
) -> TaskExecutionRequest:
    return task_execution(
        job_id=job_id,
        execution_id=f"{job_id}-plan",
        role="planner",
        project_id=project_id,
        issue_external_id=issue_external_id,
        issue_title=issue_title,
        issue_body=issue_body,
        repository_mode=repository_mode,
        repository_url=repository_url,
        local_repository_path=local_repository_path,
        default_branch=default_branch,
        timeout_seconds=timeout_seconds,
    )


def build_task_packet(request: TaskExecutionRequest) -> dict[str, object]:
    if not request.job_id or not request.execution_id:
        raise ValueError("task execution identifiers are required")
    if not request.objective:
        raise ValueError("task objective is required")
    if request.timeout_seconds <= 0:
        raise ValueError("task timeout must be positive")
    if len(request.pipeline) > 32 or any(
        not command.command.strip() or command.timeout_seconds < 1 or command.timeout_seconds > 3600
        for command in request.pipeline
    ):
        raise ValueError("task pipeline is invalid")
    return {
        "protocolVersion": "1.0",
        "jobId": request.job_id,
        "executionId": request.execution_id,
        "role": request.role,
        "objective": request.objective,
        "issue": {
            "externalId": request.issue_external_id,
            "title": request.issue_title,
            "body": request.issue_body,
        },
        "repository": request.repository.packet(),
        "promptPath": ".loop/prompt.md",
        "expectedOutput": ".loop/result.json",
        "timeoutSeconds": request.timeout_seconds,
        "environmentRefs": [],
        "pipeline": [
            {"command": command.command, "timeoutSeconds": command.timeout_seconds}
            for command in request.pipeline
        ],
        "constraints": {
            "mayModifyFiles": request.may_modify_files,
            "mayPush": request.may_push,
            "mayMerge": request.may_merge,
        },
    }
