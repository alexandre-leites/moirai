from __future__ import annotations

import asyncio
import os
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Self
from unittest.mock import AsyncMock, patch

from moirai.health import HealthState
from moirai.main import (
    CheckpointerUnavailableError,
    _build_checkpointer,
    _log_unexpected_completion,
    register_services,
)


class _ModuleUnavailable:
    def __init__(self, *names: str) -> None:
        self._names = names
        self._previous: dict[str, object] = {}

    def __enter__(self) -> Self:
        for name in self._names:
            self._previous[name] = sys.modules.get(name, "__absent__")
            sys.modules[name] = None  # type: ignore[assignment]
        return self

    def __exit__(self, *exc: object) -> None:
        for name, value in self._previous.items():
            if value == "__absent__":
                sys.modules.pop(name, None)
            else:
                sys.modules[name] = value  # type: ignore[assignment]


class BuildCheckpointerTests(unittest.IsolatedAsyncioTestCase):
    async def asyncSetUp(self) -> None:
        os.environ.pop("LOOP_ALLOW_NO_CHECKPOINTER", None)

    async def asyncTearDown(self) -> None:
        os.environ.pop("LOOP_ALLOW_NO_CHECKPOINTER", None)

    async def test_missing_dependency_is_fatal_by_default(self) -> None:
        with _ModuleUnavailable("psycopg_pool"), self.assertRaises(ModuleNotFoundError):
            await _build_checkpointer("postgresql://localhost/db")

    async def test_missing_dependency_is_swallowed_when_explicitly_allowed(self) -> None:
        os.environ["LOOP_ALLOW_NO_CHECKPOINTER"] = "true"
        with _ModuleUnavailable("psycopg_pool"):
            result = await _build_checkpointer("postgresql://localhost/db")
        self.assertIsNone(result)

    async def test_unreachable_database_is_fatal_by_default(self) -> None:
        with patch("psycopg_pool.AsyncConnectionPool.open", new=AsyncMock(side_effect=RuntimeError("refused"))), \
             patch("psycopg_pool.AsyncConnectionPool.close", new=AsyncMock()), \
             self.assertRaises(CheckpointerUnavailableError):
            await _build_checkpointer("postgresql://localhost/nonexistent")

    async def test_unreachable_database_is_swallowed_when_explicitly_allowed(self) -> None:
        os.environ["LOOP_ALLOW_NO_CHECKPOINTER"] = "true"
        with patch("psycopg_pool.AsyncConnectionPool.open", new=AsyncMock(side_effect=RuntimeError("refused"))), \
             patch("psycopg_pool.AsyncConnectionPool.close", new=AsyncMock()):
            result = await _build_checkpointer("postgresql://localhost/nonexistent")
        self.assertIsNone(result)


class RegisterServicesTests(unittest.IsolatedAsyncioTestCase):
    async def test_returns_the_runner_control_service_for_the_scheduler_to_bind_to(self) -> None:
        import grpc

        from moirai.grpc.runner_control import RunnerControlService

        server = grpc.aio.server()
        result = register_services(server, object())
        self.assertIsInstance(result, RunnerControlService)


class LogUnexpectedCompletionTests(unittest.IsolatedAsyncioTestCase):
    def setUp(self) -> None:
        self._tmpdir = tempfile.TemporaryDirectory()
        self.addCleanup(self._tmpdir.cleanup)

    async def test_a_normal_shutdown_is_not_treated_as_unexpected(self) -> None:
        health = HealthState(_path=Path(self._tmpdir.name) / "health.json")
        shutdown = asyncio.Event()
        shutdown.set()

        async def _noop() -> None:
            return None

        task = asyncio.create_task(_noop())
        await task
        _log_unexpected_completion("scheduler", health, shutdown, task)
        self.assertNotIn("scheduler", health.dead_loops)

    async def test_an_unrequested_completion_marks_the_loop_dead_and_triggers_shutdown(self) -> None:
        health = HealthState(_path=Path(self._tmpdir.name) / "health.json")
        shutdown = asyncio.Event()

        async def _noop() -> None:
            return None

        task = asyncio.create_task(_noop())
        await task
        _log_unexpected_completion("scheduler", health, shutdown, task)
        self.assertIn("scheduler", health.dead_loops)
        self.assertTrue(shutdown.is_set())

    async def test_a_task_that_raised_always_marks_the_loop_dead(self) -> None:
        health = HealthState(_path=Path(self._tmpdir.name) / "health.json")
        shutdown = asyncio.Event()
        shutdown.set()

        async def _boom() -> None:
            raise RuntimeError("boom")

        task = asyncio.create_task(_boom())
        with self.assertRaises(RuntimeError):
            await task
        _log_unexpected_completion("scheduler", health, shutdown, task)
        self.assertIn("scheduler", health.dead_loops)


if __name__ == "__main__":
    unittest.main()
