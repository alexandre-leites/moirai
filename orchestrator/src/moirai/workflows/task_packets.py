from __future__ import annotations

import re
from dataclasses import dataclass, replace
from typing import Literal, cast

ExecutionRole = Literal["planner", "developer", "pipeline", "reviewer", "verifier", "repairer"]

_ENVIRONMENT_NAME = re.compile(r"^[A-Z_][A-Z0-9_]{0,127}$")
_MAX_CONTEXT_ENTRIES = 64
_MAX_CONTEXT_ENTRY_BYTES = 8 * 1024


@dataclass(frozen=True)
class PipelineCommand:
    command: str
    timeout_seconds: int


@dataclass(frozen=True)
class EnvironmentRef:
    """A credential the runner must resolve before executing the task.

    Only the name and the reference travel in the packet, never a value. The
    runner resolves it either from the control plane -- which answers from the
    project's own credential, over TLS, for a job the runner currently holds --
    or, failing that, from its own environment under the same name.
    """

    name: str
    secret_ref: str

    def packet(self) -> dict[str, str]:
        return {"name": self.name, "secretRef": self.secret_ref}


GITHUB_TOKEN_REF = EnvironmentRef(name="GITHUB_TOKEN", secret_ref="github_token")
SSH_KEY_REF = EnvironmentRef(name="GIT_SSH_KEY", secret_ref="ssh_private_key")


def _is_ssh_remote(repository_url: str | None) -> bool:
    """Whether git will reach this remote over ssh rather than https."""
    if not repository_url:
        return False
    url = repository_url.strip()
    return url.startswith(("ssh://", "git+ssh://")) or ("@" in url.split("/", 1)[0] and "://" not in url)


def environment_refs_for(
    *, repository_mode: str, may_push: bool, repository_url: str | None = None
) -> tuple[EnvironmentRef, ...]:
    """Decide which credentials a role needs for its repository.

    A managed clone is fetched from the code host by the runner, and any role
    that may push writes back to it, so both cases require a credential. Which
    one follows the remote: an ssh remote needs a key, everything else a token.
    An ``existing_path`` read-only role works entirely inside an
    operator-provided checkout and is given nothing.

    A reference here is a *request*, not a guarantee -- the runner falls back to
    its own environment when the control plane has nothing for it, which is what
    keeps a deployment that predates per-project credentials working.
    """
    if not (may_push or repository_mode == "managed_clone"):
        return ()
    if _is_ssh_remote(repository_url):
        return (SSH_KEY_REF,)
    return (GITHUB_TOKEN_REF,)


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
    environment_refs: tuple[EnvironmentRef, ...] = ()
    pipeline: tuple[PipelineCommand, ...] = ()
    acceptance_criteria: tuple[str, ...] = ()
    plan: tuple[str, ...] = ()
    previous_failures: tuple[str, ...] = ()
    current_commit: str = ""
    diff_summary: str = ""
    failed_checks: tuple[str, ...] = ()
    review_findings: tuple[str, ...] = ()


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
    acceptance_criteria: tuple[str, ...] = (),
    plan: tuple[str, ...] = (),
    previous_failures: tuple[str, ...] = (),
    current_commit: str = "",
    diff_summary: str = "",
    failed_checks: tuple[str, ...] = (),
    review_findings: tuple[str, ...] = (),
    environment_refs: tuple[EnvironmentRef, ...] | None = None,
) -> TaskExecutionRequest:
    if repository_mode not in {"managed_clone", "existing_path"}:
        raise ValueError("task packet repository mode is invalid")
    mode = cast(Literal["managed_clone", "existing_path"], repository_mode)
    if role not in {"planner", "developer", "pipeline", "reviewer", "verifier", "repairer"}:
        raise ValueError("task packet execution role is invalid")
    read_only = role in {"planner", "pipeline", "reviewer", "verifier"}
    may_push = role == "developer"
    if environment_refs is None:
        environment_refs = environment_refs_for(
            repository_mode=mode, may_push=may_push, repository_url=repository_url
        )
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
        may_push=may_push,
        may_merge=False,
        environment_refs=environment_refs,
        acceptance_criteria=acceptance_criteria,
        plan=plan,
        previous_failures=previous_failures,
        current_commit=current_commit,
        diff_summary=diff_summary,
        failed_checks=failed_checks,
        review_findings=review_findings,
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
    acceptance_criteria: tuple[str, ...] = (),
    plan: tuple[str, ...] = (),
    previous_failures: tuple[str, ...] = (),
    current_commit: str = "",
    diff_summary: str = "",
    failed_checks: tuple[str, ...] = (),
    review_findings: tuple[str, ...] = (),
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
        acceptance_criteria=acceptance_criteria,
        plan=plan,
        previous_failures=previous_failures,
        current_commit=current_commit,
        diff_summary=diff_summary,
        failed_checks=failed_checks,
        review_findings=review_findings,
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
    acceptance_criteria: tuple[str, ...] = (),
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
        acceptance_criteria=acceptance_criteria,
    )


def _validate_environment_refs(references: tuple[EnvironmentRef, ...]) -> None:
    if len(references) > 64:
        raise ValueError("task environment references are invalid")
    names: set[str] = set()
    for reference in references:
        if not _ENVIRONMENT_NAME.fullmatch(reference.name) or reference.name in names:
            raise ValueError("task environment references are invalid")
        secret_ref = reference.secret_ref
        if not secret_ref or secret_ref.strip() != secret_ref or len(secret_ref) > 512:
            raise ValueError("task environment references are invalid")
        names.add(reference.name)


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
    context_lists = (
        request.acceptance_criteria,
        request.plan,
        request.previous_failures,
        request.failed_checks,
        request.review_findings,
    )
    if any(
        len(values) > _MAX_CONTEXT_ENTRIES
        or any(not value.strip() or len(value.encode()) > _MAX_CONTEXT_ENTRY_BYTES for value in values)
        for values in context_lists
    ):
        raise ValueError("task execution context is invalid")
    _validate_environment_refs(request.environment_refs)
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
        "environmentRefs": [reference.packet() for reference in request.environment_refs],
        "pipeline": [
            {"command": command.command, "timeoutSeconds": command.timeout_seconds}
            for command in request.pipeline
        ],
        "acceptanceCriteria": list(request.acceptance_criteria),
        "plan": list(request.plan),
        "previousFailures": list(request.previous_failures),
        "currentCommit": request.current_commit,
        "diffSummary": request.diff_summary,
        "failedChecks": list(request.failed_checks),
        "reviewFindings": list(request.review_findings),
        "constraints": {
            "mayModifyFiles": request.may_modify_files,
            "mayPush": request.may_push,
            "mayMerge": request.may_merge,
        },
    }
