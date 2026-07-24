from collections.abc import Sequence
from typing import Protocol

from .github_cli import GitHubCliError, GitHubCliIssueTracker, GitHubRepository


class IssueTracker(Protocol):
    async def close_issue(self, external_issue_id: str) -> None: ...

    async def add_labels(self, external_issue_id: str, labels: Sequence[str]) -> None: ...


__all__ = ["GitHubCliError", "GitHubCliIssueTracker", "GitHubRepository", "IssueTracker"]
