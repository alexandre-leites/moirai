from __future__ import annotations

import json
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4


_VALID_ROLES = frozenset({"planner", "developer", "pipeline", "reviewer", "repairer"})


class AsyncpgWorkflowPersistence:
    def __init__(self, pool: Any, now: Callable[[], datetime] | None = None) -> None:
        self._pool = pool
        self._now = now or (lambda: datetime.now(UTC))

    async def transition(
        self, workflow_run_id: str, status: str, updates: dict[str, object]
    ) -> None:
        now = self._now()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                changed = await connection.fetchrow(
                    """
                    UPDATE app.workflow_runs
                    SET status = $2, current_phase = $2, updated_at = $3
                    WHERE id = $1
                    RETURNING id
                    """,
                    _uuid(workflow_run_id),
                    status,
                    now,
                )
                if changed is None:
                    raise ValueError("workflow run is unknown")
                await connection.execute(
                    """
                    INSERT INTO app.workflow_events (workflow_run_id, event_type, severity, payload, created_at)
                    VALUES ($1, 'workflow_transition', 'info', $2::jsonb, $3)
                    """,
                    _uuid(workflow_run_id),
                    json.dumps({"status": status, "updates": updates}, separators=(",", ":"), sort_keys=True),
                    now,
                )


    async def latest_checkpoint(self, workflow_run_id: str) -> tuple[int, dict[str, object]] | None:
        record = await self._pool.fetchrow(
            """
            SELECT version, state
            FROM app.workflow_checkpoints
            WHERE workflow_run_id = $1
            ORDER BY version DESC
            LIMIT 1
            """,
            _uuid(workflow_run_id),
        )
        if record is None:
            return None
        state = record["state"]
        if isinstance(state, str):
            try:
                state = json.loads(state)
            except json.JSONDecodeError as error:
                raise ValueError("workflow checkpoint state is invalid") from error
        if not isinstance(state, dict) or any(not isinstance(key, str) for key in state):
            raise ValueError("workflow checkpoint state is invalid")
        return int(record["version"]), state

    async def checkpoint(self, workflow_run_id: str, state: dict[str, object]) -> int:
        now = self._now()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                workflow = await connection.fetchrow(
                    "SELECT id FROM app.workflow_runs WHERE id = $1 FOR UPDATE",
                    _uuid(workflow_run_id),
                )
                if workflow is None:
                    raise ValueError("workflow run is unknown")
                version = await connection.fetchval(
                    "SELECT COALESCE(MAX(version), 0) + 1 FROM app.workflow_checkpoints WHERE workflow_run_id = $1",
                    _uuid(workflow_run_id),
                )
                await connection.execute(
                    """
                    INSERT INTO app.workflow_checkpoints (workflow_run_id, version, state, created_at)
                    VALUES ($1, $2, $3::jsonb, $4)
                    """,
                    _uuid(workflow_run_id),
                    int(version),
                    json.dumps(state, separators=(",", ":"), sort_keys=True),
                    now,
                )
        return int(version)

    async def get_queued_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None:
        record = await self._pool.fetchrow(
            """
            SELECT id, role, attempt
            FROM app.workflow_execution_requests
            WHERE workflow_run_id = $1 AND status = 'queued'
            ORDER BY created_at ASC
            LIMIT 1
            """,
            _uuid(workflow_run_id),
        )
        if record is None:
            return None
        return {
            "id": str(record["id"]),
            "role": str(record["role"]),
            "attempt": int(record["attempt"]),
        }

    async def dispatch(self, workflow_run_id: str, role: str) -> str:
        if role not in _VALID_ROLES:
            raise ValueError("workflow execution role is invalid")
        now = self._now()
        execution_id = uuid4()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                workflow = await connection.fetchrow(
                    "SELECT id FROM app.workflow_runs WHERE id = $1 FOR UPDATE",
                    _uuid(workflow_run_id),
                )
                if workflow is None:
                    raise ValueError("workflow run is unknown")
                attempt = await connection.fetchval(
                    """
                    SELECT COALESCE(MAX(attempt), 0) + 1
                    FROM app.workflow_execution_requests
                    WHERE workflow_run_id = $1 AND role = $2
                    """,
                    _uuid(workflow_run_id),
                    role,
                )
                await connection.execute(
                    """
                    INSERT INTO app.workflow_execution_requests
                        (id, workflow_run_id, role, attempt, status, created_at)
                    VALUES ($1, $2, $3, $4, 'queued', $5)
                    """,
                    execution_id,
                    _uuid(workflow_run_id),
                    role,
                    int(attempt),
                    now,
                )
        return str(execution_id)


def _uuid(value: str) -> Any:
    from uuid import UUID

    try:
        return UUID(value)
    except ValueError as error:
        raise ValueError("workflow run ID is invalid") from error
