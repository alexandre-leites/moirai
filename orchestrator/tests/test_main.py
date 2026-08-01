from __future__ import annotations

import asyncio
import base64
import os
import sys
import tempfile
import unittest
from pathlib import Path
from typing import Self
from unittest.mock import AsyncMock, patch

from moirai.config import OrchestratorConfig
from moirai.health import HealthState
from moirai.main import (
    CheckpointerUnavailableError,
    _build_checkpointer,
    _connect_control_plane,
    _log_unexpected_completion,
    _run_workflow_maintenance_loop,
    register_services,
)
from moirai.persistence.secrets import SecretCipher, SecretCipherError


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


class _RecordingControlPlane:
    def __init__(
        self, stalled: tuple[str, ...] = (), waiting_for_checks: tuple[str, ...] = ()
    ) -> None:
        self.stalled = stalled
        self.waiting_for_checks = waiting_for_checks
        self.recovered: list[str] = []
        self.polled: list[str] = []
        self.calls: list[str] = []

    async def drain_pending_transitions(self, on_transition: object, now: object) -> int:
        self.calls.append("drain")
        return 0

    async def close_orphaned_execution_requests(self, now: object, stale_after: object) -> int:
        self.calls.append("sweep")
        return 0

    async def find_workflow_runs_waiting_for_checks(self, limit: int) -> tuple[str, ...]:
        self.calls.append("poll")
        return self.waiting_for_checks

    async def find_stalled_workflow_runs(self, now: object, stale_after: object, limit: int) -> tuple[str, ...]:
        self.calls.append("detect")
        return self.stalled

    async def recover_stalled_workflow_run(
        self, workflow_run_id: str, on_transition: object, now: object
    ) -> bool:
        self.recovered.append(workflow_run_id)
        if workflow_run_id == "explodes":
            raise RuntimeError("recovery failed")
        return True


class _Leader:
    def __init__(self, stop_event: asyncio.Event, error: Exception | None = None) -> None:
        self._stop_event = stop_event
        self._error = error
        self.iterations = 0
        self.closed = False

    async def is_leader(self) -> bool:
        self.iterations += 1
        self._stop_event.set()
        if self._error is not None:
            raise self._error
        return True

    async def close(self) -> None:
        self.closed = True


class WorkflowMaintenanceLoopTests(unittest.IsolatedAsyncioTestCase):
    async def _run_once(
        self,
        control_plane: object,
        leader_error: Exception | None = None,
        on_transition: object | None = None,
    ) -> _Leader:
        from datetime import UTC, datetime, timedelta

        stop_event = asyncio.Event()
        leader = _Leader(stop_event, leader_error)

        async def noop(*_args: object) -> None:
            return None

        await asyncio.wait_for(
            _run_workflow_maintenance_loop(
                control_plane, noop if on_transition is None else on_transition, stop_event,
                lambda: datetime.now(UTC), timedelta(milliseconds=1), leader,
            ),
            timeout=10,
        )
        return leader

    async def test_leadership_probe_failure_does_not_kill_the_loop(self) -> None:
        """AsyncpgLeader re-raises whatever the database did. The loop's
        done-callback now shuts the whole orchestrator down, so an unhandled
        exception here would turn a failover blip into a process exit."""
        control_plane = _RecordingControlPlane()

        leader = await self._run_once(control_plane, leader_error=RuntimeError("connection lost"))

        self.assertEqual(leader.iterations, 1)
        self.assertTrue(leader.closed)
        self.assertEqual(control_plane.calls, [])

    async def test_sweep_runs_before_detection_and_one_failure_does_not_stop_the_batch(self) -> None:
        control_plane = _RecordingControlPlane(stalled=("explodes", "recovers"))

        await self._run_once(control_plane)

        self.assertEqual(control_plane.calls, ["drain", "sweep", "poll", "detect"])
        self.assertEqual(control_plane.recovered, ["explodes", "recovers"])

    async def test_waiting_checks_resume_through_the_transition_callback(self) -> None:
        control_plane = _RecordingControlPlane(waiting_for_checks=("checks-1", "checks-2"))
        transitions: list[tuple[str, str, dict[str, object]]] = []

        async def on_transition(workflow_run_id: str, status: str, updates: dict[str, object]) -> None:
            transitions.append((workflow_run_id, status, updates))

        await self._run_once(control_plane, on_transition=on_transition)

        self.assertEqual(
            transitions,
            [
                ("checks-1", "waiting_github_checks", {"poll_github_checks": True}),
                ("checks-2", "waiting_github_checks", {"poll_github_checks": True}),
            ],
        )


if __name__ == "__main__":
    unittest.main()


class ConnectControlPlaneTests(unittest.IsolatedAsyncioTestCase):
    """How the credential cipher reaches the control plane at startup."""

    KEY = base64.b64encode(b"k" * 32).decode()

    def _config(self, secret_key: str | None) -> OrchestratorConfig:
        return OrchestratorConfig(
            database_url="postgresql://localhost/db",
            grpc_bind="0.0.0.0:50051",
            secret_key=secret_key,
        )

    async def test_no_key_configured_connects_without_a_cipher(self) -> None:
        async def factory(database_url: str, secret_cipher: object = None) -> dict[str, object]:
            return {"url": database_url, "cipher": secret_cipher}

        connected = await _connect_control_plane(factory, self._config(None))
        self.assertIsNone(connected["cipher"])

    async def test_a_configured_key_is_passed_to_the_factory(self) -> None:
        async def factory(database_url: str, secret_cipher: object = None) -> dict[str, object]:
            return {"url": database_url, "cipher": secret_cipher}

        connected = await _connect_control_plane(factory, self._config(self.KEY))
        self.assertIsInstance(connected["cipher"], SecretCipher)

    async def test_an_unusable_key_fails_startup_rather_than_the_first_write(self) -> None:
        calls: list[str] = []

        async def factory(database_url: str, secret_cipher: object = None) -> object:
            calls.append(database_url)
            return object()

        with self.assertRaises(SecretCipherError):
            await _connect_control_plane(factory, self._config("not-a-key"))
        self.assertEqual(calls, [])

    async def test_a_factory_without_the_parameter_is_called_with_the_url_alone(self) -> None:
        async def factory(database_url: str) -> dict[str, object]:
            return {"url": database_url}

        connected = await _connect_control_plane(factory, self._config(self.KEY))
        self.assertEqual(connected, {"url": "postgresql://localhost/db"})

    async def test_a_type_error_inside_the_factory_is_not_retried_without_the_cipher(self) -> None:
        # Retrying would connect a control plane that silently stores nothing
        # encrypted, which is worse than failing to start.
        attempts: list[object] = []

        async def factory(database_url: str, secret_cipher: object = None) -> object:
            attempts.append(secret_cipher)
            raise TypeError("something unrelated broke")

        with self.assertRaises(TypeError):
            await _connect_control_plane(factory, self._config(self.KEY))
        self.assertEqual(len(attempts), 1)
