from __future__ import annotations

import asyncio
import json
import os
import re
from dataclasses import dataclass
from datetime import UTC, datetime
from typing import Any, Protocol, Sequence

from moirai.domain.issues import ExternalIssue


class GitHubCliError(RuntimeError):
    pass


class GitHubCliUnavailableError(GitHubCliError):
    pass


class CommandRunner(Protocol):
    async def run(self, command: Sequence[str], timeout_seconds: float) -> tuple[int, str, str]: ...


@dataclass(frozen=True)
class GitHubRepository:
    owner: str
    name: str

    @property
    def slug(self) -> str:
        return f"{self.owner}/{self.name}"

    @classmethod
    def from_remote_url(cls, remote_url: str) -> GitHubRepository:
        value = remote_url.strip()
        match = re.fullmatch(
            r"(?:https://github\.com/|git@github\.com:)([A-Za-z0-9_.-]+)/([A-Za-z0-9_.-]+?)(?:\.git)?/?",
            value,
        )
        if match is None:
            raise ValueError("project repository must be a GitHub remote URL")
        return cls(owner=match.group(1), name=match.group(2))


class SubprocessCommandRunner:
    def __init__(self, github_token: str | None = None) -> None:
        self._github_token = github_token

    async def run(self, command: Sequence[str], timeout_seconds: float) -> tuple[int, str, str]:
        env = {**os.environ, "GH_TOKEN": self._github_token} if self._github_token else None
        try:
            process = await asyncio.create_subprocess_exec(
                *command,
                stdout=asyncio.subprocess.PIPE,
                stderr=asyncio.subprocess.PIPE,
                env=env,
            )
        except FileNotFoundError as error:
            raise GitHubCliUnavailableError("GitHub CLI (gh) is not installed in this image") from error
        try:
            stdout, stderr = await asyncio.wait_for(process.communicate(), timeout_seconds)
        except TimeoutError:
            process.kill()
            await process.wait()
            raise GitHubCliError("GitHub CLI command timed out") from None
        return process.returncode or 0, stdout.decode(), stderr.decode()


async def verify_gh_ready(runner: CommandRunner | None = None, timeout_seconds: float = 10.0) -> None:
    """Fail loudly at startup if the GitHub CLI is absent or unauthenticated.

    Without this, a missing/unauthenticated `gh` is only discovered when the
    first workflow tries to open a pull request or sync issues.
    """
    active_runner = runner or SubprocessCommandRunner()
    try:
        code, _, stderr = await active_runner.run(("gh", "auth", "status"), timeout_seconds)
    except GitHubCliUnavailableError:
        raise
    except GitHubCliError as error:
        raise GitHubCliUnavailableError(f"GitHub CLI readiness check failed: {error}") from error
    if code != 0:
        raise GitHubCliUnavailableError(
            _redact(f"GitHub CLI is not authenticated: {stderr.strip() or 'gh auth status failed'}")
        )


class GitHubCliIssueTracker:
    def __init__(
        self,
        repository: GitHubRepository,
        runner: CommandRunner | None = None,
        timeout_seconds: float = 30.0,
    ) -> None:
        self._repository = repository
        self._runner = runner or SubprocessCommandRunner()
        self._timeout_seconds = timeout_seconds

    async def list_open_issues(self) -> list[ExternalIssue]:
        fields = "number,title,body,state,labels,createdAt,updatedAt"
        payload = await self._json("issue", "list", "--repo", self._repository.slug, "--state", "open", "--json", fields)
        if not isinstance(payload, list):
            raise GitHubCliError("GitHub CLI issue list response is not an array")
        return [self._issue_from_json(item) for item in payload]

    async def add_labels(self, external_issue_id: str, labels: Sequence[str]) -> None:
        if not labels:
            return
        await self._run("issue", "edit", external_issue_id, "--repo", self._repository.slug, *self._label_args("--add-label", labels))

    async def remove_labels(self, external_issue_id: str, labels: Sequence[str]) -> None:
        if not labels:
            return
        await self._run("issue", "edit", external_issue_id, "--repo", self._repository.slug, *self._label_args("--remove-label", labels))

    async def close_issue(self, external_issue_id: str) -> None:
        await self._run("issue", "close", external_issue_id, "--repo", self._repository.slug)

    async def _json(self, *arguments: str) -> Any:
        stdout = await self._run(*arguments)
        try:
            return json.loads(stdout)
        except json.JSONDecodeError as error:
            raise GitHubCliError("GitHub CLI returned invalid JSON") from error

    async def _run(self, *arguments: str) -> str:
        code, stdout, stderr = await self._runner.run(("gh", *arguments), self._timeout_seconds)
        if code != 0:
            detail = stderr.strip() or "GitHub CLI failed without stderr"
            raise GitHubCliError(_redact(detail))
        return stdout

    @staticmethod
    def _label_args(flag: str, labels: Sequence[str]) -> tuple[str, ...]:
        arguments: list[str] = []
        for label in labels:
            arguments.extend((flag, label))
        return tuple(arguments)

    @staticmethod
    def _issue_from_json(value: Any) -> ExternalIssue:
        if not isinstance(value, dict):
            raise GitHubCliError("GitHub CLI issue item is not an object")
        labels = value.get("labels", [])
        if not isinstance(labels, list):
            raise GitHubCliError("GitHub CLI issue labels are invalid")
        names = tuple(label["name"] for label in labels if isinstance(label, dict) and isinstance(label.get("name"), str))
        try:
            return ExternalIssue(
                external_id=str(value["number"]),
                title=str(value["title"]),
                body=str(value.get("body") or ""),
                state=str(value["state"]),
                labels=names,
                created_at=datetime.fromisoformat(str(value["createdAt"]).replace("Z", "+00:00")).astimezone(UTC),
                updated_at=datetime.fromisoformat(str(value["updatedAt"]).replace("Z", "+00:00")).astimezone(UTC),
            )
        except (KeyError, TypeError, ValueError) as error:
            raise GitHubCliError("GitHub CLI issue item is missing required fields") from error


def _redact(value: str) -> str:
    for prefix in ("ghp_", "github_pat_"):
        start = value.find(prefix)
        if start >= 0:
            end = start + len(prefix)
            while end < len(value) and (value[end].isalnum() or value[end] in "_-"):
                end += 1
            value = f"{value[:start]}[REDACTED]{value[end:]}"
    return value
