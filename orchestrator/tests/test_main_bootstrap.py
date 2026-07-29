from __future__ import annotations

import os
import unittest
from collections.abc import Iterable
from datetime import UTC, datetime, timedelta
from hashlib import sha256
from typing import Any
from unittest.mock import patch
from uuid import uuid4

from moirai.main import _bootstrap_initial_setup, _seed_issue_if_needed
from moirai.persistence.authentication import AsyncpgAuthentication

_SEED_TOKEN_VALUE = "a-real-secret-value"
_SEED_TOKEN_HASH = sha256(_SEED_TOKEN_VALUE.encode()).hexdigest()

# The environment a fully configured deployment starts with; individual tests
# drop or override one variable at a time to describe a partial configuration.
_CONFIGURED_ENVIRONMENT = {
    "LOOP_INITIAL_ADMIN_PASSWORD": "correct horse battery staple",
    "RUNNER_REGISTRATION_TOKEN": _SEED_TOKEN_VALUE,
}


def _environment(*, without: Iterable[str] = (), **overrides: str) -> Any:
    removed = set(without)
    values = {
        name: value
        for name, value in {**_CONFIGURED_ENVIRONMENT, **overrides}.items()
        if name not in removed
    }
    return patch.dict(os.environ, values, clear=True)


class _FakePool:
    """Stands in for the asyncpg pool, modelling only the rows bootstrap reads and writes."""

    def __init__(
        self,
        *,
        users: int = 0,
        projects: Iterable[str] = (),
        token_hashes: Iterable[str] = (),
    ) -> None:
        self.user_count = users
        self.projects: dict[str, str] = {name: str(uuid4()) for name in projects}
        self.token_hashes: set[str] = set(token_hashes)
        self.seeded_issue_project_ids: set[str] = set()
        self.executed: list[tuple[str, tuple[object, ...]]] = []

    async def fetchval(self, query: str, *args: object) -> object:
        if "COUNT(*) FROM app.users" in query:
            return self.user_count
        if "COUNT(*) FROM app.projects WHERE name" in query:
            return 1 if args[0] in self.projects else 0
        if "id FROM app.projects WHERE name" in query:
            return self.projects.get(str(args[0]))
        if "COUNT(*) FROM app.runner_registration_tokens" in query:
            return 1 if args[0] in self.token_hashes else 0
        if "COUNT(*) FROM app.issues" in query:
            return 1 if str(args[0]) in self.seeded_issue_project_ids else 0
        raise AssertionError(query)

    async def execute(self, query: str, *args: object) -> str:
        self.executed.append((query, args))
        if "INSERT INTO app.users" in query:
            self.user_count += 1
        elif "INSERT INTO app.projects" in query:
            self.projects[str(args[1])] = str(args[0])
        elif "INSERT INTO app.runner_registration_tokens" in query:
            self.token_hashes.add(str(args[1]))
        elif "INSERT INTO app.issues" in query:
            self.seeded_issue_project_ids.add(str(args[1]))
        else:
            raise AssertionError(query)
        return "INSERT 0 1"

    def inserts(self, table: str) -> list[tuple[object, ...]]:
        marker = f"INSERT INTO app.{table}"
        return [args for query, args in self.executed if marker in query]


class BootstrapPartialStateTests(unittest.IsolatedAsyncioTestCase):
    """Every bootstrap step must be able to run without the others having run.

    A first start that crashed — or that ran before a secret was configured —
    leaves the database part-seeded, and the orchestrator has to be able to
    finish the job on the next start rather than failing or silently skipping.
    """

    async def test_completes_the_remaining_steps_when_the_seed_project_exists_without_users(
        self,
    ) -> None:
        # Regression for the NameError raised when uuid4 was imported inside the
        # branch that creates the project but used to mint the token id below it.
        pool = _FakePool(projects=["demo"])

        with _environment():
            await _bootstrap_initial_setup(pool)

        self.assertEqual(len(pool.inserts("users")), 1)
        self.assertEqual(pool.inserts("projects"), [], "the existing project must not be duplicated")
        self.assertEqual(len(pool.inserts("runner_registration_tokens")), 1)

    async def test_seeds_the_project_and_token_when_the_admin_user_already_exists(self) -> None:
        pool = _FakePool(users=1)

        with _environment():
            await _bootstrap_initial_setup(pool)

        self.assertEqual(pool.inserts("users"), [], "an existing user must not be re-created")
        self.assertEqual(len(pool.inserts("projects")), 1)
        self.assertEqual(len(pool.inserts("runner_registration_tokens")), 1)

    async def test_seeds_the_project_and_token_when_the_admin_password_is_unset(self) -> None:
        pool = _FakePool()

        with _environment(without=("LOOP_INITIAL_ADMIN_PASSWORD",)):
            await _bootstrap_initial_setup(pool)

        self.assertEqual(pool.inserts("users"), [])
        self.assertEqual(len(pool.inserts("projects")), 1)
        self.assertEqual(len(pool.inserts("runner_registration_tokens")), 1)

    async def test_seeds_the_registration_token_when_it_is_the_only_missing_row(self) -> None:
        pool = _FakePool(users=1, projects=["demo"])

        with _environment():
            await _bootstrap_initial_setup(pool)

        self.assertEqual(pool.inserts("users"), [])
        self.assertEqual(pool.inserts("projects"), [])
        self.assertEqual(len(pool.inserts("runner_registration_tokens")), 1)

    async def test_creates_every_seed_row_on_an_empty_database(self) -> None:
        pool = _FakePool()

        with _environment():
            await _bootstrap_initial_setup(pool)

        self.assertEqual(len(pool.inserts("users")), 1)
        self.assertEqual(len(pool.inserts("projects")), 1)
        self.assertEqual(len(pool.inserts("runner_registration_tokens")), 1)

    async def test_a_repeated_bootstrap_writes_nothing(self) -> None:
        pool = _FakePool()

        with _environment():
            await _bootstrap_initial_setup(pool)
            after_first_run = list(pool.executed)
            await _bootstrap_initial_setup(pool)

        self.assertEqual(pool.executed, after_first_run)


class BootstrapAdminUserRaceTests(unittest.IsolatedAsyncioTestCase):
    """A second instance starting against the same empty database loses the insert race."""

    async def test_continues_when_another_instance_created_the_first_user(self) -> None:
        pool = _FakePool()

        async def losing_create_user(_self: object, *args: object, **kwargs: object) -> str:
            pool.user_count = 1
            raise ValueError("username is already in use")

        with _environment(), patch.object(AsyncpgAuthentication, "create_user", losing_create_user):
            await _bootstrap_initial_setup(pool)

        self.assertEqual(pool.inserts("users"), [])
        self.assertEqual(len(pool.inserts("projects")), 1)
        self.assertEqual(len(pool.inserts("runner_registration_tokens")), 1)

    async def test_reraises_when_the_user_table_is_still_empty(self) -> None:
        pool = _FakePool()

        async def failing_create_user(_self: object, *args: object, **kwargs: object) -> str:
            raise ValueError("password must contain between 12 and 1024 characters")

        with _environment(), patch.object(AsyncpgAuthentication, "create_user", failing_create_user):
            with self.assertRaises(ValueError):
                await _bootstrap_initial_setup(pool)


class BootstrapRegistrationTokenTests(unittest.IsolatedAsyncioTestCase):
    async def test_skips_seeding_when_registration_token_env_var_is_unset(self) -> None:
        pool = _FakePool()

        with _environment(without=("RUNNER_REGISTRATION_TOKEN",)):
            await _bootstrap_initial_setup(pool)

        self.assertEqual(
            pool.inserts("runner_registration_tokens"),
            [],
            "must not seed a registration token when RUNNER_REGISTRATION_TOKEN is unset",
        )

    async def test_seeds_token_hash_for_the_same_variable_compose_gives_the_runner(self) -> None:
        pool = _FakePool()

        with _environment():
            before = datetime.now(UTC)
            await _bootstrap_initial_setup(pool)
            after = datetime.now(UTC)

        inserts = pool.inserts("runner_registration_tokens")
        self.assertEqual(len(inserts), 1)
        _token_id, token_hash, _labels_json, created_at, expires_at = inserts[0]
        self.assertEqual(token_hash, _SEED_TOKEN_HASH)

        # TTL must be a short, fixed duration via timedelta arithmetic — not
        # now.replace(year=now.year + 10), which both mismatches the API-issued token
        # TTL and raises ValueError if evaluated on a leap day.
        ttl = expires_at - created_at
        self.assertEqual(ttl, timedelta(minutes=15))
        self.assertTrue(before <= created_at <= after)

    async def test_does_not_reseed_a_hash_that_is_already_present(self) -> None:
        # The existence check deliberately ignores used_at/expires_at: registration
        # tokens are single-use, so re-seeding a spent hash would re-open it.
        pool = _FakePool(users=1, projects=["demo"], token_hashes=[_SEED_TOKEN_HASH])

        with _environment():
            await _bootstrap_initial_setup(pool)

        self.assertEqual(pool.executed, [])


class BootstrapSeedProjectTests(unittest.IsolatedAsyncioTestCase):
    async def test_skips_the_seed_project_when_the_configured_name_is_blank(self) -> None:
        pool = _FakePool(users=1)

        with _environment(LOOP_SEED_PROJECT_NAME="   "):
            await _bootstrap_initial_setup(pool)

        self.assertEqual(pool.inserts("projects"), [])
        self.assertEqual(
            len(pool.inserts("runner_registration_tokens")),
            1,
            "opting out of the seed project must not disable the other steps",
        )

    async def test_seeds_the_configured_name_and_repository(self) -> None:
        pool = _FakePool(users=1)

        with _environment(
            LOOP_SEED_PROJECT_NAME="acme",
            LOOP_SEED_PROJECT_REPOSITORY_URL="https://github.com/acme/service.git",
        ):
            await _bootstrap_initial_setup(pool)

        inserts = pool.inserts("projects")
        self.assertEqual(len(inserts), 1)
        _project_id, name, repository_url, _configuration, _now = inserts[0]
        self.assertEqual(name, "acme")
        self.assertEqual(repository_url, "https://github.com/acme/service.git")


class SeedIssueTests(unittest.IsolatedAsyncioTestCase):
    async def test_creates_the_seed_issue_once(self) -> None:
        pool = _FakePool(projects=["demo"])

        with _environment(LOOP_SEED_ISSUE_TITLE="first issue"):
            await _seed_issue_if_needed(pool)
            await _seed_issue_if_needed(pool)

        inserts = pool.inserts("issues")
        self.assertEqual(len(inserts), 1)
        self.assertEqual(inserts[0][2], "first issue")

    async def test_skips_the_seed_issue_when_the_project_is_absent(self) -> None:
        pool = _FakePool()

        with _environment(LOOP_SEED_ISSUE_TITLE="first issue"):
            await _seed_issue_if_needed(pool)

        self.assertEqual(pool.executed, [])


if __name__ == "__main__":
    unittest.main()
