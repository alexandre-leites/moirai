from __future__ import annotations

import json
from dataclasses import dataclass
from enum import StrEnum
from typing import Any, Sequence

from moirai.issue_trackers.github_cli import (
    CommandRunner,
    GitHubCliError,
    GitHubRepository,
    SubprocessCommandRunner,
    _redact,
)


class CheckStatus(StrEnum):
    PENDING = "pending"
    PASSING = "passing"
    FAILING = "failing"
    SKIPPED = "skipped"
    CANCELLED = "cancelled"


@dataclass(frozen=True)
class PullRequest:
    external_id: str
    url: str
    state: str
    head_branch: str
    head_commit: str


@dataclass(frozen=True)
class PullRequestCheck:
    name: str
    status: CheckStatus
    url: str | None = None


class GitHubCliCodeHost:
    def __init__(
        self,
        repository: GitHubRepository,
        runner: CommandRunner | None = None,
        timeout_seconds: float = 30.0,
    ) -> None:
        self._repository = repository
        self._runner = runner or SubprocessCommandRunner()
        self._timeout_seconds = timeout_seconds

    async def find_pull_request(self, branch: str) -> PullRequest | None:
        payload = await self._json(
            "pr",
            "list",
            "--repo",
            self._repository.slug,
            "--head",
            branch,
            "--state",
            "all",
            "--limit",
            "1",
            "--json",
            "number,url,state,headRefName,headRefOid",
        )
        if not isinstance(payload, list):
            raise GitHubCliError("GitHub CLI pull request list response is not an array")
        if not payload:
            return None
        return self._pull_request_from_json(payload[0])

    async def create_or_find_pull_request(
        self,
        workflow_id: str,
        branch: str,
        base_branch: str,
        title: str,
        issue_number: str | None = None,
    ) -> PullRequest:
        existing = await self.find_pull_request(branch)
        if existing is not None:
            return existing
        marker = f"<!-- loop-engineering-workflow:{workflow_id} -->"
        closes_issue = f"\n\nCloses #{issue_number}" if issue_number else ""
        body = f"{marker}{closes_issue}"
        await self._run(
            "pr",
            "create",
            "--repo",
            self._repository.slug,
            "--head",
            branch,
            "--base",
            base_branch,
            "--title",
            title,
            "--body",
            body,
        )
        created = await self.find_pull_request(branch)
        if created is None:
            raise GitHubCliError("GitHub CLI created a pull request that could not be found")
        return created

    async def required_checks(self, pull_request_id: str) -> list[PullRequestCheck]:
        payload = await self._json(
            "pr",
            "checks",
            pull_request_id,
            "--repo",
            self._repository.slug,
            "--json",
            "name,bucket,link,state",
        )
        if not isinstance(payload, list):
            raise GitHubCliError("GitHub CLI pull request checks response is not an array")
        return [self._check_from_json(check) for check in payload]

    async def enable_auto_merge(self, pull_request_id: str, method: str) -> None:
        await self._run(
            "pr",
            "merge",
            pull_request_id,
            "--repo",
            self._repository.slug,
            self._merge_flag(method),
            "--auto",
        )

    async def merge_pull_request(self, pull_request_id: str, method: str) -> None:
        checks = await self.required_checks(pull_request_id)
        if any(check.status is not CheckStatus.PASSING for check in checks):
            raise GitHubCliError("refusing to merge pull request before every reported check is passing")
        await self._run(
            "pr",
            "merge",
            pull_request_id,
            "--repo",
            self._repository.slug,
            self._merge_flag(method),
        )

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
    def _merge_flag(method: str) -> str:
        flags = {"merge": "--merge", "rebase": "--rebase", "squash": "--squash"}
        try:
            return flags[method]
        except KeyError as error:
            raise ValueError("merge method must be merge, rebase, or squash") from error

    @staticmethod
    def _pull_request_from_json(value: Any) -> PullRequest:
        if not isinstance(value, dict):
            raise GitHubCliError("GitHub CLI pull request item is not an object")
        try:
            return PullRequest(
                external_id=str(value["number"]),
                url=str(value["url"]),
                state=str(value["state"]),
                head_branch=str(value["headRefName"]),
                head_commit=str(value["headRefOid"]),
            )
        except (KeyError, TypeError, ValueError) as error:
            raise GitHubCliError("GitHub CLI pull request item is missing required fields") from error

    @staticmethod
    def _check_from_json(value: Any) -> PullRequestCheck:
        if not isinstance(value, dict) or not isinstance(value.get("name"), str):
            raise GitHubCliError("GitHub CLI pull request check is invalid")
        status = GitHubCliCodeHost._check_status(value.get("bucket"), value.get("state"))
        link = value.get("link")
        if link is not None and not isinstance(link, str):
            raise GitHubCliError("GitHub CLI pull request check link is invalid")
        return PullRequestCheck(name=value["name"], status=status, url=link)

    @staticmethod
    def _check_status(bucket: object, state: object) -> CheckStatus:
        normalized = str(bucket or state).lower()
        statuses = {
            "pass": CheckStatus.PASSING,
            "success": CheckStatus.PASSING,
            "pending": CheckStatus.PENDING,
            "queued": CheckStatus.PENDING,
            "in_progress": CheckStatus.PENDING,
            "waiting": CheckStatus.PENDING,
            "fail": CheckStatus.FAILING,
            "failure": CheckStatus.FAILING,
            "error": CheckStatus.FAILING,
            "skipping": CheckStatus.SKIPPED,
            "skipped": CheckStatus.SKIPPED,
            "neutral": CheckStatus.SKIPPED,
            "canceling": CheckStatus.CANCELLED,
            "cancelled": CheckStatus.CANCELLED,
            "canceled": CheckStatus.CANCELLED,
        }
        try:
            return statuses[normalized]
        except KeyError as error:
            raise GitHubCliError(f"GitHub CLI returned an unknown check state: {normalized}") from error
