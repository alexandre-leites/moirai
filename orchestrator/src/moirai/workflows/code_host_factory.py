from __future__ import annotations

import logging
import time
from typing import Any
from uuid import UUID

from moirai.code_hosts import CodeHost, GitHubCliCodeHost
from moirai.issue_trackers import GitHubRepository, IssueTracker
from moirai.issue_trackers.github_cli import CommandRunner, GitHubCliIssueTracker

_LOGGER = logging.getLogger(__name__)


class ProjectCodeHostFactory:
    """Resolves a project's code host / issue tracker from its own repository_url.

    Results are cached briefly per project so every graph node does not issue a
    query; the cache is both time-bound (so a project's repository can change
    without an orchestrator restart) and explicitly invalidatable.
    """

    def __init__(
        self,
        pool: Any,
        command_runner: CommandRunner | None = None,
        cache_ttl_seconds: float = 30.0,
    ) -> None:
        self._pool = pool
        self._command_runner = command_runner
        self._cache_ttl_seconds = cache_ttl_seconds
        self._cache: dict[str, tuple[float, GitHubRepository | None]] = {}

    def invalidate(self, project_id: str) -> None:
        self._cache.pop(project_id, None)

    async def code_host(self, project_id: str) -> CodeHost | None:
        repository = await self._repository(project_id)
        return GitHubCliCodeHost(repository, self._command_runner) if repository is not None else None

    async def issue_tracker(self, project_id: str) -> IssueTracker | None:
        repository = await self._repository(project_id)
        return GitHubCliIssueTracker(repository, self._command_runner) if repository is not None else None

    async def _repository(self, project_id: str) -> GitHubRepository | None:
        if not project_id:
            return None
        now = time.monotonic()
        cached = self._cache.get(project_id)
        if cached is not None and now - cached[0] < self._cache_ttl_seconds:
            return cached[1]
        try:
            project_uuid = UUID(project_id)
        except ValueError:
            _LOGGER.error("workflow references a project ID that is not a UUID: %s", project_id)
            self._cache[project_id] = (now, None)
            return None
        record = await self._pool.fetchrow(
            "SELECT repository_url FROM app.projects WHERE id = $1", project_uuid
        )
        repository: GitHubRepository | None = None
        raw_url = record["repository_url"] if record is not None else None
        if raw_url:
            try:
                repository = GitHubRepository.from_remote_url(str(raw_url))
            except ValueError:
                _LOGGER.error(
                    "project %s has a repository URL that could not be parsed as a GitHub remote: %s",
                    project_id,
                    raw_url,
                )
        self._cache[project_id] = (now, repository)
        return repository
