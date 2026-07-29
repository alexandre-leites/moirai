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
    "last_diff_hash": "last_diff_hash",
    "last_failure_fingerprint": "last_failure_fingerprint",
    "non_progress_attempts": "non_progress_attempts",
}

_TERMINAL_STATUSES = frozenset({"completed", "blocked", "failed", "cancelled"})

# Outcome-identity columns have exactly one encoding for "absent": SQL NULL.
# An empty string from any caller is normalised here so this writer and the
# control plane's own writer can never disagree about what "no diff" means.
_NULL_WHEN_EMPTY_COLUMNS = frozenset({"last_diff_hash", "last_failure_fingerprint"})


class AsyncpgWorkflowPersistence:
    def __init__(self, pool: Any, now: Callable[[], datetime] | None = None) -> None:
        self._pool = pool
        self._now = now or (lambda: datetime.now(UTC))

    async def transition(
        self, workflow_run_id: str, status: str, updates: dict[str, object]
    ) -> None:
        now = self._now()
        set_clauses = ["status = $2", "current_phase = $2", "updated_at = $3"]
        params: list[Any] = [_uuid(workflow_run_id), status, now]
        if updates.get("progressed") is True:
            set_clauses.append("last_progress_at = $3")
        for key, column in _DURABLE_COLUMNS.items():
            if key in updates:
                value = updates[key]
                if key in _NULL_WHEN_EMPTY_COLUMNS and value == "":
                    value = None
                params.append(value)
                set_clauses.append(f"{column} = ${len(params)}")
        if status in _TERMINAL_STATUSES:
            params.append(now)
            set_clauses.append(f"completed_at = COALESCE(completed_at, ${len(params)})")
        query = f"UPDATE app.workflow_runs SET {', '.join(set_clauses)} WHERE id = $1 RETURNING id, project_id"
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                changed = await connection.fetchrow(query, *params)
                if changed is None:
                    raise ValueError("workflow run is unknown")
                if status in _TERMINAL_STATUSES:
                    await connection.execute(
                        "DELETE FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
                        changed["project_id"],
                        _uuid(workflow_run_id),
                    )
                    await self._update_project_circuit(
                        connection, changed["project_id"], workflow_run_id, status, updates, now
                    )
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


    async def _update_project_circuit(
        self,
        connection: Any,
        project_id: Any,
        workflow_run_id: str,
        status: str,
        updates: dict[str, object],
        now: datetime,
    ) -> None:
        if status == "completed":
            await connection.execute(
                """
                INSERT INTO app.project_circuit_state
                    (project_id, state, consecutive_failures, last_failure_reason, opened_at, probe_workflow_run_id, updated_at)
                VALUES ($1, 'closed', 0, NULL, NULL, NULL, $2)
                ON CONFLICT (project_id) DO UPDATE
                SET state = 'closed', consecutive_failures = 0, last_failure_reason = NULL,
                    opened_at = NULL, probe_workflow_run_id = NULL, updated_at = EXCLUDED.updated_at
                """,
                project_id,
                now,
            )
            await connection.execute(
                """
                UPDATE app.provider_circuit_state
                SET state = 'closed', consecutive_failures = 0, last_failure_reason = NULL,
                    opened_at = NULL, probe_workflow_run_id = NULL, updated_at = $2
                WHERE probe_workflow_run_id = $1
                """,
                _uuid(workflow_run_id),
                now,
            )
            return
        if status != "blocked":
            return
        reason = str(updates.get("blocking_reason") or "workflow blocked")[:1024]
        await connection.execute(
            """
            INSERT INTO app.project_circuit_state
                (project_id, state, consecutive_failures, last_failure_reason, opened_at, probe_workflow_run_id, updated_at)
            VALUES ($1, 'closed', 1, $2, NULL, NULL, $3)
            ON CONFLICT (project_id) DO UPDATE
            SET consecutive_failures = CASE
                    WHEN app.project_circuit_state.last_failure_reason = EXCLUDED.last_failure_reason
                    THEN app.project_circuit_state.consecutive_failures + 1 ELSE 1 END,
                state = CASE
                    WHEN app.project_circuit_state.state = 'half_open' THEN 'open'
                    WHEN app.project_circuit_state.last_failure_reason = EXCLUDED.last_failure_reason
                     AND app.project_circuit_state.consecutive_failures >= 2 THEN 'open' ELSE 'closed' END,
                opened_at = CASE
                    WHEN app.project_circuit_state.state = 'half_open' THEN EXCLUDED.updated_at
                    WHEN app.project_circuit_state.last_failure_reason = EXCLUDED.last_failure_reason
                     AND app.project_circuit_state.consecutive_failures >= 2 THEN EXCLUDED.updated_at ELSE NULL END,
                probe_workflow_run_id = NULL,
                last_failure_reason = EXCLUDED.last_failure_reason,
                updated_at = EXCLUDED.updated_at
            """,
            project_id,
            reason,
            now,
        )
        await connection.execute(
            """
            UPDATE app.provider_circuit_state
            SET state = 'open', opened_at = $2, probe_workflow_run_id = NULL,
                consecutive_failures = consecutive_failures + 1,
                last_failure_reason = $3, updated_at = $2
            WHERE probe_workflow_run_id = $1
            """,
            _uuid(workflow_run_id),
            now,
            reason,
        )

    async def load_state(self, workflow_run_id: str) -> dict[str, object]:
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                record = await connection.fetchrow(
                    """
                    SELECT wr.id, wr.project_id, wr.status, wr.branch_name, wr.planning_attempts,
                           wr.implementation_attempts, wr.pipeline_repair_attempts, wr.review_cycles,
                           wr.ci_repair_attempts, wr.total_agent_executions, wr.blocking_reason,
                           wr.pull_request_external_id, wr.pull_request_url, i.external_id,
                           i.human_approval_required, p.default_branch, p.configuration,
                           j.id AS job_id
                    FROM app.workflow_runs AS wr
                    JOIN app.issues AS i ON i.id = wr.issue_id
                    JOIN app.projects AS p ON p.id = wr.project_id
                    LEFT JOIN app.jobs AS j ON j.workflow_run_id = wr.id
                    WHERE wr.id = $1
                    FOR UPDATE OF wr
                    """,
                    _uuid(workflow_run_id),
                )
                if record is None:
                    raise ValueError("workflow run is unknown")
                if str(record["status"]) in _TERMINAL_STATUSES:
                    await connection.execute(
                        "DELETE FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
                        record["project_id"],
                        _uuid(workflow_run_id),
                    )
                branch_name = _optional_text(record["branch_name"])
                if branch_name is None:
                    job_id = record["job_id"]
                    if job_id is None:
                        raise ValueError("workflow run has no job")
                    branch_name = f"agent/{record['external_id']}/{str(job_id)[:8]}"
                    await connection.execute(
                        "UPDATE app.workflow_runs SET branch_name = $2, updated_at = $3 WHERE id = $1",
                        _uuid(workflow_run_id),
                        branch_name,
                        self._now(),
                    )
        configuration = record["configuration"]
        if isinstance(configuration, str):
            configuration = json.loads(configuration)
        if not isinstance(configuration, dict):
            configuration = {}
        merge_method = configuration.get("merge_method", "squash")
        if merge_method not in {"merge", "rebase", "squash"}:
            merge_method = "squash"
        return {
            "workflow_run_id": workflow_run_id,
            "project_id": str(record["project_id"]),
            "issue_id": str(record["external_id"]),
            "status": str(record["status"]),
            "branch_name": branch_name,
            "base_branch": str(record["default_branch"]),
            "human_approval_required": bool(record["human_approval_required"]),
            "merge_method": merge_method,
            "planning_attempts": int(record["planning_attempts"]),
            "implementation_attempts": int(record["implementation_attempts"]),
            "pipeline_repair_attempts": int(record["pipeline_repair_attempts"]),
            "review_cycles": int(record["review_cycles"]),
            "ci_repair_attempts": int(record["ci_repair_attempts"]),
            "total_agent_executions": int(record["total_agent_executions"]),
            "blocking_reason": _optional_text(record["blocking_reason"]),
            "pull_request_id": _optional_text(record["pull_request_external_id"]),
            "pull_request_url": _optional_text(record["pull_request_url"]),
        }

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


def _optional_text(value: object) -> str | None:
    return None if value is None else str(value)


def _uuid(value: str) -> Any:
    from uuid import UUID

    try:
        return UUID(value)
    except ValueError as error:
        raise ValueError("workflow run ID is invalid") from error
