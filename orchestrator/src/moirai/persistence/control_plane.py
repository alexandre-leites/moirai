from __future__ import annotations

import json
from collections.abc import Awaitable, Callable, Iterable
from datetime import datetime, timedelta
from hashlib import sha256
from pathlib import PurePath
from secrets import compare_digest, token_urlsafe
from typing import TYPE_CHECKING, Any, cast
from uuid import UUID, uuid4

from moirai.domain.control_plane import (
    AuthenticationError,
    JobOffer,
    OfferError,
    RegistrationError,
    ScheduledJob,
)
from moirai.domain.leases import StaleLeaseError
from moirai.domain.models import (
    ExecutionEvent,
    Issue,
    JobLease,
    Project,
    Runner,
    Workflow,
    WorkflowStatus,
)
from moirai.domain.scheduling import Assignment
from moirai.persistence.authentication import (
    AsyncpgAuthentication,
    AuthenticatedSession,
    SessionCredentials,
)

if TYPE_CHECKING:
    # Deferred: grpc/protocol.py imports this module's classes back for a
    # Protocol-conformance check, so this side must not import it at runtime.
    from moirai.grpc.protocol import (
        ProjectRecord,
        RegistrationTokenRecord,
        RunnerRecord,
        WorkflowRecord,
    )
from moirai.workflows.runner_events import (
    WorkflowTransition,
    role_to_suffix,
    validate_runner_event,
    workflow_transition_for_terminal_event,
)
from moirai.workflows.task_packets import (
    ExecutionRole,
    PipelineCommand,
    build_task_packet,
    pipeline_task_execution,
    planner_task_execution,
    task_execution,
)


class AsyncpgControlPlane:
    def __init__(self, pool: Any, circuit_probe_cooldown: timedelta = timedelta(minutes=5)) -> None:
        if circuit_probe_cooldown <= timedelta():
            raise ValueError("circuit probe cooldown must be positive")
        self._pool = pool
        self._authentication = AsyncpgAuthentication(pool)
        self._circuit_probe_cooldown = circuit_probe_cooldown

    @property
    def pool(self) -> Any:
        """The underlying connection pool, for callers that need durability-specific setup (migrations, workflow runtime, scheduling)."""
        return self._pool

    @classmethod
    async def connect(cls, database_url: str) -> AsyncpgControlPlane:
        try:
            import asyncpg
        except ModuleNotFoundError as error:
            raise RuntimeError("asyncpg is required to run the orchestrator") from error
        pg_dsn = database_url.replace("+asyncpg", "")
        return cls(await asyncpg.create_pool(dsn=pg_dsn, min_size=1, max_size=10))

    async def close(self) -> None:
        await self._pool.close()

    async def metrics_snapshot(self, now: datetime) -> dict[str, float]:
        record = await self._pool.fetchrow(
            """
            SELECT
                (SELECT COUNT(*)
                 FROM app.issues AS issue
                 JOIN app.projects AS project ON project.id = issue.project_id
                 WHERE project.enabled = true AND issue.eligible = true AND issue.state = 'open') AS queue_depth,
                (SELECT COUNT(*)
                 FROM app.workflow_runs
                 WHERE status NOT IN ('completed', 'blocked', 'failed', 'cancelled')) AS active_workflows,
                (SELECT EXTRACT(EPOCH FROM ($1::timestamptz - MIN(last_seen_at)))
                 FROM app.runners
                 WHERE enabled = true AND revoked_at IS NULL AND last_seen_at IS NOT NULL) AS runner_heartbeat_age
            """,
            now,
        )
        if record is None:
            return {"queue_depth": 0, "active_workflows": 0, "runner_heartbeat_age": 0}
        return {
            "queue_depth": float(record["queue_depth"] or 0),
            "active_workflows": float(record["active_workflows"] or 0),
            "runner_heartbeat_age": float(record["runner_heartbeat_age"] or 0),
        }

    async def login(self, username: str, password: str, now: datetime) -> SessionCredentials:
        return await self._authentication.login(username, password, now)

    async def validate_session(
        self, session_token: str, csrf_token: str | None, now: datetime, require_csrf: bool
    ) -> AuthenticatedSession:
        return await self._authentication.validate_session(
            session_token, csrf_token, now, require_csrf
        )

    async def revoke_session(self, session_token: str, now: datetime) -> None:
        await self._authentication.revoke_session(session_token, now)

    async def reap_expired_data(self, now: datetime) -> dict[str, int]:
        return {
            "sessions": await self._authentication.reap_expired_sessions(now),
            "audit_events": await self._authentication.reap_audit_events(now),
            "workflow_events": await self._authentication.reap_workflow_events(now),
        }

    async def append_audit(
        self,
        actor_user_id: str | None,
        action: str,
        resource_type: str,
        resource_id: str,
        outcome: str,
        now: datetime,
    ) -> None:
        await self._authentication.append_audit(
            actor_user_id, action, resource_type, resource_id, outcome, now
        )

    async def create_project(
        self,
        name: str,
        repository_mode: str,
        repository_url: str | None,
        local_repository_path: str | None,
        default_branch: str,
        required_runner_labels: Iterable[str],
        now: datetime,
        actor_user_id: str | None = None,
    ) -> ProjectRecord:
        normalized = _project_configuration(
            name,
            repository_mode,
            repository_url,
            local_repository_path,
            default_branch,
            required_runner_labels,
        )
        project_id = uuid4()
        record = await self._pool.fetchrow(
            """
            INSERT INTO app.projects
                (id, name, enabled, repository_mode, repository_url, local_repository_path,
                 default_branch, configuration, created_at, updated_at)
            VALUES ($1, $2, true, $3, $4, $5, $6, $7::jsonb, $8, $8)
            RETURNING id, name, enabled
            """,
            project_id,
            normalized["name"],
            normalized["repository_mode"],
            normalized["repository_url"],
            normalized["local_repository_path"],
            normalized["default_branch"],
            json.dumps({"required_runner_labels": normalized["required_runner_labels"]}),
            now,
        )
        if record is None:
            raise ValueError("project could not be created")
        project: ProjectRecord = {"id": str(record["id"]), "name": str(record["name"]), "enabled": bool(record["enabled"])}
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.create", "project", project["id"], "succeeded", now)
        return project

    async def list_projects(self) -> list[ProjectRecord]:
        records = await self._pool.fetch(
            "SELECT id, name, enabled FROM app.projects ORDER BY name ASC, id ASC"
        )
        return [
            {"id": str(record["id"]), "name": str(record["name"]), "enabled": bool(record["enabled"])}
            for record in records
        ]

    async def list_enabled_projects(self) -> list[Project]:
        records = await self._pool.fetch(
            """
            SELECT id, configuration, repository_url
            FROM app.projects
            WHERE enabled = true
            ORDER BY id ASC
            """
        )
        result = []
        for record in records:
            config = record["configuration"]
            if isinstance(config, str):
                config = json.loads(config)
            labels = frozenset(config.get("required_runner_labels", []))
            result.append(Project(str(record["id"]), True, labels, _optional_text(record["repository_url"])))
        return result

    async def upsert_issue(
        self,
        *,
        project_id: str,
        external_id: str,
        title: str,
        body: str,
        state: str,
        labels: list[str],
        priority: int,
        eligible: bool,
        human_approval_required: bool,
        external_created_at: datetime,
        external_updated_at: datetime,
        now: datetime,
    ) -> None:
        labels_json = json.dumps(sorted(labels), separators=(",", ":"))
        await self._pool.execute(
            """
            INSERT INTO app.issues
                (id, project_id, provider, external_id, display_number, title, body, url,
                 state, labels, priority, eligible, human_approval_required,
                 external_created_at, external_updated_at, last_synced_at, raw_snapshot)
            VALUES (gen_random_uuid(), $1, 'github', $2, $2, $3, $4, '', $5, $6::jsonb,
                    $7, $8, $9, $10, $11, $12, '{}'::jsonb)
            ON CONFLICT (project_id, provider, external_id) DO UPDATE
            SET title = EXCLUDED.title,
                body = EXCLUDED.body,
                state = EXCLUDED.state,
                labels = EXCLUDED.labels,
                priority = EXCLUDED.priority,
                eligible = EXCLUDED.eligible,
                human_approval_required = EXCLUDED.human_approval_required,
                external_updated_at = EXCLUDED.external_updated_at,
                last_synced_at = EXCLUDED.last_synced_at
            """,
            _uuid(project_id),
            external_id,
            title,
            body,
            state,
            labels_json,
            priority,
            eligible,
            human_approval_required,
            external_created_at,
            external_updated_at,
            now,
        )

    async def list_active_workflows_for_project(self, project_id: str) -> list[dict[str, object]]:
        records = await self._pool.fetch(
            """
            SELECT wr.id, wr.status, i.external_id, i.id AS issue_id, i.labels
            FROM app.workflow_runs wr
            JOIN app.issues i ON i.id = wr.issue_id
            WHERE wr.project_id = $1
            ORDER BY wr.id ASC
            """,
            _uuid(project_id),
        )
        return [
            {
                "id": str(record["id"]),
                "status": str(record["status"]),
                "external_id": str(record["external_id"]),
                "issue_id": str(record["issue_id"]),
            }
            for record in records
        ]

    async def get_issue_labels(self, issue_id: str) -> list[str]:
        record = await self._pool.fetchrow(
            "SELECT labels FROM app.issues WHERE id = $1",
            _uuid(issue_id),
        )
        if record is None:
            return []
        labels = record["labels"]
        if isinstance(labels, str):
            labels = json.loads(labels)
        if not isinstance(labels, list):
            return []
        return [str(label) for label in labels]

    async def set_issue_labels(self, issue_id: str, labels: list[str]) -> None:
        await self._pool.execute(
            "UPDATE app.issues SET labels = $2::jsonb WHERE id = $1",
            _uuid(issue_id),
            json.dumps(sorted(set(labels)), separators=(",", ":")),
        )

    async def mark_missing_issues_ineligible(
        self, project_id: str, external_ids: list[str], now: datetime
    ) -> None:
        await self._pool.execute(
            """
            UPDATE app.issues
            SET eligible = false, last_synced_at = $2
            WHERE project_id = $1 AND state = 'open'
              AND NOT (external_id = ANY($3::text[]))
            """,
            _uuid(project_id),
            now,
            external_ids,
        )

    async def issue_sync_retry_state(self, now: datetime) -> list[dict[str, object]]:
        records = await self._pool.fetch(
            """
            SELECT project_id, consecutive_failures, next_retry_at
            FROM app.issue_sync_state
            WHERE consecutive_failures > 0 AND next_retry_at > $1
            """,
            now,
        )
        return [
            {
                "project_id": str(record["project_id"]),
                "consecutive_failures": int(record["consecutive_failures"]),
                "next_retry_at": record["next_retry_at"],
            }
            for record in records
        ]

    async def record_issue_sync_failure(
        self, project_id: str, failures: int, retry_at: datetime, error: str, now: datetime
    ) -> None:
        await self._pool.execute(
            """
            INSERT INTO app.issue_sync_state
                (project_id, consecutive_failures, next_retry_at, last_error, updated_at)
            VALUES ($1, $2, $3, $4, $5)
            ON CONFLICT (project_id) DO UPDATE
            SET consecutive_failures = EXCLUDED.consecutive_failures,
                next_retry_at = EXCLUDED.next_retry_at,
                last_error = EXCLUDED.last_error,
                updated_at = EXCLUDED.updated_at
            """,
            _uuid(project_id),
            failures,
            retry_at,
            error[:1024],
            now,
        )

    async def clear_issue_sync_failure(self, project_id: str, now: datetime) -> None:
        await self._pool.execute(
            """
            INSERT INTO app.issue_sync_state
                (project_id, consecutive_failures, next_retry_at, last_error, updated_at)
            VALUES ($1, 0, NULL, NULL, $2)
            ON CONFLICT (project_id) DO UPDATE
            SET consecutive_failures = 0, next_retry_at = NULL, last_error = NULL, updated_at = EXCLUDED.updated_at
            """,
            _uuid(project_id),
            now,
        )

    async def record_provider_failure(self, provider: str, reason: str, now: datetime) -> None:
        if not provider.strip():
            raise ValueError("provider is required")
        await self._pool.execute(
            """
            INSERT INTO app.provider_circuit_state
                (provider, state, consecutive_failures, last_failure_reason, opened_at, updated_at)
            VALUES ($1, 'closed', 1, $2, NULL, $3)
            ON CONFLICT (provider) DO UPDATE
            SET consecutive_failures = app.provider_circuit_state.consecutive_failures + 1,
                state = CASE WHEN app.provider_circuit_state.consecutive_failures >= 2 THEN 'open' ELSE 'closed' END,
                opened_at = CASE WHEN app.provider_circuit_state.consecutive_failures >= 2 THEN EXCLUDED.updated_at ELSE NULL END,
                last_failure_reason = EXCLUDED.last_failure_reason,
                updated_at = EXCLUDED.updated_at
            """,
            provider.strip(),
            reason[:1024],
            now,
        )

    async def clear_provider_failure(self, provider: str, now: datetime) -> None:
        await self._pool.execute(
            """
            INSERT INTO app.provider_circuit_state
                (provider, state, consecutive_failures, last_failure_reason, opened_at, updated_at)
            VALUES ($1, 'closed', 0, NULL, NULL, $2)
            ON CONFLICT (provider) DO UPDATE
            SET state = 'closed', consecutive_failures = 0, last_failure_reason = NULL,
                opened_at = NULL, updated_at = EXCLUDED.updated_at
            """,
            provider,
            now,
        )

    async def update_project(
        self,
        project_id: str,
        name: str,
        repository_mode: str,
        repository_url: str | None,
        local_repository_path: str | None,
        default_branch: str,
        required_runner_labels: Iterable[str],
        now: datetime,
        actor_user_id: str | None = None,
    ) -> ProjectRecord:
        normalized = _project_configuration(
            name,
            repository_mode,
            repository_url,
            local_repository_path,
            default_branch,
            required_runner_labels,
        )
        record = await self._pool.fetchrow(
            """
            UPDATE app.projects
            SET name = $2,
                repository_mode = $3,
                repository_url = $4,
                local_repository_path = $5,
                default_branch = $6,
                configuration = $7::jsonb,
                updated_at = $8
            WHERE id = $1
            RETURNING id, name, enabled
            """,
            _uuid(project_id),
            normalized["name"],
            normalized["repository_mode"],
            normalized["repository_url"],
            normalized["local_repository_path"],
            normalized["default_branch"],
            json.dumps({"required_runner_labels": normalized["required_runner_labels"]}),
            now,
        )
        if record is None:
            raise ValueError("project is unknown")
        project: ProjectRecord = {"id": str(record["id"]), "name": str(record["name"]), "enabled": bool(record["enabled"])}
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.update", "project", project["id"], "succeeded", now)
        return project

    async def set_project_enabled(
        self, project_id: str, enabled: bool, now: datetime, actor_user_id: str | None = None
    ) -> ProjectRecord:
        record = await self._pool.fetchrow(
            """
            UPDATE app.projects SET enabled = $2, updated_at = $3 WHERE id = $1
            RETURNING id, name, enabled
            """,
            _uuid(project_id),
            enabled,
            now,
        )
        if record is None:
            raise ValueError("project is unknown")
        project: ProjectRecord = {"id": str(record["id"]), "name": str(record["name"]), "enabled": bool(record["enabled"])}
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.enable" if enabled else "project.disable", "project", project["id"], "succeeded", now)
        return project

    @staticmethod
    def _hash(secret: str) -> str:
        return sha256(secret.encode("utf-8")).hexdigest()

    async def create_registration_token(
        self,
        allowed_labels: Iterable[str],
        expires_at: datetime,
        actor_user_id: str | None = None,
        now: datetime | None = None,
    ) -> str:
        token = token_urlsafe(32)
        labels = sorted({label.strip() for label in allowed_labels})
        if not labels or any(not label for label in labels):
            raise ValueError("runner labels are invalid")
        token_id = uuid4()
        created_at = now or expires_at
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                await connection.execute(
                    """
                    INSERT INTO app.runner_registration_tokens
                        (id, token_hash, created_by_user_id, allowed_labels, created_at, expires_at)
                    VALUES ($1, $2, $3, $4::jsonb, $5, $6)
                    """,
                    token_id,
                    self._hash(token),
                    _uuid_or_none(actor_user_id),
                    _json(labels),
                    created_at,
                    expires_at,
                )
                await AsyncpgAuthentication._append_audit(
                    connection,
                    actor_user_id=_uuid_or_none(actor_user_id),
                    action="runner.registration_token.create",
                    resource_type="runner_registration_token",
                    resource_id=str(token_id),
                    outcome="succeeded",
                    now=created_at,
                )
        return token

    async def list_registration_tokens(self) -> list[RegistrationTokenRecord]:
        records = await self._pool.fetch(
            """
            SELECT id, allowed_labels, created_at, expires_at, used_at, revoked_at
            FROM app.runner_registration_tokens
            ORDER BY created_at DESC, id DESC
            """
        )
        return [_registration_token(record) for record in records]

    async def revoke_registration_token(
        self, token_id: str, actor_user_id: str | None, now: datetime
    ) -> RegistrationTokenRecord:
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                record = await connection.fetchrow(
                    """
                    UPDATE app.runner_registration_tokens
                    SET revoked_at = $2
                    WHERE id = $1 AND used_at IS NULL AND revoked_at IS NULL
                    RETURNING id, allowed_labels, created_at, expires_at, used_at, revoked_at
                    """,
                    _uuid(token_id),
                    now,
                )
                if record is None:
                    raise ValueError("registration token is unknown or inactive")
                token = _registration_token(record)
                await AsyncpgAuthentication._append_audit(
                    connection,
                    actor_user_id=_uuid_or_none(actor_user_id),
                    action="runner.registration_token.revoke",
                    resource_type="runner_registration_token",
                    resource_id=str(token["id"]),
                    outcome="succeeded",
                    now=now,
                )
        return token

    async def register_runner(
        self, token: str, name: str, labels: Iterable[str], now: datetime, capacity: int = 1
    ) -> tuple[Runner, str]:
        if capacity < 1:
            raise RegistrationError("runner capacity must be a positive integer")
        labels_set = frozenset(labels)
        credential = token_urlsafe(32)
        runner_id = uuid4()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                registration = await connection.fetchrow(
                    """
                    SELECT id, allowed_labels
                    FROM app.runner_registration_tokens
                    WHERE token_hash = $1
                      AND used_at IS NULL
                      AND revoked_at IS NULL
                      AND expires_at > $2
                    FOR UPDATE
                    """,
                    self._hash(token),
                    now,
                )
                if registration is None:
                    raise RegistrationError("registration token cannot be used")
                allowed_labels = frozenset(_labels(registration["allowed_labels"]))
                if not labels_set.issubset(allowed_labels):
                    raise RegistrationError("runner labels exceed token permissions")
                await connection.execute(
                    """
                    INSERT INTO app.runners
                        (id, name, enabled, draining, status, version, labels, capabilities, registered_at, capacity)
                    VALUES ($1, $2, true, false, 'offline', '1.0', $3::jsonb, '{}'::jsonb, $4, $5)
                    """,
                    runner_id,
                    name,
                    _json(sorted(labels_set)),
                    now,
                    capacity,
                )
                await connection.execute(
                    """
                    INSERT INTO app.runner_credentials (id, runner_id, credential_hash, created_at)
                    VALUES ($1, $2, $3, $4)
                    """,
                    uuid4(),
                    runner_id,
                    self._hash(credential),
                    now,
                )
                consumed = await connection.execute(
                    """
                    UPDATE app.runner_registration_tokens
                    SET used_at = $1
                    WHERE id = $2 AND used_at IS NULL
                    """,
                    now,
                    registration["id"],
                )
                if consumed != "UPDATE 1":
                    raise RegistrationError("registration token cannot be used")
        return Runner(str(runner_id), labels_set, False, True, False, False, capacity=capacity), credential

    async def authenticate_runner(self, runner_id: str, credential: str, now: datetime) -> Runner:
        runner = await self._load_runner_credential(runner_id, now)
        if not compare_digest(runner[1], self._hash(credential)):
            raise AuthenticationError("runner credential is invalid")
        return runner[0]

    async def heartbeat(self, runner_id: str, credential: str, now: datetime) -> Runner:
        # Raises if the credential is invalid; the record itself is re-read below.
        await self.authenticate_runner(runner_id, credential, now)
        updated = await self._pool.fetchrow(
            """
            UPDATE app.runners
            SET status = 'online', last_seen_at = $2
            WHERE id = $1 AND enabled = true AND revoked_at IS NULL
            RETURNING id, labels, enabled, draining, capacity
            """,
            _uuid(runner_id),
            now,
        )
        if updated is None:
            raise AuthenticationError("runner is inactive")
        return _runner(updated, connected=True, healthy=True)

    async def list_workflows(self) -> list[WorkflowRecord]:
        records = await self._pool.fetch(
            """
            SELECT wr.id, wr.project_id, wr.status, wr.current_phase,
                   wr.pull_request_external_id, wr.pull_request_url, wr.blocking_reason,
                   wr.planning_attempts, wr.implementation_attempts, wr.pipeline_repair_attempts,
                   wr.review_cycles, wr.ci_repair_attempts, wr.total_agent_executions
            FROM app.workflow_runs AS wr
            ORDER BY wr.created_at DESC, wr.id ASC
            """
        )
        return [
            {
                "id": str(record["id"]),
                "project_id": str(record["project_id"]),
                "status": str(record["status"]),
                "phase": str(record["current_phase"]),
                "pull_request_external_id": _optional_text(record["pull_request_external_id"]),
                "pull_request_url": _optional_text(record["pull_request_url"]),
                "blocking_reason": _optional_text(record["blocking_reason"]),
                "planning_attempts": int(record["planning_attempts"]),
                "implementation_attempts": int(record["implementation_attempts"]),
                "pipeline_repair_attempts": int(record["pipeline_repair_attempts"]),
                "review_cycles": int(record["review_cycles"]),
                "ci_repair_attempts": int(record["ci_repair_attempts"]),
                "total_agent_executions": int(record["total_agent_executions"]),
            }
            for record in records
        ]

    async def set_runner_state(
        self, runner_id: str, state: str, actor_user_id: str | None, now: datetime
    ) -> RunnerRecord:
        if state not in {"enable", "disable", "drain", "revoke"}:
            raise ValueError("runner state is invalid")
        updates = {
            "enable": (True, False, None),
            "disable": (False, False, None),
            "drain": (True, True, None),
            "revoke": (False, True, now),
        }[state]
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                record = await connection.fetchrow(
                    """UPDATE app.runners SET enabled = $2, draining = $3,
                       revoked_at = COALESCE(revoked_at, $4), status = CASE WHEN $1 = 'revoke' THEN 'offline' ELSE status END
                       WHERE id = $5 RETURNING id, name, enabled, draining, status, labels, last_seen_at""",
                    state, updates[0], updates[1], updates[2], _uuid(runner_id),
                )
                if record is None:
                    raise ValueError("runner is unknown")
                if state == "revoke":
                    await connection.execute(
                        "UPDATE app.runner_credentials SET revoked_at = $2 WHERE runner_id = $1 AND revoked_at IS NULL",
                        _uuid(runner_id), now,
                    )
                if actor_user_id is not None:
                    await AsyncpgAuthentication._append_audit(
                        connection, actor_user_id=_uuid(actor_user_id), action=f"runner.{state}",
                        resource_type="runner", resource_id=runner_id, outcome="succeeded", now=now,
                    )
        return _runner_record(record)

    async def list_runners(self) -> list[RunnerRecord]:
        records = await self._pool.fetch(
            "SELECT id, name, enabled, draining, status, labels, last_seen_at FROM app.runners ORDER BY name ASC, id ASC"
        )
        return [
            {
                "id": str(record["id"]),
                "name": str(record["name"]),
                "enabled": bool(record["enabled"]),
                "draining": bool(record["draining"]),
                "status": str(record["status"]),
                "labels": record["labels"] if isinstance(record["labels"], list) else json.loads(record["labels"]),
                "last_seen_at": record["last_seen_at"],
            }
            for record in records
        ]

    async def record_human_decision(
        self,
        workflow_run_id: str,
        decision: str,
        comment: str | None,
        actor_user_id: str | None,
        now: datetime,
    ) -> dict[str, object]:
        if decision not in ("approved", "changes_requested"):
            raise ValueError("human decision is invalid")
        if actor_user_id is None:
            raise ValueError("human decision requires an authenticated user")
        approval_id = uuid4()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                workflow = await connection.fetchrow(
                    """
                    SELECT id, project_id, status
                    FROM app.workflow_runs
                    WHERE id = $1 AND status = 'waiting_human'
                    FOR UPDATE
                    """,
                    _uuid(workflow_run_id),
                )
                if workflow is None:
                    raise ValueError("workflow run is not awaiting human approval")
                await connection.execute(
                    """
                    INSERT INTO app.human_approvals
                        (id, workflow_run_id, commit_sha, user_id, decision, comment, created_at)
                    VALUES ($1, $2, '', $3, $4, $5, $6)
                    """,
                    approval_id,
                    _uuid(workflow_run_id),
                    _uuid(actor_user_id),
                    decision,
                    comment,
                    now,
                )
                await AsyncpgAuthentication._append_audit(
                    connection,
                    actor_user_id=_uuid(actor_user_id),
                    action=f"workflow.human_decision.{decision}",
                    resource_type="workflow_run",
                    resource_id=workflow_run_id,
                    outcome="succeeded",
                    now=now,
                )
        return {
            "id": str(workflow["id"]),
            "project_id": str(workflow["project_id"]),
            "status": str(workflow["status"]),
        }

    async def get_queued_execution_request(self, workflow_run_id: str) -> dict[str, Any] | None:
        return await self._pool.fetchrow(
            """
            SELECT id, role, attempt
            FROM app.workflow_execution_requests
            WHERE workflow_run_id = $1 AND status = 'queued'
            ORDER BY created_at ASC
            LIMIT 1
            """,
            _uuid(workflow_run_id),
        )

    async def build_task_packet(self, scheduled: ScheduledJob) -> dict[str, object]:
        record = await self._pool.fetchrow(
            """
            SELECT j.id AS job_id, i.external_id, i.title, i.body, p.id AS project_id,
                    p.repository_mode, p.repository_url, p.local_repository_path, p.default_branch,
                    w.current_commit, w.last_failure_fingerprint, w.blocking_reason,
                    request.id AS execution_request_id, request.role AS execution_role
            FROM app.jobs AS j
            JOIN app.workflow_runs AS w ON w.id = j.workflow_run_id
            JOIN app.issues AS i ON i.id = w.issue_id
            JOIN app.projects AS p ON p.id = j.project_id
            LEFT JOIN LATERAL (
                SELECT id, role
                FROM app.workflow_execution_requests
                WHERE workflow_run_id = w.id AND status = 'dispatched'
                ORDER BY dispatched_at DESC, id DESC
                LIMIT 1
            ) AS request ON true
            WHERE j.id = $1 AND j.status = 'offered'
            """,
            _uuid(scheduled.offer.job_id),
        )
        if record is None:
            raise ValueError("scheduled job is unavailable")
        job_id = str(record["job_id"])
        project_id = str(record["project_id"])
        issue_external_id = str(record["external_id"])
        issue_title = str(record["title"])
        issue_body = str(record["body"])
        repository_mode = str(record["repository_mode"])
        repository_url = _optional_text(record["repository_url"])
        local_repository_path = _optional_text(record["local_repository_path"])
        default_branch = str(record["default_branch"])
        current_commit = _optional_text(record.get("current_commit")) or ""
        prior_failure = _optional_text(record.get("last_failure_fingerprint"))
        blocking_reason = _optional_text(record.get("blocking_reason"))
        acceptance_criteria = (issue_title,)
        previous_failures = tuple(value for value in (prior_failure, blocking_reason) if value)
        request_id = record.get("execution_request_id")
        role = record.get("execution_role")
        if request_id is None or role is None:
            return build_task_packet(
                planner_task_execution(
                    job_id=job_id,
                    project_id=project_id,
                    issue_external_id=issue_external_id,
                    issue_title=issue_title,
                    issue_body=issue_body,
                    repository_mode=repository_mode,
                    repository_url=repository_url,
                    local_repository_path=local_repository_path,
                    default_branch=default_branch,
                )
            )
        if role == "pipeline":
            steps = await self._pool.fetch(
                """
                SELECT command, timeout_seconds
                FROM app.project_pipeline_steps
                WHERE project_id = $1 AND required = true
                ORDER BY position, id
                """,
                _uuid(project_id),
            )
            pipeline = tuple(
                PipelineCommand(command=str(step["command"]), timeout_seconds=int(step["timeout_seconds"]))
                for step in steps
            )
            return build_task_packet(
                pipeline_task_execution(
                    job_id=job_id,
                    execution_id=f"{request_id}-pipeline",
                    project_id=project_id,
                    issue_external_id=issue_external_id,
                    issue_title=issue_title,
                    issue_body=issue_body,
                    repository_mode=repository_mode,
                    repository_url=repository_url,
                    local_repository_path=local_repository_path,
                    default_branch=default_branch,
                    pipeline=pipeline,
                )
            )
        if role not in {"planner", "developer", "reviewer", "repairer"}:
            raise ValueError("workflow execution request role is invalid")
        return build_task_packet(
            task_execution(
                job_id=job_id,
                execution_id=f"{request_id}-{role_to_suffix(str(role))}",
                role=cast(ExecutionRole, str(role)),
                project_id=project_id,
                issue_external_id=issue_external_id,
                issue_title=issue_title,
                issue_body=issue_body,
                repository_mode=repository_mode,
                repository_url=repository_url,
                local_repository_path=local_repository_path,
                default_branch=default_branch,
                acceptance_criteria=acceptance_criteria,
                previous_failures=previous_failures,
                current_commit=current_commit,
            )
        )

    async def _claim_circuit_probes(
        self,
        connection: Any,
        project_id: Any,
        provider: str,
        workflow_id: UUID,
        now: datetime,
    ) -> bool:
        circuits = (
            ("app.project_circuit_state", "project_id", project_id),
            ("app.provider_circuit_state", "provider", provider),
        )
        locked: list[tuple[str, str, Any, Any]] = []
        for table, key, value in circuits:
            record = await connection.fetchrow(
                f"SELECT state, opened_at FROM {table} WHERE {key} = $1 FOR UPDATE", value
            )
            locked.append((table, key, value, record))
        for _, _, _, record in locked:
            if record is None or str(record["state"]) == "closed":
                continue
            if str(record["state"]) == "half_open":
                return False
            opened_at = record["opened_at"]
            if not isinstance(opened_at, datetime) or opened_at + self._circuit_probe_cooldown > now:
                return False
        for table, key, value, record in locked:
            if record is None or str(record["state"]) == "closed":
                continue
            claimed = await connection.execute(
                f"""
                UPDATE {table}
                SET state = 'half_open', probe_workflow_run_id = $2, updated_at = $3
                WHERE {key} = $1 AND state = 'open' AND probe_workflow_run_id IS NULL
                """,
                value,
                workflow_id,
                now,
            )
            if claimed != "UPDATE 1":
                return False
        return True

    async def schedule(self, now: datetime, offer_ttl: timedelta) -> ScheduledJob | None:
        if offer_ttl <= timedelta():
            raise ValueError("offer_ttl must be positive")
        expires_at = now + offer_ttl
        workflow_id = uuid4()
        job_id = uuid4()
        offer_id = uuid4()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                candidate = await connection.fetchrow(
                    """
                    SELECT i.id AS issue_id, i.project_id, i.provider, i.external_id, i.priority,
                           i.external_created_at, i.last_synced_at,
                           p.enabled, p.configuration, r.id AS runner_id, r.labels,
                           r.enabled AS runner_enabled, r.draining, r.status
                    FROM app.issues AS i
                    JOIN app.projects AS p ON p.id = i.project_id
                    JOIN app.runners AS r ON r.status = 'online'
                    WHERE i.eligible = true
                      AND p.enabled = true
                      AND r.enabled = true
                      AND r.draining = false
                      AND r.revoked_at IS NULL
                      AND r.labels @> COALESCE(p.configuration->'required_runner_labels', '[]'::jsonb)
                       AND NOT EXISTS (
                           SELECT 1 FROM app.project_locks AS lock
                           WHERE lock.project_id = p.id
                       )
                       AND NOT EXISTS (
                           SELECT 1 FROM app.project_circuit_state AS circuit
                           WHERE circuit.project_id = p.id
                             AND (circuit.state = 'half_open' OR (circuit.state = 'open' AND circuit.opened_at > $1))
                       )
                       AND NOT EXISTS (
                           SELECT 1 FROM app.provider_circuit_state AS circuit
                           WHERE circuit.provider = i.provider
                             AND (circuit.state = 'half_open' OR (circuit.state = 'open' AND circuit.opened_at > $1))
                       )
                       AND (
                           SELECT COUNT(*) FROM app.jobs AS active_job
                          WHERE active_job.runner_id = r.id
                            AND active_job.status IN ('offered', 'preparing', 'running')
                      ) < r.capacity
                    ORDER BY i.priority DESC, i.external_created_at, i.last_synced_at,
                             i.project_id, i.external_id, r.id
                    FOR UPDATE OF i, p, r SKIP LOCKED
                    LIMIT 1
                    """,
                    now - self._circuit_probe_cooldown,
                )
                if candidate is None:
                    return None
                if not await self._claim_circuit_probes(
                    connection,
                    candidate["project_id"],
                    str(candidate["provider"]),
                    workflow_id,
                    now,
                ):
                    return None
                await connection.execute(
                    """
                    INSERT INTO app.workflow_runs
                        (id, project_id, issue_id, thread_id, status, current_phase, created_at, updated_at)
                    VALUES ($1, $2, $3, $4, 'offered', 'offered', $5, $5)
                    """,
                    workflow_id,
                    candidate["project_id"],
                    candidate["issue_id"],
                    str(workflow_id),
                    now,
                )
                await connection.execute(
                    """
                    INSERT INTO app.project_locks (project_id, workflow_run_id, acquired_at, updated_at)
                    VALUES ($1, $2, $3, $3)
                    """,
                    candidate["project_id"],
                    workflow_id,
                    now,
                )
                await connection.execute(
                    """
                    INSERT INTO app.jobs
                        (id, workflow_run_id, project_id, runner_id, status, lease_generation,
                         lease_expires_at, offered_at)
                    VALUES ($1, $2, $3, $4, 'offered', 1, $5, $6)
                    """,
                    job_id,
                    workflow_id,
                    candidate["project_id"],
                    candidate["runner_id"],
                    expires_at,
                    now,
                )
                await connection.execute(
                    """
                    INSERT INTO app.job_offers (id, job_id, runner_id, status, created_at, expires_at)
                    VALUES ($1, $2, $3, 'offered', $4, $5)
                    """,
                    offer_id,
                    job_id,
                    candidate["runner_id"],
                    now,
                    expires_at,
                )
        issue = Issue(
            str(candidate["issue_id"]),
            str(candidate["project_id"]),
            str(candidate["external_id"]),
            int(candidate["priority"]),
            candidate["external_created_at"],
            candidate["last_synced_at"],
            True,
        )
        runner = Runner(
            str(candidate["runner_id"]),
            frozenset(_labels(candidate["labels"])),
            True,
            bool(candidate["runner_enabled"]),
            bool(candidate["draining"]),
            True,
        )
        workflow = Workflow(str(workflow_id), issue.project_id, issue.id, WorkflowStatus.OFFERED)
        lease = JobLease(str(job_id), runner.id, 1, expires_at)
        offer = JobOffer(str(job_id), workflow.id, issue.id, runner.id, expires_at, lease)
        return ScheduledJob(Assignment(issue, runner), workflow, offer)

    async def schedule_execution(self, now: datetime, offer_ttl: timedelta) -> ScheduledJob | None:
        if offer_ttl <= timedelta():
            raise ValueError("offer_ttl must be positive")
        expires_at = now + offer_ttl
        offer_id = uuid4()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                candidate = await connection.fetchrow(
                    """
                    SELECT request.id AS request_id, j.id AS job_id, w.id AS workflow_run_id,
                           i.id AS issue_id, i.project_id, i.external_id, i.priority,
                           i.external_created_at, i.last_synced_at, r.id AS runner_id, r.labels,
                           r.enabled AS runner_enabled, r.draining, r.status
                    FROM app.workflow_execution_requests AS request
                    JOIN app.workflow_runs AS w ON w.id = request.workflow_run_id
                    JOIN app.jobs AS j ON j.workflow_run_id = w.id
                    JOIN app.issues AS i ON i.id = w.issue_id
                    JOIN app.projects AS p ON p.id = j.project_id
                    JOIN app.runners AS r ON r.status = 'online'
                    WHERE request.status = 'queued'
                      AND p.enabled = true
                      AND r.enabled = true
                      AND r.draining = false
                      AND r.revoked_at IS NULL
                      AND r.labels @> COALESCE(p.configuration->'required_runner_labels', '[]'::jsonb)
                      AND (
                          SELECT COUNT(*) FROM app.jobs AS active_job
                          WHERE active_job.runner_id = r.id
                            AND active_job.status IN ('offered', 'preparing', 'running')
                      ) < r.capacity
                    ORDER BY request.created_at, request.id, r.id
                    FOR UPDATE OF request, j, w, i, p, r SKIP LOCKED
                    LIMIT 1
                    """
                )
                if candidate is None:
                    return None
                updated = await connection.fetchrow(
                    """
                    UPDATE app.jobs
                    SET runner_id = $2, status = 'offered', lease_generation = lease_generation + 1,
                        lease_expires_at = $3, offered_at = $4, accepted_at = NULL, started_at = NULL,
                        finished_at = NULL, last_event_sequence = 0
                    WHERE id = $1 AND status IN ('completed', 'failed', 'cancelled')
                    RETURNING lease_generation
                    """,
                    candidate["job_id"],
                    candidate["runner_id"],
                    expires_at,
                    now,
                )
                if updated is None:
                    return None
                changed = await connection.execute(
                    """
                    UPDATE app.workflow_execution_requests
                    SET status = 'dispatched', dispatched_at = $2
                    WHERE id = $1 AND status = 'queued'
                    """,
                    candidate["request_id"],
                    now,
                )
                if changed != "UPDATE 1":
                    raise RuntimeError("workflow execution request claim was lost")
                await connection.execute(
                    """
                    INSERT INTO app.job_offers (id, job_id, runner_id, status, created_at, expires_at)
                    VALUES ($1, $2, $3, 'offered', $4, $5)
                    """,
                    offer_id,
                    candidate["job_id"],
                    candidate["runner_id"],
                    now,
                    expires_at,
                )
        issue = Issue(
            str(candidate["issue_id"]),
            str(candidate["project_id"]),
            str(candidate["external_id"]),
            int(candidate["priority"]),
            candidate["external_created_at"],
            candidate["last_synced_at"],
            True,
        )
        runner = Runner(
            str(candidate["runner_id"]),
            frozenset(_labels(candidate["labels"])),
            True,
            bool(candidate["runner_enabled"]),
            bool(candidate["draining"]),
            True,
        )
        workflow = Workflow(
            str(candidate["workflow_run_id"]), issue.project_id, issue.id, WorkflowStatus.IMPLEMENTING
        )
        generation = int(updated["lease_generation"])
        lease = JobLease(str(candidate["job_id"]), runner.id, generation, expires_at)
        offer = JobOffer(str(candidate["job_id"]), workflow.id, issue.id, runner.id, expires_at, lease)
        return ScheduledJob(Assignment(issue, runner), workflow, offer)

    async def expire_offers(self, now: datetime) -> tuple[str, ...]:
        expired: list[str] = []
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                while True:
                    offer = await connection.fetchrow(
                        """
                        UPDATE app.job_offers
                        SET status = 'expired', responded_at = $1
                        WHERE id = (
                            SELECT id FROM app.job_offers
                            WHERE status = 'offered' AND expires_at <= $1
                            ORDER BY expires_at, id
                            FOR UPDATE SKIP LOCKED
                            LIMIT 1
                        )
                        RETURNING job_id
                        """,
                        now,
                    )
                    if offer is None:
                        break
                    job = await connection.fetchrow(
                        """
                        UPDATE app.jobs
                        SET status = 'cancelled', finished_at = $2, recovery_reason = 'offer_expired'
                        WHERE id = $1 AND status = 'offered'
                        RETURNING workflow_run_id, project_id
                        """,
                        offer["job_id"],
                        now,
                    )
                    if job is None:
                        continue
                    await connection.execute(
                        """
                        UPDATE app.workflow_runs
                        SET status = 'cancelled', current_phase = 'cancelled', terminal_reason = 'offer_expired',
                            completed_at = $2, updated_at = $2
                        WHERE id = $1
                        """,
                        job["workflow_run_id"],
                        now,
                    )
                    await connection.execute(
                        "DELETE FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
                        job["project_id"],
                        job["workflow_run_id"],
                    )
                    expired.append(str(offer["job_id"]))
        return tuple(expired)

    async def expire_leases(self, now: datetime) -> tuple[str, ...]:
        expired: list[str] = []
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                while True:
                    job = await connection.fetchrow(
                        """
                        UPDATE app.jobs
                        SET status = 'recovering', lease_generation = lease_generation + 1,
                            recovery_reason = 'lease_expired'
                        WHERE id = (
                            SELECT id FROM app.jobs
                            WHERE status IN ('preparing', 'running') AND lease_expires_at <= $1
                            ORDER BY lease_expires_at, id
                            FOR UPDATE SKIP LOCKED
                            LIMIT 1
                        )
                        RETURNING id, workflow_run_id, runner_id, lease_generation
                        """,
                        now,
                    )
                    if job is None:
                        break
                    await connection.execute(
                        """
                        UPDATE app.workflow_runs
                        SET status = 'recovering', current_phase = 'recovering', updated_at = $2
                        WHERE id = $1
                        """,
                        job["workflow_run_id"],
                        now,
                    )
                    await connection.execute(
                        "UPDATE app.runners SET status = 'offline' WHERE id = $1",
                        job["runner_id"],
                    )
                    expired.append(str(job["id"]))
        return tuple(expired)

    async def recover_one(self, now: datetime, offer_ttl: timedelta) -> ScheduledJob | None:
        if offer_ttl <= timedelta():
            raise ValueError("offer_ttl must be positive")
        expires_at = now + offer_ttl
        offer_id = uuid4()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                candidate = await connection.fetchrow(
                    """
                    SELECT j.id AS job_id, j.workflow_run_id, j.project_id, j.lease_generation,
                           i.id AS issue_id, i.external_id, i.priority, i.external_created_at, i.last_synced_at,
                           r.id AS runner_id, r.labels, r.enabled AS runner_enabled, r.draining
                    FROM app.jobs AS j
                    JOIN app.workflow_runs AS w ON w.id = j.workflow_run_id
                    JOIN app.project_locks AS lock ON lock.workflow_run_id = w.id
                    JOIN app.issues AS i ON i.id = w.issue_id
                    JOIN app.projects AS p ON p.id = j.project_id
                    JOIN app.runners AS r ON r.status = 'online'
                    WHERE j.status = 'recovering'
                      AND w.status = 'recovering'
                      AND p.enabled = true
                      AND r.enabled = true
                      AND r.draining = false
                      AND r.revoked_at IS NULL
                      AND r.labels @> COALESCE(p.configuration->'required_runner_labels', '[]'::jsonb)
                      AND (
                          SELECT COUNT(*) FROM app.jobs AS active_job
                          WHERE active_job.runner_id = r.id
                            AND active_job.status IN ('offered', 'preparing', 'running')
                      ) < r.capacity
                    ORDER BY i.priority DESC, i.external_created_at, i.last_synced_at, i.project_id, i.external_id, r.id
                    FOR UPDATE OF j, w, lock, i, p, r SKIP LOCKED
                    LIMIT 1
                    """
                )
                if candidate is None:
                    return None
                updated = await connection.fetchrow(
                    """
                    UPDATE app.jobs
                    SET runner_id = $2, status = 'offered', lease_expires_at = $3, offered_at = $4,
                        accepted_at = NULL, started_at = NULL, finished_at = NULL, last_event_sequence = 0
                    WHERE id = $1 AND status = 'recovering' AND lease_generation = $5
                    RETURNING lease_generation
                    """,
                    candidate["job_id"],
                    candidate["runner_id"],
                    expires_at,
                    now,
                    candidate["lease_generation"],
                )
                if updated is None:
                    return None
                await connection.execute(
                    """
                    UPDATE app.workflow_runs
                    SET status = 'offered', current_phase = 'recovering', updated_at = $2
                    WHERE id = $1 AND status = 'recovering'
                    """,
                    candidate["workflow_run_id"],
                    now,
                )
                await connection.execute(
                    """
                    INSERT INTO app.job_offers (id, job_id, runner_id, status, created_at, expires_at)
                    VALUES ($1, $2, $3, 'offered', $4, $5)
                    """,
                    offer_id,
                    candidate["job_id"],
                    candidate["runner_id"],
                    now,
                    expires_at,
                )
                await connection.execute(
                    """
                    INSERT INTO app.workflow_events (workflow_run_id, event_type, severity, payload, created_at)
                    VALUES ($1, 'lease_recovery_offered', 'warning', $2::jsonb, $3)
                    """,
                    candidate["workflow_run_id"],
                    json.dumps(
                        {
                            "job_id": str(candidate["job_id"]),
                            "runner_id": str(candidate["runner_id"]),
                            "lease_generation": int(updated["lease_generation"]),
                        },
                        separators=(",", ":"),
                    ),
                    now,
                )
        issue = Issue(
            str(candidate["issue_id"]),
            str(candidate["project_id"]),
            str(candidate["external_id"]),
            int(candidate["priority"]),
            candidate["external_created_at"],
            candidate["last_synced_at"],
            True,
        )
        runner = Runner(
            str(candidate["runner_id"]),
            frozenset(_labels(candidate["labels"])),
            True,
            bool(candidate["runner_enabled"]),
            bool(candidate["draining"]),
            True,
        )
        workflow = Workflow(str(candidate["workflow_run_id"]), issue.project_id, issue.id, WorkflowStatus.OFFERED)
        lease = JobLease(str(candidate["job_id"]), runner.id, int(updated["lease_generation"]), expires_at)
        offer = JobOffer(str(candidate["job_id"]), workflow.id, issue.id, runner.id, expires_at, lease)
        return ScheduledJob(Assignment(issue, runner), workflow, offer)

    async def accept_offer(self, job_id: str, runner_id: str, now: datetime) -> JobLease:
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                offer = await connection.fetchrow(
                    """
                    UPDATE app.job_offers
                    SET status = 'accepted', responded_at = $3
                    WHERE job_id = $1 AND runner_id = $2 AND status = 'offered' AND expires_at > $3
                    RETURNING job_id
                    """,
                    _uuid(job_id),
                    _uuid(runner_id),
                    now,
                )
                if offer is None:
                    raise OfferError("job offer is no longer active")
                job = await connection.fetchrow(
                    """
                    UPDATE app.jobs
                    SET status = 'preparing', accepted_at = $3
                    WHERE id = $1 AND runner_id = $2 AND status = 'offered'
                    RETURNING lease_generation, lease_expires_at
                    """,
                    _uuid(job_id),
                    _uuid(runner_id),
                    now,
                )
                if job is None:
                    raise OfferError("job offer is no longer active")
                await connection.execute(
                    """
                    UPDATE app.workflow_runs SET status = 'preparing', current_phase = 'preparing', updated_at = $2
                    WHERE id = (SELECT workflow_run_id FROM app.jobs WHERE id = $1)
                    """,
                    _uuid(job_id),
                    now,
                )
        return JobLease(str(job_id), runner_id, int(job["lease_generation"]), job["lease_expires_at"])

    async def reject_offer(self, job_id: str, runner_id: str, now: datetime) -> None:
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                offer = await connection.fetchrow(
                    """
                    UPDATE app.job_offers
                    SET status = 'rejected', responded_at = $3
                    WHERE job_id = $1 AND runner_id = $2 AND status = 'offered' AND expires_at > $3
                    RETURNING job_id
                    """,
                    _uuid(job_id),
                    _uuid(runner_id),
                    now,
                )
                if offer is None:
                    raise OfferError("job offer is no longer active")
                job = await connection.fetchrow(
                    """
                    UPDATE app.jobs
                    SET status = 'cancelled', finished_at = $3, recovery_reason = 'runner_rejected_offer'
                    WHERE id = $1 AND runner_id = $2 AND status = 'offered'
                    RETURNING workflow_run_id, project_id
                    """,
                    _uuid(job_id),
                    _uuid(runner_id),
                    now,
                )
                if job is None:
                    raise OfferError("job offer is no longer active")
                await connection.execute(
                    """
                    UPDATE app.workflow_runs
                    SET status = 'cancelled', current_phase = 'cancelled', updated_at = $2
                    WHERE id = $1
                    """,
                    job["workflow_run_id"],
                    now,
                )
                await connection.execute(
                    "DELETE FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
                    job["project_id"],
                    job["workflow_run_id"],
                )

    async def renew_lease(
        self, job_id: str, runner_id: str, generation: int, expires_at: datetime, now: datetime
    ) -> JobLease:
        if expires_at <= now:
            raise ValueError("renewed lease expiry must be in the future")
        job = await self._pool.fetchrow(
            """
            UPDATE app.jobs
            SET lease_expires_at = $5
            WHERE id = $1 AND runner_id = $2 AND status IN ('preparing', 'running')
              AND lease_generation = $3 AND lease_expires_at > $4
            RETURNING lease_generation, lease_expires_at, last_event_sequence
            """,
            _uuid(job_id),
            _uuid(runner_id),
            generation,
            now,
            expires_at,
        )
        if job is None:
            raise StaleLeaseError("cannot renew a stale lease")
        return JobLease(str(job_id), runner_id, int(job["lease_generation"]), job["lease_expires_at"], int(job["last_event_sequence"]))

    async def accept_event(
        self,
        event: ExecutionEvent,
        now: datetime,
        on_transition: Callable[[str, str, dict[str, object]], Awaitable[None]] | None = None,
    ) -> JobLease:
        summary = validate_runner_event(event.event_type, event.execution_id, event.payload)
        outbox_id: UUID | None = None
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                job = await connection.fetchrow(
                    """
                    SELECT j.id, j.workflow_run_id, j.lease_generation, j.lease_expires_at,
                           j.last_event_sequence, w.status AS workflow_status, w.last_diff_hash,
                           w.last_failure_fingerprint, w.non_progress_attempts
                    FROM app.jobs AS j
                    JOIN app.workflow_runs AS w ON w.id = j.workflow_run_id
                    WHERE j.id = $1 AND j.runner_id = $2 AND j.status IN ('preparing', 'running')
                      AND j.lease_generation = $3 AND j.lease_expires_at > $4
                      AND j.last_event_sequence < $5
                    FOR UPDATE OF j
                    """,
                    _uuid(event.job_id),
                    _uuid(event.runner_id),
                    event.lease_generation,
                    now,
                    event.event_sequence,
                )
                if job is None:
                    raise StaleLeaseError("runner execution event was rejected")

                resolved = await self._resolve_dispatched_execution(
                    connection, str(job["workflow_run_id"]), event.job_id, event.execution_id
                )
                if summary.terminal and resolved is None:
                    # The runner's execution ID does not correspond to a request this
                    # orchestrator actually dispatched for this job: never let it drive
                    # a workflow transition (that is the gate #8 closes).
                    raise StaleLeaseError("runner execution event was rejected")
                resolved_role, resolved_attempt = resolved if resolved is not None else (None, None)

                current_workflow_status = str(job["workflow_status"])
                transition = workflow_transition_for_terminal_event(
                    summary, current_workflow_status, role=resolved_role
                )
                progress_updates = await self._record_progress_evidence(connection, job, summary, event.payload, now)
                non_progress_attempts = progress_updates.get("non_progress_attempts", 0)
                if isinstance(non_progress_attempts, int) and non_progress_attempts >= 4:
                    transition = WorkflowTransition(
                        new_status="blocked",
                        state_updates={
                            **(transition.state_updates if transition is not None else {}),
                            **progress_updates,
                            "status": "blocked",
                            "blocking_reason": "workflow stopped after four identical execution outcomes",
                        },
                    )

                if transition is not None:
                    await connection.execute(
                        """
                        UPDATE app.jobs
                        SET last_event_sequence = $2, status = $3,
                            started_at = COALESCE(started_at, $4), finished_at = $4
                        WHERE id = $1 AND last_event_sequence < $2
                        """,
                        _uuid(event.job_id),
                        event.event_sequence,
                        summary.event_type,
                        now,
                    )
                else:
                    await connection.execute(
                        """
                        UPDATE app.jobs
                        SET last_event_sequence = $2,
                            status = CASE WHEN status = 'preparing' THEN 'running' ELSE status END,
                            started_at = COALESCE(started_at, $3)
                        WHERE id = $1 AND last_event_sequence < $2
                        """,
                        _uuid(event.job_id),
                        event.event_sequence,
                        now,
                    )

                await connection.execute(
                    """
                    INSERT INTO app.workflow_events (workflow_run_id, event_type, severity, payload, created_at)
                    VALUES ($1, 'runner_execution_event', 'info', $2::jsonb, $3)
                    """,
                    job["workflow_run_id"],
                    _json_payload(event),
                    now,
                )

                execution_type = _EXECUTION_TYPE_BY_ROLE.get(resolved_role) if resolved_role else None
                attempt = resolved_attempt if resolved_attempt is not None else 0
                if execution_type is not None:
                    if summary.event_type == "started":
                        await connection.execute(
                            """
                            INSERT INTO app.executions
                                (id, job_id, execution_type, attempt, status, lease_generation, started_at, timeout_seconds)
                            VALUES (gen_random_uuid(), $1, $2, $3, 'running', $4, $5, 3600)
                            ON CONFLICT (job_id, execution_type, attempt) DO NOTHING
                            """,
                            _uuid(event.job_id),
                            execution_type,
                            attempt,
                            event.lease_generation,
                            now,
                        )
                    elif summary.terminal:
                        result_json = (
                            json.dumps(
                                {"changedFiles": summary.changed_files, "commandsRun": summary.commands_run},
                                separators=(",", ":"),
                            )
                            if summary.succeeded
                            else None
                        )
                        await connection.execute(
                            """
                            UPDATE app.executions
                            SET status = $3, finished_at = $4, exit_code = $5, result = $6::jsonb
                            WHERE job_id = $1 AND execution_type = $2 AND lease_generation = $7
                              AND status = 'running' AND attempt = $8
                            """,
                            _uuid(event.job_id),
                            execution_type,
                            summary.event_type,
                            now,
                            summary.exit_code,
                            result_json,
                            event.lease_generation,
                            attempt,
                        )

                if resolved_role == "pipeline" and summary.terminal:
                    await connection.execute(
                        """
                        INSERT INTO app.pipeline_runs
                            (id, workflow_run_id, commit_sha, status, result, started_at, finished_at)
                        VALUES (gen_random_uuid(), $1, '', $2, $3::jsonb, $4, $4)
                        """,
                        job["workflow_run_id"],
                        "passed" if summary.succeeded else "failed",
                        json.dumps(
                            {"exitCode": summary.exit_code, "commandsRun": summary.commands_run},
                            separators=(",", ":"),
                            sort_keys=True,
                        ),
                        now,
                    )

                if resolved_role == "reviewer" and summary.terminal and summary.succeeded:
                    await self._record_ai_review(connection, job["workflow_run_id"], summary, now)

                if transition is not None:
                    new_wf_status = transition.new_status
                    await connection.execute(
                        """
                        UPDATE app.workflow_runs
                        SET status = $2, current_phase = $2, updated_at = $3
                        WHERE id = $1
                        """,
                        job["workflow_run_id"],
                        new_wf_status,
                        now,
                    )
                    await connection.execute(
                        """
                        UPDATE app.runners SET status = 'online'
                        WHERE id = $1
                        """,
                        _uuid(event.runner_id),
                    )
                    outbox_id = uuid4()
                    await connection.execute(
                        """
                        INSERT INTO app.workflow_transition_outbox
                            (id, workflow_run_id, new_status, state_updates, status, created_at)
                        VALUES ($1, $2, $3, $4::jsonb, 'pending', $5)
                        """,
                        outbox_id,
                        job["workflow_run_id"],
                        new_wf_status,
                        json.dumps(transition.state_updates, separators=(",", ":"), sort_keys=True),
                        now,
                    )

        if transition is not None and on_transition is not None and outbox_id is not None:
            await self._drain_outbox_entry(
                outbox_id, str(job["workflow_run_id"]), transition.new_status, transition.state_updates,
                on_transition, now,
            )

        updated_sequence = max(int(job["last_event_sequence"]), event.event_sequence)
        return JobLease(event.job_id, event.runner_id, int(job["lease_generation"]), job["lease_expires_at"], updated_sequence)

    async def _record_progress_evidence(
        self,
        connection: Any,
        job: Any,
        summary: Any,
        payload: dict[str, Any],
        now: datetime,
    ) -> dict[str, object]:
        if not summary.terminal:
            return {}
        diff_hash = (
            sha256(json.dumps(sorted(summary.changed_files), separators=(",", ":")).encode("utf-8")).hexdigest()
            if summary.succeeded
            else None
        )
        fingerprint = sha256(
            json.dumps(
                {
                    "event_type": summary.event_type,
                    "exit_code": summary.exit_code,
                    "changed_files": sorted(summary.changed_files),
                    "result": summary.result,
                    "payload": payload,
                },
                separators=(",", ":"),
                sort_keys=True,
                default=str,
            ).encode("utf-8")
        ).hexdigest()
        current = diff_hash or fingerprint
        previous = _optional_text(job.get("last_diff_hash")) or _optional_text(
            job.get("last_failure_fingerprint")
        )
        same_outcome = previous == current
        attempts = int(job.get("non_progress_attempts") or 0) + 1 if same_outcome else 0
        await connection.execute(
            """
            UPDATE app.workflow_runs
            SET last_diff_hash = $2,
                last_failure_fingerprint = $3,
                non_progress_attempts = $4,
                last_progress_at = CASE WHEN $5 THEN $6 ELSE last_progress_at END,
                updated_at = $6
            WHERE id = $1
            """,
            job["workflow_run_id"],
            diff_hash,
            fingerprint,
            attempts,
            not same_outcome,
            now,
        )
        return {
            "last_diff_hash": diff_hash or "",
            "last_failure_fingerprint": fingerprint,
            "non_progress_attempts": attempts,
            "progressed": not same_outcome,
        }

    async def _resolve_dispatched_execution(
        self, connection: Any, workflow_run_id: str, job_id: str, execution_id: str
    ) -> tuple[str, int] | None:
        """Resolves (role, attempt) for an execution ID by joining the
        dispatched request it claims to be -- never by trusting the string
        itself. Returns None when the execution ID does not correspond to
        anything this orchestrator actually dispatched for this job.
        """
        if execution_id == f"{job_id}-plan":
            # The very first planning dispatch for a workflow predates any
            # app.workflow_execution_requests row (schedule() bootstraps it
            # directly), so there is nothing to join on yet. It is only
            # trustworthy as long as that remains true for this workflow --
            # job_id is itself bound to a single workflow by the lease check
            # above, so this cannot be replayed against a different job.
            existing = await connection.fetchrow(
                "SELECT 1 AS present FROM app.workflow_execution_requests WHERE workflow_run_id = $1 LIMIT 1",
                _uuid(workflow_run_id),
            )
            if existing is None:
                return "planner", 1
            return None

        candidate = execution_id[:36]
        try:
            request_id = UUID(candidate)
        except ValueError:
            return None
        if execution_id[36:37] != "-":
            return None
        row = await connection.fetchrow(
            """
            SELECT role, attempt
            FROM app.workflow_execution_requests
            WHERE id = $1 AND workflow_run_id = $2 AND status = 'dispatched'
            """,
            request_id,
            _uuid(workflow_run_id),
        )
        if row is not None:
            return str(row["role"]), int(row["attempt"])
        return None

    async def _record_ai_review(
        self, connection: Any, workflow_run_id: Any, summary: Any, now: datetime
    ) -> None:
        result = summary.result if isinstance(summary.result, dict) else None
        verdict = result.get("verdict") if result is not None else None
        if verdict not in {"approved", "changes_requested", "human_required", "invalid"}:
            verdict = "invalid"
        findings = result.get("findings", []) if result is not None else []
        await connection.execute(
            """
            INSERT INTO app.ai_reviews (id, workflow_run_id, commit_sha, verdict, result, created_at)
            VALUES (gen_random_uuid(), $1, $2, $3, $4::jsonb, $5)
            """,
            workflow_run_id,
            "",
            verdict,
            json.dumps({"verdict": verdict, "findings": findings, "raw": result or {}}, separators=(",", ":"), sort_keys=True),
            now,
        )

    async def _drain_outbox_entry(
        self,
        outbox_id: UUID,
        workflow_run_id: str,
        new_status: str,
        state_updates: dict[str, object],
        on_transition: Callable[[str, str, dict[str, object]], Awaitable[None]],
        now: datetime,
    ) -> None:
        try:
            await on_transition(workflow_run_id, new_status, state_updates)
        except Exception as error:  # noqa: BLE001 - leaves the outbox row pending for the background drain to retry
            await self._pool.execute(
                """
                UPDATE app.workflow_transition_outbox
                SET attempts = attempts + 1, last_error = $2
                WHERE id = $1 AND status = 'pending'
                """,
                outbox_id,
                str(error)[:1024],
            )
            return
        await self._pool.execute(
            """
            UPDATE app.workflow_transition_outbox
            SET status = 'processed', processed_at = $2
            WHERE id = $1
            """,
            outbox_id,
            now,
        )

    async def drain_pending_transitions(
        self,
        on_transition: Callable[[str, str, dict[str, object]], Awaitable[None]],
        now: datetime,
        limit: int = 50,
    ) -> int:
        """At-least-once delivery for transitions an earlier accept_event call
        committed but never finished invoking (process crash, DB blip, etc).
        Intended to be polled periodically by a background worker."""
        async with self._pool.acquire() as connection:
            rows = await connection.fetch(
                """
                UPDATE app.workflow_transition_outbox
                SET status = 'processing'
                WHERE id IN (
                    SELECT id FROM app.workflow_transition_outbox
                    WHERE status = 'pending'
                    ORDER BY created_at
                    LIMIT $1
                    FOR UPDATE SKIP LOCKED
                )
                RETURNING id, workflow_run_id, new_status, state_updates
                """,
                limit,
            )
        processed = 0
        for row in rows:
            state_updates = row["state_updates"]
            if isinstance(state_updates, str):
                state_updates = json.loads(state_updates)
            try:
                await on_transition(str(row["workflow_run_id"]), str(row["new_status"]), state_updates)
            except Exception as error:  # noqa: BLE001 - leaves the outbox row pending for the next drain pass
                await self._pool.execute(
                    """
                    UPDATE app.workflow_transition_outbox
                    SET status = 'pending', attempts = attempts + 1, last_error = $2
                    WHERE id = $1
                    """,
                    row["id"],
                    str(error)[:1024],
                )
                continue
            await self._pool.execute(
                """
                UPDATE app.workflow_transition_outbox
                SET status = 'processed', processed_at = $2
                WHERE id = $1
                """,
                row["id"],
                now,
            )
            processed += 1
        return processed

    async def find_stalled_workflow_runs(self, now: datetime, stale_after: timedelta) -> tuple[str, ...]:
        """Workflow runs whose status implies in-flight work but which have
        no queued/dispatched execution request and no active job -- the
        symptom of a crash between committing a transition and invoking the
        graph runtime (see accept_event / the outbox above)."""
        rows = await self._pool.fetch(
            """
            SELECT wr.id
            FROM app.workflow_runs AS wr
            WHERE wr.status NOT IN ('completed', 'blocked', 'failed', 'cancelled', 'offered', 'preparing')
              AND wr.updated_at <= $1
              AND NOT EXISTS (
                  SELECT 1 FROM app.workflow_execution_requests AS req
                  WHERE req.workflow_run_id = wr.id AND req.status IN ('queued', 'dispatched')
              )
              AND NOT EXISTS (
                  SELECT 1 FROM app.jobs AS j
                  WHERE j.workflow_run_id = wr.id AND j.status IN ('offered', 'preparing', 'running')
              )
            """,
            now - stale_after,
        )
        return tuple(str(row["id"]) for row in rows)

    async def recover_stalled_workflow_run(
        self,
        workflow_run_id: str,
        on_transition: Callable[[str, str, dict[str, object]], Awaitable[None]],
    ) -> bool:
        """Re-enters the graph runtime for a stalled run using its last
        checkpoint, with no new state updates, so the current node's
        dispatch logic re-evaluates and (idempotently) resumes progress."""
        record = await self._pool.fetchrow(
            "SELECT status FROM app.workflow_runs WHERE id = $1",
            _uuid(workflow_run_id),
        )
        if record is None:
            return False
        await on_transition(workflow_run_id, str(record["status"]), {})
        return True

    async def _load_runner_credential(self, runner_id: str, now: datetime) -> tuple[Runner, str]:
        record = await self._pool.fetchrow(
            """
            SELECT r.id, r.labels, r.enabled, r.draining, r.status, r.capacity, c.credential_hash
            FROM app.runners AS r
            JOIN app.runner_credentials AS c ON c.runner_id = r.id
            WHERE r.id = $1
              AND r.revoked_at IS NULL
              AND c.revoked_at IS NULL
              AND (c.expires_at IS NULL OR c.expires_at > $2)
            ORDER BY c.created_at DESC
            LIMIT 1
            """,
            _uuid(runner_id),
            now,
        )
        if record is None:
            raise AuthenticationError("runner is unknown")
        return _runner(record), str(record["credential_hash"])


def _project_configuration(
    name: str,
    repository_mode: str,
    repository_url: str | None,
    local_repository_path: str | None,
    default_branch: str,
    required_runner_labels: Iterable[str],
) -> dict[str, object]:
    normalized_name = name.strip()
    normalized_branch = default_branch.strip()
    labels = sorted({label.strip() for label in required_runner_labels})
    if not normalized_name or len(normalized_name) > 256 or not normalized_branch:
        raise ValueError("project configuration is invalid")
    if any(not label for label in labels):
        raise ValueError("runner labels are invalid")
    if repository_mode == "managed_clone":
        if not repository_url or local_repository_path:
            raise ValueError("managed clone source is invalid")
        return {
            "name": normalized_name,
            "repository_mode": repository_mode,
            "repository_url": repository_url.strip(),
            "local_repository_path": None,
            "default_branch": normalized_branch,
            "required_runner_labels": labels,
        }
    if repository_mode == "existing_path":
        if repository_url or not local_repository_path or not PurePath(local_repository_path).is_absolute():
            raise ValueError("existing path source is invalid")
        return {
            "name": normalized_name,
            "repository_mode": repository_mode,
            "repository_url": None,
            "local_repository_path": str(PurePath(local_repository_path)),
            "default_branch": normalized_branch,
            "required_runner_labels": labels,
        }
    raise ValueError("repository mode is invalid")


def _optional_text(value: Any) -> str | None:
    if value is None:
        return None
    return str(value)


def _json(values: list[str]) -> str:
    import json

    return json.dumps(values, separators=(",", ":"))


def _json_payload(event: ExecutionEvent) -> str:
    import json

    return json.dumps(
        {
            "job_id": event.job_id,
            "runner_id": event.runner_id,
            "lease_generation": event.lease_generation,
            "event_sequence": event.event_sequence,
            "payload": event.payload,
        },
        separators=(",", ":"),
        sort_keys=True,
    )


def _registration_token(record: Any) -> RegistrationTokenRecord:
    return {
        "id": str(record["id"]),
        "allowed_labels": _labels(record["allowed_labels"]),
        "created_at": record["created_at"],
        "expires_at": record["expires_at"],
        "used_at": record["used_at"],
        "revoked_at": record["revoked_at"],
    }


def _labels(value: Any) -> list[str]:
    if isinstance(value, str):
        import json

        value = json.loads(value)
    if not isinstance(value, list) or any(not isinstance(label, str) for label in value):
        raise AuthenticationError("runner labels are invalid")
    return value


def _runner_record(record: Any) -> RunnerRecord:
    return {
        "id": str(record["id"]),
        "name": str(record["name"]),
        "enabled": bool(record["enabled"]),
        "draining": bool(record["draining"]),
        "status": str(record["status"]),
        "labels": _labels(record["labels"]),
        "last_seen_at": record["last_seen_at"],
    }


def _runner(record: Any, *, connected: bool | None = None, healthy: bool | None = None) -> Runner:
    status = str(record.get("status", "offline"))
    return Runner(
        str(record["id"]),
        frozenset(_labels(record["labels"])),
        status == "online" if connected is None else connected,
        bool(record["enabled"]),
        bool(record["draining"]),
        status == "online" if healthy is None else healthy,
        capacity=int(record.get("capacity", 1)),
    )


def _uuid(value: str) -> Any:
    from uuid import UUID

    try:
        return UUID(value)
    except ValueError as error:
        raise AuthenticationError("runner is unknown") from error


def _uuid_or_none(value: str | None) -> Any:
    if value is None:
        return None
    return _uuid(value)


_EXECUTION_TYPE_BY_ROLE: dict[str, str] = {
    "planner": "run_planner",
    "developer": "run_developer",
    "pipeline": "run_local_pipeline",
    "reviewer": "run_reviewer",
    "repairer": "run_repair",
}
