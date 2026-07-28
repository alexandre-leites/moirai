from __future__ import annotations

import os
import unittest
from datetime import UTC, datetime, timedelta
from hashlib import sha256
from unittest.mock import patch

from moirai.main import _bootstrap_initial_setup


class _FakePool:
    def __init__(self) -> None:
        self.user_count = 0
        self.projects: dict[str, str] = {}
        self.executed: list[tuple[str, tuple[object, ...]]] = []

    async def fetchval(self, query: str, *args: object) -> object:
        if "COUNT(*) FROM app.users" in query:
            return self.user_count
        if "COUNT(*) FROM app.projects WHERE name" in query:
            return 1 if args[0] in self.projects else 0
        if "id FROM app.projects WHERE name" in query:
            return self.projects.get(str(args[0]))
        if "COUNT(*) FROM app.runner_registration_tokens" in query:
            return 0
        raise AssertionError(query)

    async def execute(self, query: str, *args: object) -> str:
        self.executed.append((query, args))
        if "INSERT INTO app.projects" in query:
            self.projects[str(args[1])] = str(args[0])
        return "INSERT 0 1"


class BootstrapRegistrationTokenTests(unittest.IsolatedAsyncioTestCase):
    async def test_skips_seeding_when_registration_token_env_var_is_unset(self) -> None:
        pool = _FakePool()
        with patch.dict(
            os.environ,
            {"LOOP_INITIAL_ADMIN_PASSWORD": "correct horse battery staple"},
            clear=True,
        ):
            os.environ.pop("RUNNER_REGISTRATION_TOKEN", None)
            await _bootstrap_initial_setup(pool)
        self.assertFalse(
            any("runner_registration_tokens" in query for query, _ in pool.executed),
            "must not seed a registration token when RUNNER_REGISTRATION_TOKEN is unset",
        )

    async def test_seeds_token_hash_for_the_same_variable_compose_gives_the_runner(self) -> None:
        pool = _FakePool()
        with patch.dict(
            os.environ,
            {
                "LOOP_INITIAL_ADMIN_PASSWORD": "correct horse battery staple",
                "RUNNER_REGISTRATION_TOKEN": "a-real-secret-value",
            },
            clear=True,
        ):
            before = datetime.now(UTC)
            await _bootstrap_initial_setup(pool)
            after = datetime.now(UTC)

        inserts = [
            (query, args) for query, args in pool.executed if "INSERT INTO app.runner_registration_tokens" in query
        ]
        self.assertEqual(len(inserts), 1)
        _, args = inserts[0]
        _token_id, token_hash, _labels_json, created_at, expires_at = args
        self.assertEqual(token_hash, sha256(b"a-real-secret-value").hexdigest())

        # TTL must be a short, fixed duration via timedelta arithmetic — not
        # now.replace(year=now.year + 10), which both mismatches the API-issued token
        # TTL and raises ValueError if evaluated on a leap day.
        ttl = expires_at - created_at
        self.assertEqual(ttl, timedelta(minutes=15))
        self.assertTrue(before <= created_at <= after)


if __name__ == "__main__":
    unittest.main()
