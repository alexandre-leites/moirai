from __future__ import annotations

import logging
import time
from collections.abc import Awaitable, Callable
from typing import Any
from uuid import UUID

from moirai.code_hosts import CodeHost, GitHubCliCodeHost
from moirai.issue_trackers import GitHubRepository, IssueTracker
from moirai.issue_trackers.github_cli import (
    CommandRunner,
    GitHubCliIssueTracker,
    SubprocessCommandRunner,
)

_LOGGER = logging.getLogger(__name__)


class ProjectCodeHostFactory:
    """Resolves a project's code host / issue tracker from its own repository_url.

    Results are cached briefly per project so every graph node does not issue a
    query; the cache is both time-bound (so a project's repository can change
    without an orchestrator restart) and explicitly invalidatable.

    Each project also gets a command runner carrying its own GitHub credential
    when one is configured, so a private repository is reached as an identity
    scoped to it rather than with one deployment-wide token that can read every
    project. Projects without a credential keep using the shared runner.
    """

    def __init__(
        self,
        pool: Any,
        command_runner: CommandRunner | None = None,
        cache_ttl_seconds: float = 30.0,
        credential_reader: Callable[[str], Awaitable[str | None]] | None = None,
    ) -> None:
        self._pool = pool
        self._command_runner = command_runner
        self._cache_ttl_seconds = cache_ttl_seconds
        self._cache: dict[str, tuple[float, GitHubRepository | None]] = {}
        self._credential_reader = credential_reader
        self._runner_cache: dict[str, tuple[float, CommandRunner | None]] = {}

    def invalidate(self, project_id: str) -> None:
        self._cache.pop(project_id, None)
        self._runner_cache.pop(project_id, None)

    async def code_host(self, project_id: str) -> CodeHost | None:
        repository = await self._repository(project_id)
        if repository is None:
            return None
        return GitHubCliCodeHost(repository, await self._runner_for(project_id))

    async def issue_tracker(self, project_id: str) -> IssueTracker | None:
        repository = await self._repository(project_id)
        if repository is None:
            return None
        return GitHubCliIssueTracker(repository, await self._runner_for(project_id))

    async def _runner_for(self, project_id: str) -> CommandRunner | None:
        """The command runner this project's `gh` calls should use.

        A project with no credential of its own falls back to the shared runner.
        A project whose credential cannot be *opened* does not: that means the
        deployment's key changed or the row was altered, and quietly retrying as
        the deployment-wide identity would reach GitHub as the wrong user and
        report itself as a permissions problem. Raising instead puts the real
        reason in the project's sync error, where an operator will see it.
        """
        if self._credential_reader is None:
            return self._command_runner
        now = time.monotonic()
        cached = self._runner_cache.get(project_id)
        if cached is not None and now - cached[0] < self._cache_ttl_seconds:
            return cached[1] or self._command_runner
        token = await self._credential_reader(project_id)
        runner = SubprocessCommandRunner(token) if token else None
        self._runner_cache[project_id] = (now, runner)
        return runner or self._command_runner

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
