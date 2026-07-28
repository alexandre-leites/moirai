from collections.abc import Sequence
from typing import Protocol

from .github_cli import GitHubCliError, GitHubCliIssueTracker, GitHubRepository


class IssueTracker(Protocol):
    async def list_eligible_issues(self) -> Sequence[object]: ...

    async def get_issue(self, external_issue_id: str) -> object: ...

    async def close_issue(self, external_issue_id: str) -> None: ...

    async def add_labels(self, external_issue_id: str, labels: Sequence[str]) -> None: ...

    async def remove_labels(self, external_issue_id: str, labels: Sequence[str]) -> None: ...

    async def add_comment(self, external_issue_id: str, body: str, idempotency_key: str) -> None: ...


__all__ = ["GitHubCliError", "GitHubCliIssueTracker", "GitHubRepository", "IssueTracker"]
