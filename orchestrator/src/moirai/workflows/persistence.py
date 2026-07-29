from __future__ import annotations

import json
from collections.abc import Callable
from datetime import UTC, datetime
from typing import Any
from uuid import uuid4

from moirai.persistence.circuits import reopen_probe_circuits

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

# The newest request that has neither been reported on nor closed. Newest
# rather than oldest: a run that legitimately re-queued a phase (the
# maintenance loop's lost-execution repair) has older closed rows for the same
# role, and the graph is suspended on the most recent one.
_OPEN_EXECUTION_REQUEST_QUERY = """
    SELECT id, role, attempt
    FROM app.workflow_execution_requests
    WHERE workflow_run_id = $1 AND status IN ('queued', 'dispatched')
    ORDER BY created_at DESC, id DESC
    LIMIT 1
"""


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
                    # merged_at and merge_commit are COALESCEd against the row
                    # rather than overwritten: they are the durable proof that
                    # the merge happened, and a later transition that touches
                    # the pull request without re-reading the merge (a
                    # re-entered merge node, a recovery path) carries neither.
                    # Assigning EXCLUDED unconditionally would erase the merge
                    # record on the next write. state is overwritten because it
                    # is the current fact, not a milestone.
                    await connection.execute(
                        """
                        INSERT INTO app.pull_requests
                            (id, workflow_run_id, provider, external_id, url, head_commit, state,
                             merged_at, merge_commit, raw_snapshot)
                        VALUES (gen_random_uuid(), $1, 'github', $2, $3, $4, $5, $6, $7, '{}'::jsonb)
                        ON CONFLICT (workflow_run_id) DO UPDATE
                        SET external_id = EXCLUDED.external_id,
                            url = EXCLUDED.url,
                            head_commit = EXCLUDED.head_commit,
                            state = EXCLUDED.state,
                            merged_at = COALESCE(EXCLUDED.merged_at, app.pull_requests.merged_at),
                            merge_commit = COALESCE(
                                EXCLUDED.merge_commit, app.pull_requests.merge_commit
                            )
                        """,
                        _uuid(workflow_run_id),
                        str(updates.get("pull_request_id")),
                        str(updates.get("pull_request_url") or ""),
                        str(updates.get("pull_request_head_commit") or ""),
                        str(updates.get("pull_request_state") or "open"),
                        _merged_at(updates, now),
                        _optional_value(updates.get("pull_request_merge_commit")),
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
                WHERE probe_workflow_run_id = $1 AND state = 'half_open'
                """,
                _uuid(workflow_run_id),
                now,
            )
            return
        if status != "blocked":
            # Issue #92: a cancelled or failed run delivers no verdict about the
            # project, but if it was holding a probe the circuit stays half-open
            # -- and unschedulable -- until something releases it. Nothing else
            # in this path does. Only the probe is released: neither status
            # counts as a project failure, so nothing else about the circuit
            # changes.
            await reopen_probe_circuits(connection, _uuid(workflow_run_id), now)
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
            WHERE probe_workflow_run_id = $1 AND state = 'half_open'
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
                    if str(record["status"]) != "completed":
                        # The same compensation as the lock release above, for
                        # the circuit (issue #92). A terminal status can also be
                        # written straight to app.workflow_runs by the control
                        # plane's runner-event path, and the runtime returns
                        # early for a run that is already terminal, so
                        # transition() -- and with it every circuit write -- is
                        # never reached for that run. `completed` is excluded
                        # because a delivered probe must *close* its circuit,
                        # which only transition() knows how to do; a probe left
                        # pointing at a completed run is the reaper's job.
                        await reopen_probe_circuits(
                            connection, _uuid(workflow_run_id), self._now()
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

    async def get_open_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None:
        """The execution request this run is still waiting on, if any.

        "Open" is `queued` or `dispatched`: the scheduler moves a request from
        one to the other the moment it offers the work, and both mean the same
        thing to the graph -- an execution was requested and has not reported
        back yet. Every other status is closed: `accept_event` stamps the
        runner's terminal event on the row in the same transaction that records
        the outcome, and the maintenance loop closes the rest as `orphaned` or
        `expired`. A closed row must never be mistaken for live work, or the
        phase it belonged to would never run again.
        """
        record = await self._pool.fetchrow(_OPEN_EXECUTION_REQUEST_QUERY, _uuid(workflow_run_id))
        return None if record is None else _execution_request(record, created=False)

    async def dispatch(self, workflow_run_id: str, role: str) -> dict[str, Any]:
        """Ensures the run has an open execution request, and reports whether
        this call is the one that created it.

        A workflow run has at most one execution in flight, so an already-open
        request is returned as-is instead of queueing a second one. That makes
        the whole dispatch idempotent under the outbox's at-least-once
        delivery, and it is decided inside the transaction that holds the run's
        row lock, so two concurrent deliveries cannot both insert. `created` is
        what tells the caller whether an attempt was actually spent: reusing a
        request must never charge the retry budget a second time.
        """
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
                open_request = await connection.fetchrow(
                    _OPEN_EXECUTION_REQUEST_QUERY, _uuid(workflow_run_id)
                )
                if open_request is not None:
                    return _execution_request(open_request, created=False)
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
        return {"id": str(execution_id), "role": role, "attempt": int(attempt), "created": True}


def _optional_value(value: object) -> str | None:
    """SQL NULL for anything the code host did not report.

    The graph state keeps these as empty strings so a checkpoint stays
    JSON-plain; the column's one encoding for "absent" is NULL.
    """
    text = "" if value is None else str(value).strip()
    return text or None


def _merged_at(updates: dict[str, object], now: datetime) -> datetime | None:
    """When the pull request merged, as a timestamp the column can hold.

    Non-NULL only for an update that reports a merge, so no timestamp can ever
    be written for a pull request nobody said had merged -- the reported
    timestamp is read *after* that test, never as a substitute for it.

    The code host reports an ISO-8601 string, so it is parsed here rather than
    handed to asyncpg as text. A merge the code host confirmed but timestamped
    unusably or not at all still gets a timestamp -- `now` -- because the
    alternative is a merged pull request whose `merged_at` stays NULL, which is
    precisely the state this column exists to rule out. A pull request that is
    not merged gets NULL, and the COALESCE in the upsert keeps that from
    erasing an earlier merge.
    """
    if (
        updates.get("pull_request_merged") is not True
        and str(updates.get("pull_request_state") or "") != "merged"
    ):
        return None
    reported = _optional_value(updates.get("pull_request_merged_at"))
    if reported is None:
        return now
    try:
        parsed = datetime.fromisoformat(reported)
    except ValueError:
        return now
    return parsed if parsed.tzinfo is not None else parsed.replace(tzinfo=UTC)


def _execution_request(record: Any, created: bool) -> dict[str, Any]:
    return {
        "id": str(record["id"]),
        "role": str(record["role"]),
        "attempt": int(record["attempt"]),
        "created": created,
    }


def _optional_text(value: object) -> str | None:
    return None if value is None else str(value)


def _uuid(value: str) -> Any:
    from uuid import UUID

    try:
        return UUID(value)
    except ValueError as error:
        raise ValueError("workflow run ID is invalid") from error
