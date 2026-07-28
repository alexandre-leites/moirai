from typing import Protocol, Sequence

from moirai.issue_trackers.github_cli import GitHubCliError

from .github_cli import CheckStatus, GitHubCliCodeHost, PullRequest, PullRequestCheck, checks_pass


class CodeHost(Protocol):
    async def create_or_find_pull_request(
        self,
        workflow_id: str,
        branch: str,
        base_branch: str,
        title: str,
        issue_number: str | None = None,
    ) -> PullRequest: ...

    async def required_checks(self, pull_request_id: str) -> Sequence[PullRequestCheck]: ...

    async def enable_auto_merge(self, pull_request_id: str, method: str) -> None: ...

    async def merge_pull_request(self, pull_request_id: str, method: str) -> None: ...


__all__ = [
    "CheckStatus",
    "CodeHost",
    "GitHubCliCodeHost",
    "GitHubCliError",
    "PullRequest",
    "PullRequestCheck",
    "checks_pass",
]
