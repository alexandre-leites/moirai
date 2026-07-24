from __future__ import annotations

from collections.abc import Awaitable, Callable, Iterable
from datetime import datetime, timedelta
import json
from pathlib import PurePath
from hashlib import sha256
from secrets import compare_digest, token_urlsafe
from typing import Any, cast
from uuid import uuid4

from moirai.domain.control_plane import (
    AuthenticationError,
    JobOffer,
    OfferError,
    RegistrationError,
    ScheduledJob,
)
from moirai.domain.leases import EventSequenceError, StaleLeaseError
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
from moirai.persistence.authentication import AsyncpgAuthentication
from moirai.workflows.runner_events import validate_runner_event, workflow_transition_for_terminal_event
from moirai.workflows.task_packets import (
    ExecutionRole,
    build_task_packet,
    planner_task_execution,
    task_execution,
)


class AsyncpgControlPlane:
    def __init__(self, pool: Any) -> None:
        self._pool = pool
        self._authentication = AsyncpgAuthentication(pool)

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

    async def login(self, username: str, password: str, now: datetime) -> Any:
        return await self._authentication.login(username, password, now)

    async def validate_session(
        self, session_token: str, csrf_token: str | None, now: datetime, require_csrf: bool
    ) -> Any:
        return await self._authentication.validate_session(
            session_token, csrf_token, now, require_csrf
        )

    async def revoke_session(self, session_token: str, now: datetime) -> None:
        await self._authentication.revoke_session(session_token, now)

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
    ) -> dict[str, object]:
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
        project = {"id": str(record["id"]), "name": str(record["name"]), "enabled": bool(record["enabled"])}
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.create", "project", str(project["id"]), "succeeded", now)
        return project

    async def list_projects(self) -> list[dict[str, object]]:
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
              AND wr.status NOT IN ('completed', 'blocked', 'failed', 'cancelled')
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
    ) -> dict[str, object]:
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
        project = {"id": str(record["id"]), "name": str(record["name"]), "enabled": bool(record["enabled"])}
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.update", "project", str(project["id"]), "succeeded", now)
        return project

    async def set_project_enabled(
        self, project_id: str, enabled: bool, now: datetime, actor_user_id: str | None = None
    ) -> dict[str, object]:
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
        project = {"id": str(record["id"]), "name": str(record["name"]), "enabled": bool(record["enabled"])}
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.enable" if enabled else "project.disable", "project", str(project["id"]), "succeeded", now)
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

    async def list_registration_tokens(self) -> list[dict[str, object]]:
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
    ) -> dict[str, object]:
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
        self, token: str, name: str, labels: Iterable[str], now: datetime
    ) -> tuple[Runner, str]:
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
                        (id, name, enabled, draining, status, version, labels, capabilities, registered_at)
                    VALUES ($1, $2, true, false, 'offline', '1.0', $3::jsonb, '{}'::jsonb, $4)
                    """,
                    runner_id,
                    name,
                    _json(sorted(labels_set)),
                    now,
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
        return Runner(str(runner_id), labels_set, False, True, False, False), credential

    async def authenticate_runner(self, runner_id: str, credential: str, now: datetime) -> Runner:
        runner = await self._load_runner_credential(runner_id, now)
        if not compare_digest(runner[1], self._hash(credential)):
            raise AuthenticationError("runner credential is invalid")
        return runner[0]

    async def heartbeat(self, runner_id: str, credential: str, now: datetime) -> Runner:
        runner = await self.authenticate_runner(runner_id, credential, now)
        updated = await self._pool.fetchrow(
            """
            UPDATE app.runners
            SET status = 'online', last_seen_at = $2
            WHERE id = $1 AND enabled = true AND revoked_at IS NULL
            RETURNING id, labels, enabled, draining
            """,
            _uuid(runner_id),
            now,
        )
        if updated is None:
            raise AuthenticationError("runner is inactive")
        return _runner(updated, connected=True, healthy=True)

    async def list_runners(self) -> list[dict[str, object]]:
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
        values = {
            "job_id": str(record["job_id"]),
            "project_id": str(record["project_id"]),
            "issue_external_id": str(record["external_id"]),
            "issue_title": str(record["title"]),
            "issue_body": str(record["body"]),
            "repository_mode": str(record["repository_mode"]),
            "repository_url": _optional_text(record["repository_url"]),
            "local_repository_path": _optional_text(record["local_repository_path"]),
            "default_branch": str(record["default_branch"]),
        }
        request_id = record.get("execution_request_id")
        role = record.get("execution_role")
        if request_id is None or role is None:
            return build_task_packet(planner_task_execution(**values))
        if role not in {"planner", "developer", "reviewer", "repairer"}:
            raise ValueError("workflow execution request role is invalid")
        return build_task_packet(
            task_execution(
                execution_id=f"{request_id}-{role_to_suffix(str(role))}",
                role=cast(ExecutionRole, str(role)),
                **values,
            )
        )

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
                    SELECT i.id AS issue_id, i.project_id, i.external_id, i.priority,
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
                          SELECT 1 FROM app.jobs AS active_job
                          WHERE active_job.runner_id = r.id
                            AND active_job.status IN ('offered', 'preparing', 'running')
                      )
                    ORDER BY i.priority DESC, i.external_created_at, i.last_synced_at,
                             i.project_id, i.external_id, r.id
                    FOR UPDATE OF i, p, r SKIP LOCKED
                    LIMIT 1
                    """
                )
                if candidate is None:
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
                      AND NOT EXISTS (
                          SELECT 1 FROM app.jobs AS active_job
                          WHERE active_job.runner_id = r.id
                            AND active_job.status IN ('offered', 'preparing', 'running')
                      )
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
                      AND NOT EXISTS (
                          SELECT 1 FROM app.jobs AS active_job
                          WHERE active_job.runner_id = r.id
                            AND active_job.status IN ('offered', 'preparing', 'running')
                      )
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
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                job = await connection.fetchrow(
                    """
                    SELECT j.id, j.workflow_run_id, j.lease_generation, j.lease_expires_at,
                           j.last_event_sequence, w.status AS workflow_status
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

                current_workflow_status = str(job["workflow_status"])
                transition = workflow_transition_for_terminal_event(summary, current_workflow_status)

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

                execution_type = _execution_type_from_id(event.execution_id)
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
                            event.event_sequence,
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
                              AND status = 'running'
                            """,
                            _uuid(event.job_id),
                            execution_type,
                            summary.event_type,
                            now,
                            summary.exit_code,
                            result_json,
                            event.lease_generation,
                        )

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

        if transition is not None and on_transition is not None:
            await on_transition(str(job["workflow_run_id"]), transition.new_status, transition.state_updates)

        updated_sequence = max(int(job["last_event_sequence"]), event.event_sequence)
        return JobLease(event.job_id, event.runner_id, int(job["lease_generation"]), job["lease_expires_at"], updated_sequence)

    async def _load_runner_credential(self, runner_id: str, now: datetime) -> tuple[Runner, str]:
        record = await self._pool.fetchrow(
            """
            SELECT r.id, r.labels, r.enabled, r.draining, r.status, c.credential_hash
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


def _registration_token(record: Any) -> dict[str, object]:
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


def _runner(record: Any, *, connected: bool | None = None, healthy: bool | None = None) -> Runner:
    status = str(record.get("status", "offline"))
    return Runner(
        str(record["id"]),
        frozenset(_labels(record["labels"])),
        status == "online" if connected is None else connected,
        bool(record["enabled"]),
        bool(record["draining"]),
        status == "online" if healthy is None else healthy,
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


def role_to_suffix(role: str) -> str:
    suffixes = {
        "planner": "plan",
        "developer": "implement",
        "reviewer": "review",
        "repairer": "repair",
    }
    try:
        return suffixes[role]
    except KeyError as error:
        raise ValueError("workflow execution request role is invalid") from error


def _execution_type_from_id(execution_id: str) -> str | None:
    suffix_to_type = {
        "-plan": "run_planner",
        "-implement": "run_developer",
        "-review": "run_reviewer",
        "-repair": "run_repair",
        "-pipeline": "run_local_pipeline",
    }
    for suffix, execution_type in suffix_to_type.items():
        if execution_id.endswith(suffix):
            return execution_type
    return None
