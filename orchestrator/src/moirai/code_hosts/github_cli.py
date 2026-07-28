from __future__ import annotations

import json
import logging
from collections.abc import Sequence
from dataclasses import dataclass
from enum import StrEnum
from typing import Any

from moirai.issue_trackers.github_cli import (
    CommandRunner,
    GitHubCliError,
    GitHubRepository,
    SubprocessCommandRunner,
    _redact,
)

_LOGGER = logging.getLogger(__name__)


class CheckStatus(StrEnum):
    PENDING = "pending"
    PASSING = "passing"
    FAILING = "failing"
    SKIPPED = "skipped"
    CANCELLED = "cancelled"


class ChecksResult(StrEnum):
    PENDING = "pending"
    PASSED = "passed"
    FAILED = "failed"


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
    required: bool = True


@dataclass(frozen=True)
class PullRequestReview:
    author: str
    state: str
    body: str


def checks_result(checks: Sequence[PullRequestCheck]) -> ChecksResult:
    if not checks:
        return ChecksResult.PENDING
    pending = False
    for check in checks:
        if check.status is CheckStatus.PASSING:
            continue
        if check.status is CheckStatus.SKIPPED and not check.required:
            continue
        if check.status is CheckStatus.PENDING:
            pending = True
            continue
        return ChecksResult.FAILED
    return ChecksResult.PENDING if pending else ChecksResult.PASSED


def checks_pass(checks: Sequence[PullRequestCheck]) -> bool:
    """The single pass/fail policy for a pull request's checks, shared by
    the workflow's wait_for_checks gate and merge_pull_request's own guard
    so the two layers can never disagree.

    An empty check list does not pass (no CI configured is not the same as
    CI having passed). A skipped check passes only if it is not required;
    a skipped required check does not pass.
    """
    return checks_result(checks) is ChecksResult.PASSED


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

    async def get_pull_request(self, pull_request_id: str) -> PullRequest:
        payload = await self._json(
            "pr",
            "view",
            pull_request_id,
            "--repo",
            self._repository.slug,
            "--json",
            "number,url,state,headRefName,headRefOid",
        )
        return self._pull_request_from_json(payload)

    async def create_or_find_branch(self, branch: str, base_branch: str) -> str:
        try:
            existing = await self._json("api", f"repos/{self._repository.slug}/git/ref/heads/{branch}")
        except GitHubCliError:
            existing = None
        if isinstance(existing, dict):
            return branch
        base = await self._json("api", f"repos/{self._repository.slug}/git/ref/heads/{base_branch}")
        object_data = base.get("object") if isinstance(base, dict) else None
        sha = object_data.get("sha") if isinstance(object_data, dict) else None
        if not isinstance(sha, str) or not sha:
            raise GitHubCliError("GitHub default branch reference is invalid")
        await self._run(
            "api", f"repos/{self._repository.slug}/git/refs", "-X", "POST",
            "-f", f"ref=refs/heads/{branch}", "-f", f"sha={sha}",
        )
        return branch

    async def push_branch(self, branch: str, commit_sha: str) -> None:
        if not branch or not commit_sha:
            raise ValueError("branch and commit SHA are required")
        await self._run(
            "api", f"repos/{self._repository.slug}/git/refs/heads/{branch}", "-X", "PATCH",
            "-f", f"sha={commit_sha}", "-f", "force=false",
        )

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
            "name,bucket,link,state,isRequired",
        )
        if not isinstance(payload, list):
            raise GitHubCliError("GitHub CLI pull request checks response is not an array")
        return [self._check_from_json(check) for check in payload]

    async def update_pull_request(self, pull_request_id: str, title: str, body: str) -> None:
        if not title.strip():
            raise ValueError("pull request title is required")
        await self._run(
            "pr", "edit", pull_request_id, "--repo", self._repository.slug, "--title", title, "--body", body
        )

    async def get_pull_request_reviews(self, pull_request_id: str) -> list[PullRequestReview]:
        payload = await self._json(
            "pr", "view", pull_request_id, "--repo", self._repository.slug, "--json", "reviews"
        )
        reviews = payload.get("reviews", []) if isinstance(payload, dict) else []
        if not isinstance(reviews, list):
            raise GitHubCliError("GitHub CLI pull request reviews are invalid")
        result: list[PullRequestReview] = []
        for review in reviews:
            if not isinstance(review, dict):
                raise GitHubCliError("GitHub CLI pull request review is invalid")
            author = review.get("author")
            login = author.get("login") if isinstance(author, dict) else None
            state = review.get("state")
            body = review.get("body", "")
            if not isinstance(login, str) or not isinstance(state, str) or not isinstance(body, str):
                raise GitHubCliError("GitHub CLI pull request review is invalid")
            result.append(PullRequestReview(login, state, body))
        return result

    async def close_pull_request(self, pull_request_id: str) -> None:
        await self._run("pr", "close", pull_request_id, "--repo", self._repository.slug)

    async def get_default_branch_head(self) -> str:
        repository = await self._json("api", f"repos/{self._repository.slug}")
        default_branch = repository.get("default_branch") if isinstance(repository, dict) else None
        if not isinstance(default_branch, str) or not default_branch:
            raise GitHubCliError("GitHub CLI repository default branch is invalid")
        reference = await self._json("api", f"repos/{self._repository.slug}/git/ref/heads/{default_branch}")
        object_data = reference.get("object") if isinstance(reference, dict) else None
        sha = object_data.get("sha") if isinstance(object_data, dict) else None
        if not isinstance(sha, str) or not sha:
            raise GitHubCliError("GitHub CLI repository default branch head is invalid")
        return sha

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
        if not checks_pass(checks):
            raise GitHubCliError("refusing to merge pull request before every required check is passing")
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
        required = value.get("isRequired")
        if not isinstance(required, bool):
            required = True
        return PullRequestCheck(name=value["name"], status=status, url=link, required=required)

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
        status = statuses.get(normalized)
        if status is None:
            _LOGGER.warning("GitHub CLI returned an unrecognized check state: %s", normalized)
            return CheckStatus.PENDING
        return status
