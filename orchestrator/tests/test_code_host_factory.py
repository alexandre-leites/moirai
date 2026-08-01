from __future__ import annotations

import unittest
from collections.abc import Awaitable, Callable
from typing import Any, cast
from uuid import uuid4

from moirai.code_hosts import GitHubCliCodeHost
from moirai.issue_trackers.github_cli import (
    CommandRunner,
    GitHubCliIssueTracker,
    SubprocessCommandRunner,
)
from moirai.persistence.secrets import SecretCipherError
from moirai.workflows.code_host_factory import ProjectCodeHostFactory


class _FakePool:
    def __init__(self) -> None:
        self.repository_urls: dict[str, str | None] = {}
        self.calls = 0

    async def fetchrow(self, query: str, project_id: Any) -> dict[str, Any] | None:
        self.calls += 1
        assert "SELECT repository_url" in query
        url = self.repository_urls.get(str(project_id))
        if url is None and str(project_id) not in self.repository_urls:
            return None
        return {"repository_url": url}


class ProjectCodeHostFactoryTests(unittest.IsolatedAsyncioTestCase):
    async def test_resolves_code_host_from_the_projects_own_repository_url(self) -> None:
        project_id = str(uuid4())
        pool = _FakePool()
        pool.repository_urls[project_id] = "https://github.com/acme/widgets.git"
        factory = ProjectCodeHostFactory(pool)

        code_host = await factory.code_host(project_id)

        self.assertIsInstance(code_host, GitHubCliCodeHost)
        self.assertEqual(code_host._repository.slug, "acme/widgets")

    async def test_uses_the_configured_github_command_runner(self) -> None:
        project_id = str(uuid4())
        pool = _FakePool()
        pool.repository_urls[project_id] = "https://github.com/acme/widgets.git"
        runner = cast(CommandRunner, object())
        factory = ProjectCodeHostFactory(pool, command_runner=runner)

        code_host = await factory.code_host(project_id)
        issue_tracker = await factory.issue_tracker(project_id)

        assert code_host is not None and issue_tracker is not None
        self.assertIs(code_host._runner, runner)
        self.assertIs(issue_tracker._runner, runner)

    async def test_resolves_issue_tracker_from_the_projects_own_repository_url(self) -> None:
        project_id = str(uuid4())
        pool = _FakePool()
        pool.repository_urls[project_id] = "https://github.com/acme/widgets.git"
        factory = ProjectCodeHostFactory(pool)

        issue_tracker = await factory.issue_tracker(project_id)

        self.assertIsInstance(issue_tracker, GitHubCliIssueTracker)

    async def test_two_projects_resolve_to_their_own_distinct_repositories(self) -> None:
        pool = _FakePool()
        first, second = str(uuid4()), str(uuid4())
        pool.repository_urls[first] = "https://github.com/acme/widgets.git"
        pool.repository_urls[second] = "https://github.com/other-org/gadgets.git"
        factory = ProjectCodeHostFactory(pool)

        first_host = await factory.code_host(first)
        second_host = await factory.code_host(second)

        assert first_host is not None and second_host is not None
        self.assertEqual(first_host._repository.slug, "acme/widgets")
        self.assertEqual(second_host._repository.slug, "other-org/gadgets")

    async def test_missing_project_returns_none_without_raising(self) -> None:
        pool = _FakePool()
        factory = ProjectCodeHostFactory(pool)
        self.assertIsNone(await factory.code_host(str(uuid4())))

    async def test_unparseable_repository_url_returns_none_without_raising(self) -> None:
        project_id = str(uuid4())
        pool = _FakePool()
        pool.repository_urls[project_id] = "not a github url"
        factory = ProjectCodeHostFactory(pool)
        self.assertIsNone(await factory.code_host(project_id))

    async def test_non_uuid_project_id_returns_none_without_raising(self) -> None:
        pool = _FakePool()
        factory = ProjectCodeHostFactory(pool)
        self.assertIsNone(await factory.code_host("not-a-uuid"))

    async def test_result_is_cached_within_the_ttl(self) -> None:
        project_id = str(uuid4())
        pool = _FakePool()
        pool.repository_urls[project_id] = "https://github.com/acme/widgets.git"
        factory = ProjectCodeHostFactory(pool, cache_ttl_seconds=60.0)

        await factory.code_host(project_id)
        await factory.code_host(project_id)

        self.assertEqual(pool.calls, 1)

    async def test_invalidate_forces_a_fresh_lookup(self) -> None:
        project_id = str(uuid4())
        pool = _FakePool()
        pool.repository_urls[project_id] = "https://github.com/acme/widgets.git"
        factory = ProjectCodeHostFactory(pool, cache_ttl_seconds=60.0)

        await factory.code_host(project_id)
        factory.invalidate(project_id)
        pool.repository_urls[project_id] = "https://github.com/acme/renamed.git"
        code_host = await factory.code_host(project_id)

        self.assertEqual(pool.calls, 2)
        assert code_host is not None
        self.assertEqual(code_host._repository.slug, "acme/renamed")


class PerProjectCredentialTests(unittest.IsolatedAsyncioTestCase):
    """A project's own GitHub token has to reach the `gh` calls made for it.

    This is what makes a private repository work: without it every project is
    reached with the one deployment-wide token, which is the case that failed.
    """

    def _factory(
        self, project_id: str, reader: Callable[[str], Awaitable[str | None]], shared: CommandRunner | None = None
    ) -> ProjectCodeHostFactory:
        pool = _FakePool()
        pool.repository_urls[project_id] = "https://github.com/acme/widgets.git"
        return ProjectCodeHostFactory(pool, command_runner=shared, credential_reader=reader)

    async def test_the_projects_own_token_is_what_gh_is_invoked_with(self) -> None:
        project_id = str(uuid4())

        async def reader(_: str) -> str | None:
            return "ghp_project_scoped"

        factory = self._factory(project_id, reader, shared=SubprocessCommandRunner("ghp_deployment_wide"))
        code_host = await factory.code_host(project_id)
        issue_tracker = await factory.issue_tracker(project_id)

        assert code_host is not None and issue_tracker is not None
        self.assertEqual(cast(Any, code_host._runner)._github_token, "ghp_project_scoped")
        self.assertEqual(cast(Any, issue_tracker._runner)._github_token, "ghp_project_scoped")

    async def test_a_project_without_one_keeps_using_the_deployment_wide_runner(self) -> None:
        project_id = str(uuid4())
        shared = cast(CommandRunner, object())

        async def reader(_: str) -> str | None:
            return None

        factory = self._factory(project_id, reader, shared=shared)
        code_host = await factory.code_host(project_id)

        assert code_host is not None
        self.assertIs(code_host._runner, shared)

    async def test_two_projects_do_not_borrow_each_others_tokens(self) -> None:
        first, second = str(uuid4()), str(uuid4())
        tokens = {first: "ghp_first", second: "ghp_second"}
        pool = _FakePool()
        pool.repository_urls[first] = "https://github.com/acme/first.git"
        pool.repository_urls[second] = "https://github.com/acme/second.git"

        async def reader(project_id: str) -> str | None:
            return tokens[project_id]

        factory = ProjectCodeHostFactory(pool, credential_reader=reader)
        first_host = await factory.code_host(first)
        second_host = await factory.code_host(second)

        assert first_host is not None and second_host is not None
        self.assertEqual(cast(Any, first_host._runner)._github_token, "ghp_first")
        self.assertEqual(cast(Any, second_host._runner)._github_token, "ghp_second")

    async def test_a_credential_that_cannot_be_opened_is_raised_not_swallowed(self) -> None:
        # Falling back here would reach GitHub as the deployment-wide identity
        # and report a 404, sending the operator after a permissions problem
        # that does not exist. The real reason has to reach the sync error.
        project_id = str(uuid4())

        async def reader(_: str) -> str | None:
            raise SecretCipherError("stored secret could not be opened")

        factory = self._factory(project_id, reader, shared=cast(CommandRunner, object()))
        with self.assertRaises(SecretCipherError):
            await factory.code_host(project_id)

    async def test_the_credential_is_read_once_per_ttl_not_once_per_call(self) -> None:
        project_id = str(uuid4())
        reads = 0

        async def reader(_: str) -> str | None:
            nonlocal reads
            reads += 1
            return "ghp_project_scoped"

        factory = self._factory(project_id, reader)
        await factory.code_host(project_id)
        await factory.issue_tracker(project_id)
        self.assertEqual(reads, 1)

        # Rotating a token in the console must not need a restart to take hold.
        factory.invalidate(project_id)
        await factory.code_host(project_id)
        self.assertEqual(reads, 2)

    async def test_without_a_reader_configured_nothing_changes(self) -> None:
        project_id = str(uuid4())
        shared = cast(CommandRunner, object())
        pool = _FakePool()
        pool.repository_urls[project_id] = "https://github.com/acme/widgets.git"
        factory = ProjectCodeHostFactory(pool, command_runner=shared)

        code_host = await factory.code_host(project_id)

        assert code_host is not None
        self.assertIs(code_host._runner, shared)


if __name__ == "__main__":
    unittest.main()
