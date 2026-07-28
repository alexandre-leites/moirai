from __future__ import annotations

import unittest
from typing import Any
from uuid import uuid4

from moirai.code_hosts import GitHubCliCodeHost
from moirai.issue_trackers.github_cli import GitHubCliIssueTracker
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


if __name__ == "__main__":
    unittest.main()
