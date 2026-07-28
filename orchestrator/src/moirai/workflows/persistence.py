from __future__ import annotations

import json
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4


_VALID_ROLES = frozenset({"planner", "developer", "pipeline", "reviewer", "repairer"})

# The durable subset of graph state that has a matching app.workflow_runs
# column. Anything else in `updates` (gate booleans like plan_valid, which
# have no queryable column) stays in the workflow_events audit trail and the
# LangGraph checkpoint only.
_DURABLE_COLUMNS: dict[str, str] = {
    "planning_attempts": "planning_attempts",
    "implementation_attempts": "implementation_attempts",
    "pipeline_repair_attempts": "pipeline_repair_attempts",
    "review_cycles": "review_cycles",
    "ci_repair_attempts": "ci_repair_attempts",
    "total_agent_executions": "total_agent_executions",
    "blocking_reason": "blocking_reason",
    "branch_name": "branch_name",
    "base_commit": "base_commit",
    "current_commit": "current_commit",
    "pull_request_id": "pull_request_external_id",
    "pull_request_url": "pull_request_url",
}

_TERMINAL_STATUSES = frozenset({"completed", "blocked", "failed", "cancelled"})


class AsyncpgWorkflowPersistence:
    def __init__(self, pool: Any, now: Callable[[], datetime] | None = None) -> None:
        self._pool = pool
        self._now = now or (lambda: datetime.now(UTC))

    async def transition(
        self, workflow_run_id: str, status: str, updates: dict[str, object]
    ) -> None:
        now = self._now()
        set_clauses = ["status = $2", "current_phase = $2", "updated_at = $3", "last_progress_at = $3"]
        params: list[Any] = [_uuid(workflow_run_id), status, now]
        for key, column in _DURABLE_COLUMNS.items():
            if key in updates:
                params.append(updates[key])
                set_clauses.append(f"{column} = ${len(params)}")
        if status in _TERMINAL_STATUSES:
            params.append(now)
            set_clauses.append(f"completed_at = COALESCE(completed_at, ${len(params)})")
        query = f"UPDATE app.workflow_runs SET {', '.join(set_clauses)} WHERE id = $1 RETURNING id"
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                changed = await connection.fetchrow(query, *params)
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
                if "pull_request_id" in updates:
                    await connection.execute(
                        """
                        INSERT INTO app.pull_requests
                            (id, workflow_run_id, provider, external_id, url, head_commit, state, raw_snapshot)
                        VALUES (gen_random_uuid(), $1, 'github', $2, $3, $4, $5, '{}'::jsonb)
                        ON CONFLICT (workflow_run_id) DO UPDATE
                        SET external_id = EXCLUDED.external_id,
                            url = EXCLUDED.url,
                            head_commit = EXCLUDED.head_commit,
                            state = EXCLUDED.state
                        """,
                        _uuid(workflow_run_id),
                        str(updates.get("pull_request_id")),
                        str(updates.get("pull_request_url") or ""),
                        str(updates.get("pull_request_head_commit") or ""),
                        str(updates.get("pull_request_state") or "open"),
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
