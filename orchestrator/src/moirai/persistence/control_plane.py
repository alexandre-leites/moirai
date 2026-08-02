from __future__ import annotations

import json
import logging
import re
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
from moirai.domain.credentials import (
    CREDENTIAL_DELIVERY,
    CREDENTIAL_KIND_BY_ENVIRONMENT_NAME,
    VALID_CREDENTIAL_KINDS,
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
    AccountProfile,
    AsyncpgAuthentication,
    AuthenticatedSession,
    SessionCredentials,
)
from moirai.persistence.circuits import reap_orphaned_probes, reopen_probe_circuits
from moirai.persistence.secrets import SealedSecret, SecretCipher, SecretCipherError

if TYPE_CHECKING:
    # Deferred: grpc/protocol.py imports this module's classes back for a
    # Protocol-conformance check, so this side must not import it at runtime.
    from moirai.grpc.protocol import (
        IssueSyncStatusRecord,
        ProjectRecord,
        QueueEntryRecord,
        RegistrationTokenRecord,
        RunnerRecord,
        WorkflowDetailRecord,
        WorkflowEventRecord,
    )
from moirai.workflows.runner_events import (
    WorkflowTransition,
    role_to_suffix,
    validate_runner_event,
    workflow_transition_for_terminal_event,
)
from moirai.workflows.schema_validation import SchemaNotFoundError, load_schema, validate
from moirai.workflows.task_packets import (
    ExecutionRole,
    PipelineCommand,
    build_task_packet,
    pipeline_task_execution,
    planner_task_execution,
    task_execution,
)

_LOGGER = logging.getLogger(__name__)

# Number of identical terminal outcomes that blocks a workflow. README's
# "Workflow recovery guarantees" documents four; `non_progress_attempts`
# counts *repeats*, so it is 0 for the first outcome of a run and N-1 once
# N identical outcomes have been observed.
NON_PROGRESS_OUTCOME_LIMIT = 4
_CONTEXT_ENTRY_LIMIT = 64
_CONTEXT_ENTRY_BYTES = 8 * 1024
_CONTEXT_TAIL_LINES = 50
_ISSUE_CHECKLIST_ITEM = re.compile(r"^\s*[-*+]\s+\[[ xX]\]\s+(.+?)\s*$", re.MULTILINE)

# Workflow-run statuses that mean the run is over. A terminal run never has
# work in flight, so nothing may resurrect it.
_TERMINAL_WORKFLOW_STATUSES = frozenset({"completed", "blocked", "failed", "cancelled"})

# Execution-request status meaning "nothing will ever execute or report on this
# row". It is the signal recover_stalled_workflow_run reads to tell a lost
# execution (re-run the phase) from a delivered one (advance the graph).
_ORPHANED_REQUEST_STATUS = "orphaned"

# How many times one phase's execution may be lost and re-queued before the
# maintenance loop stops replacing it. A re-queue deliberately spends no retry
# budget (the attempt was charged when the node dispatched it), so without this
# nothing would bound the number of agent executions a repeatedly-failing
# environment could buy.
_LOST_EXECUTION_REQUEUE_LIMIT = 5

# How long a drainer may hold a workflow_transition_outbox row in `processing`
# before another drainer takes it back. `processing` is committed before the
# delivery starts, so a crashed drainer cannot release its own row and the
# transition would otherwise be dropped forever (issue #96). Comfortably longer
# than a real delivery, which invokes the graph and can reach GitHub.
#
# It is deliberately shorter than the 2-minute window after which a run counts
# as stalled (main._WORKFLOW_STALL_AFTER), so that on a maintenance loop ticking
# at its normal 30s the drain usually reclaims a stranded row before the
# stalled-run arm reaches the same run -- which matters because that arm
# recovers a run *without* the state updates the outbox row carries. It is a
# tendency, not a guarantee: a tick is unbounded (arm 3 can invoke the graph for
# up to _WORKFLOW_STALL_BATCH runs), and nothing orders the two arms against
# each other. Both outcomes are correct; the drain's is merely cheaper.
_OUTBOX_PROCESSING_LEASE = timedelta(seconds=90)

# Result-document keys that identify a single attempt rather than its content.
# Both are required or emitted per execution, so they must never take part in
# an outcome identity.
_VOLATILE_RESULT_KEYS = frozenset({"executionId", "sessionId"})

# Kept byte-identical to the runner's dispatch.FailureFingerprint, including
# the ordering, which is load-bearing: markers truncate the message in place
# and the first matching category wins.
_FINGERPRINT_SECRET_MARKERS = ("token", "secret", "credential", "password", "authorization")
_FINGERPRINT_CATEGORIES = (
    "workspace",
    "repository",
    "git",
    "pipeline",
    "agent",
    "executor",
    "disk",
    "environment",
)
_FINGERPRINT_MESSAGE_LINES = 5


class _CircuitProbeUnavailable(Exception):
    """Unwinds a half-open probe claim so no part of it is committed.

    Raised and caught entirely inside `_claim_circuit_probes`; it exists to
    reach the enclosing savepoint, never to reach a caller.
    """


def _runner_conditions(runner: str) -> str:
    """SQL conditions a runner row must satisfy to serve a project.

    Shared by `AsyncpgControlPlane.schedule` and `.list_queue` so the two can
    never disagree on what "a runner can serve this project" means. `runner`
    is the SQL alias of the `app.runners` row; the conditions reference the
    `p` alias of `app.projects` for the required labels, so `p` must be in
    scope wherever the fragment is embedded.
    """
    return (
        f"{runner}.enabled = true"
        f" AND {runner}.draining = false"
        f" AND {runner}.revoked_at IS NULL"
        f" AND {runner}.labels @> COALESCE(p.configuration->'required_runner_labels', '[]'::jsonb)"
        f" AND ("
        f"    SELECT COUNT(*) FROM app.jobs AS active_job"
        f"    WHERE active_job.runner_id = {runner}.id"
        f"      AND active_job.status IN ('offered', 'preparing', 'running')"
        f" ) < {runner}.capacity"
    )


def _locked_condition(project: str) -> str:
    return (
        f"NOT EXISTS (SELECT 1 FROM app.project_locks AS lock WHERE lock.project_id = {project}.id)"
    )


def _project_circuit_condition(project: str, cooldown: str) -> str:
    return (
        f"NOT EXISTS (SELECT 1 FROM app.project_circuit_state AS circuit"
        f" WHERE circuit.project_id = {project}.id"
        f" AND (circuit.state = 'half_open' OR (circuit.state = 'open' AND circuit.opened_at > {cooldown})))"
    )


def _provider_circuit_condition(issue: str, cooldown: str) -> str:
    return (
        f"NOT EXISTS (SELECT 1 FROM app.provider_circuit_state AS circuit"
        f" WHERE circuit.provider = {issue}.provider"
        f" AND (circuit.state = 'half_open' OR (circuit.state = 'open' AND circuit.opened_at > {cooldown})))"
    )


def _scheduling_conditions(issue: str, project: str, cooldown: str) -> str:
    """The issue/project half of the scheduling predicate.

    `schedule()` ANDs this with `_runner_conditions`. `list_queue()` keeps the
    whole predicate under a CASE and negates each term to report why an issue
    is not currently scheduled, so the reader cannot drift from the writer.
    """
    return (
        f"{issue}.eligible = true"
        # `eligible` is a cached verdict from the last synchronisation; the
        # state is the fact. Without this, a closed issue whose eligible flag
        # had not yet been refreshed was schedulable -- and `list_queue` filters
        # on state separately, so it would have been scheduled without ever
        # having appeared in the queue an operator can see.
        f" AND {issue}.state = 'open'"
        f" AND {project}.enabled = true"
        f" AND {_locked_condition(project)}"
        f" AND {_project_circuit_condition(project, cooldown)}"
        f" AND {_provider_circuit_condition(issue, cooldown)}"
    )


def _blocked_reason_case(issue: str, project: str, cooldown: str) -> str:
    """The first scheduling condition an issue fails, or '' when it passes all.

    Ordering is deliberate: `project_disabled` is the root cause and is
    reported first, then transient blockers in the order an operator would
    clear them (lock, project circuit, provider circuit), then runner
    availability. Every `WHEN NOT (...) ` term is the textual negation of the
    exact condition `_scheduling_conditions`/`_runner_conditions` generate.
    """
    return (
        f"CASE"
        f" WHEN NOT {project}.enabled THEN 'project_disabled'"
        f" WHEN NOT ({_locked_condition(project)}) THEN 'project_locked'"
        f" WHEN NOT ({_project_circuit_condition(project, cooldown)}) THEN 'project_circuit_open'"
        f" WHEN NOT ({_provider_circuit_condition(issue, cooldown)}) THEN 'provider_circuit_open'"
        f" WHEN NOT EXISTS ("
        f"    SELECT 1 FROM app.runners AS runner"
        f"    WHERE runner.status = 'online' AND {_runner_conditions('runner')}"
        f" ) THEN 'no_matching_runner'"
        f" ELSE ''"
        f" END"
    )


class AsyncpgControlPlane:
    def __init__(
        self,
        pool: Any,
        circuit_probe_cooldown: timedelta = timedelta(minutes=5),
        unanswered_offer_limit: int = 5,
        unanswered_offer_grace: timedelta = timedelta(minutes=15),
        secret_cipher: SecretCipher | None = None,
    ) -> None:
        if circuit_probe_cooldown <= timedelta():
            raise ValueError("circuit probe cooldown must be positive")
        if unanswered_offer_limit < 1:
            raise ValueError("unanswered offer limit must be positive")
        if unanswered_offer_grace < timedelta():
            raise ValueError("unanswered offer grace must not be negative")
        self._pool = pool
        self._authentication = AsyncpgAuthentication(pool)
        self._circuit_probe_cooldown = circuit_probe_cooldown
        self._unanswered_offer_limit = unanswered_offer_limit
        self._unanswered_offer_grace = unanswered_offer_grace
        self._secret_cipher = secret_cipher

    @property
    def pool(self) -> Any:
        """The underlying connection pool, for callers that need durability-specific setup (migrations, workflow runtime, scheduling)."""
        return self._pool

    @classmethod
    async def connect(
        cls, database_url: str, secret_cipher: SecretCipher | None = None
    ) -> AsyncpgControlPlane:
        try:
            import asyncpg
        except ModuleNotFoundError as error:
            raise RuntimeError("asyncpg is required to run the orchestrator") from error
        pg_dsn = database_url.replace("+asyncpg", "")
        return cls(
            await asyncpg.create_pool(dsn=pg_dsn, min_size=1, max_size=10),
            secret_cipher=secret_cipher,
        )

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
                (SELECT COUNT(*)
                 FROM app.jobs
                 WHERE status IN ('offered', 'preparing', 'running')) AS scheduled_jobs,
                (SELECT EXTRACT(EPOCH FROM ($1::timestamptz - MIN(last_seen_at)))
                 FROM app.runners
                 WHERE enabled = true AND revoked_at IS NULL AND last_seen_at IS NOT NULL) AS runner_heartbeat_age
            """,
            now,
        )
        if record is None:
            return {"queue_depth": 0, "active_workflows": 0, "scheduled_jobs": 0, "runner_heartbeat_age": 0}
        return {
            "queue_depth": float(record["queue_depth"] or 0),
            "active_workflows": float(record["active_workflows"] or 0),
            "scheduled_jobs": float(record["scheduled_jobs"] or 0),
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

    async def update_account(
        self,
        user_id: str,
        keep_session_id: str,
        current_password: str,
        new_password: str,
        new_email: str,
        display_name: str,
        now: datetime,
    ) -> AccountProfile:
        return await self._authentication.update_account(
            user_id,
            keep_session_id,
            current_password,
            new_password,
            new_email,
            display_name,
            now,
        )

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
        pipeline_steps: Iterable[dict[str, object]] = (),
    ) -> ProjectRecord:
        normalized = _project_configuration(
            name,
            repository_mode,
            repository_url,
            local_repository_path,
            default_branch,
            required_runner_labels,
        )
        steps = _pipeline_steps(pipeline_steps)
        project_id = uuid4()
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                record = await connection.fetchrow(
                    """
                    INSERT INTO app.projects
                        (id, name, enabled, repository_mode, repository_url, local_repository_path,
                         default_branch, configuration, created_at, updated_at)
                    VALUES ($1, $2, true, $3, $4, $5, $6, $7::jsonb, $8, $8)
                    RETURNING id, name, enabled, repository_mode, repository_url,
                              local_repository_path, default_branch, configuration
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
                await _replace_pipeline_steps(connection, project_id, steps)
        project = _project_record(record)
        project["pipeline_steps"] = list(steps)
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.create", "project", project["id"], "succeeded", now)
        return project

    async def list_projects(self) -> list[ProjectRecord]:
        records = await self._pool.fetch(
            """
            SELECT p.id, p.name, p.enabled, p.repository_mode, p.repository_url,
                   p.local_repository_path, p.default_branch, p.configuration,
                   COALESCE((
                       SELECT jsonb_agg(jsonb_build_object(
                           'command', s.command,
                           'timeout_seconds', s.timeout_seconds,
                           'position', s.position,
                           'required', s.required
                       ) ORDER BY s.position, s.id)
                       FROM app.project_pipeline_steps AS s
                       WHERE s.project_id = p.id
                   ), '[]'::jsonb) AS pipeline_steps
            FROM app.projects AS p
            ORDER BY p.name ASC, p.id ASC
            """
        )
        return [_project_record(record) for record in records]

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
        # GitHub reports "OPEN"; every read in this file compares against
        # 'open'. Normalising here rather than at each of those comparisons
        # keeps one canonical spelling in the column, and matches what
        # domain.issues.is_eligible already does with the same value.
        state = state.strip().lower()
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

    async def list_latest_workflow_runs_for_project(self, project_id: str) -> list[dict[str, object]]:
        """Return the newest workflow run per issue, ordered by issue.

        Label reconciliation needs exactly one authoritative run per issue.
        Returning every historical run made the terminal label depend on row
        order, so a stale `blocked` run could overwrite `agent:delivered`.
        """
        records = await self._pool.fetch(
            """
            SELECT DISTINCT ON (wr.issue_id)
                   wr.id, wr.status, wr.created_at, i.external_id, i.id AS issue_id
            FROM app.workflow_runs wr
            JOIN app.issues i ON i.id = wr.issue_id
            WHERE wr.project_id = $1
            ORDER BY wr.issue_id ASC, wr.created_at DESC, wr.id DESC
            """,
            _uuid(project_id),
        )
        return [
            {
                "id": str(record["id"]),
                "status": str(record["status"]),
                "created_at": record["created_at"],
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
        # This is the single "sync succeeded" write path: it clears the failure
        # state and records when the project last synced, which the web console
        # surfaces so an operator can see that issues are actually being picked
        # up.
        await self._pool.execute(
            """
            INSERT INTO app.issue_sync_state
                (project_id, consecutive_failures, next_retry_at, last_error, last_synced_at, updated_at)
            VALUES ($1, 0, NULL, NULL, $2, $2)
            ON CONFLICT (project_id) DO UPDATE
            SET consecutive_failures = 0, next_retry_at = NULL, last_error = NULL,
                last_synced_at = EXCLUDED.last_synced_at, updated_at = EXCLUDED.updated_at
            """,
            _uuid(project_id),
            now,
        )

    async def issue_sync_status(self, now: datetime) -> list[IssueSyncStatusRecord]:
        """Return per-project issue-sync status for the web console.

        Every project appears, whether or not it has synced yet: a fresh
        project reads as "never synced, zero issues", which is exactly the
        state an operator needs to see before the first pass runs.
        """
        records = await self._pool.fetch(
            """
            SELECT p.id AS project_id, p.name AS project_name, p.enabled,
                   COUNT(i.id) AS issue_count,
                   COUNT(i.id) FILTER (WHERE i.eligible = true AND i.state = 'open')
                       AS eligible_count,
                   s.last_synced_at, s.consecutive_failures, s.next_retry_at, s.last_error,
                   (s.next_retry_at IS NOT NULL AND s.next_retry_at > $1) AS backing_off
            FROM app.projects AS p
            LEFT JOIN app.issues AS i ON i.project_id = p.id AND i.state = 'open'
            LEFT JOIN app.issue_sync_state AS s ON s.project_id = p.id
            GROUP BY p.id, p.name, p.enabled, s.last_synced_at, s.consecutive_failures,
                     s.next_retry_at, s.last_error
            ORDER BY p.name
            """,
            now,
        )
        return [
            {
                "project_id": str(record["project_id"]),
                "project_name": str(record["project_name"]),
                "enabled": bool(record["enabled"]),
                "issue_count": int(record["issue_count"]),
                "eligible_count": int(record["eligible_count"]),
                "last_synced_at": record["last_synced_at"],
                # LEFT JOIN: a project that has never synchronised has no
                # issue_sync_state row, so this is NULL rather than 0. int(None)
                # raised, which took down the whole sync-status view -- the one
                # place an operator would look to find out why a project they
                # had just added was not being picked up.
                "consecutive_failures": int(record["consecutive_failures"] or 0),
                "next_retry_at": record["next_retry_at"],
                "last_error": record["last_error"],
                "backing_off": bool(record["backing_off"]),
            }
            for record in records
        ]

    async def record_provider_failure(self, provider: str, reason: str, now: datetime) -> None:
        """Records a provider failure and drops any probe pointer (issue #92).

        A fresh failure supersedes whatever an in-flight probe was going to
        report, and a pointer left behind on a non-half-open row is stale by
        definition. Keeping one used to make the circuit permanently
        unclaimable. A failure recorded against a half-open circuit is the
        probe's verdict, so it reopens with a fresh cooldown.
        """
        if not provider.strip():
            raise ValueError("provider is required")
        await self._pool.execute(
            """
            INSERT INTO app.provider_circuit_state
                (provider, state, consecutive_failures, last_failure_reason, opened_at,
                 probe_workflow_run_id, updated_at)
            VALUES ($1, 'closed', 1, $2, NULL, NULL, $3)
            ON CONFLICT (provider) DO UPDATE
            SET consecutive_failures = app.provider_circuit_state.consecutive_failures + 1,
                state = CASE
                    WHEN app.provider_circuit_state.state = 'half_open' THEN 'open'
                    WHEN app.provider_circuit_state.consecutive_failures >= 2 THEN 'open' ELSE 'closed' END,
                opened_at = CASE
                    WHEN app.provider_circuit_state.state = 'half_open' THEN EXCLUDED.updated_at
                    WHEN app.provider_circuit_state.consecutive_failures >= 2 THEN EXCLUDED.updated_at ELSE NULL END,
                probe_workflow_run_id = NULL,
                last_failure_reason = EXCLUDED.last_failure_reason,
                updated_at = EXCLUDED.updated_at
            """,
            provider.strip(),
            reason[:1024],
            now,
        )

    async def clear_provider_failure(self, provider: str, now: datetime) -> None:
        """Closes the provider circuit and drops its probe pointer (issue #92).

        Leaving the pointer set let the probe workflow's own terminal event
        reopen a circuit that had already been closed on real evidence.
        """
        await self._pool.execute(
            """
            INSERT INTO app.provider_circuit_state
                (provider, state, consecutive_failures, last_failure_reason, opened_at,
                 probe_workflow_run_id, updated_at)
            VALUES ($1, 'closed', 0, NULL, NULL, NULL, $2)
            ON CONFLICT (provider) DO UPDATE
            SET state = 'closed', consecutive_failures = 0, last_failure_reason = NULL,
                opened_at = NULL, probe_workflow_run_id = NULL, updated_at = EXCLUDED.updated_at
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
        pipeline_steps: Iterable[dict[str, object]] = (),
    ) -> ProjectRecord:
        normalized = _project_configuration(
            name,
            repository_mode,
            repository_url,
            local_repository_path,
            default_branch,
            required_runner_labels,
        )
        steps = _pipeline_steps(pipeline_steps)
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                record = await connection.fetchrow(
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
                    RETURNING id, name, enabled, repository_mode, repository_url,
                              local_repository_path, default_branch, configuration
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
                await _replace_pipeline_steps(connection, _uuid(project_id), steps)
        project = _project_record(record)
        project["pipeline_steps"] = list(steps)
        if actor_user_id is not None:
            await self.append_audit(actor_user_id, "project.update", "project", project["id"], "succeeded", now)
        return project

    async def set_project_enabled(
        self, project_id: str, enabled: bool, now: datetime, actor_user_id: str | None = None
    ) -> ProjectRecord:
        record = await self._pool.fetchrow(
            """
            UPDATE app.projects SET enabled = $2, updated_at = $3 WHERE id = $1
            RETURNING id, name, enabled, repository_mode, repository_url,
                      local_repository_path, default_branch, configuration,
                      COALESCE((
                          SELECT jsonb_agg(jsonb_build_object(
                              'command', s.command,
                              'timeout_seconds', s.timeout_seconds,
                              'position', s.position,
                              'required', s.required
                          ) ORDER BY s.position, s.id)
                          FROM app.project_pipeline_steps AS s
                          WHERE s.project_id = app.projects.id
                      ), '[]'::jsonb) AS pipeline_steps
            """,
            _uuid(project_id),
            enabled,
            now,
        )
        if record is None:
            raise ValueError("project is unknown")
        project = _project_record(record)
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

    # --- Per-project credentials ------------------------------------------

    def _cipher(self) -> SecretCipher:
        if self._secret_cipher is None:
            raise SecretCipherError(
                "no secret key is configured, so per-project credentials cannot be "
                "stored or read; set LOOP_SECRET_KEY (or LOOP_SECRET_KEY_FILE) to a "
                "32-byte key, for example from `openssl rand -base64 32`"
            )
        return self._secret_cipher

    async def set_project_credential(
        self,
        project_id: str,
        kind: str,
        value: str,
        actor_user_id: str | None,
        now: datetime,
    ) -> None:
        """Seals a credential and replaces whatever that project had for `kind`."""
        if kind not in VALID_CREDENTIAL_KINDS:
            raise ValueError(f"unknown credential kind: {kind}")
        sealed = self._cipher().seal(value)
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                # Same "project is unknown" as update_project, so the servicer
                # reports it the same way. Without this the foreign key still
                # rejects the write, but as a database error the caller sees as
                # "service unavailable" rather than as a bad project id.
                if not await connection.fetchval(
                    "SELECT true FROM app.projects WHERE id = $1", _uuid(project_id)
                ):
                    raise ValueError("project is unknown")
                await connection.execute(
                    """
                    INSERT INTO app.project_credentials
                        (project_id, kind, ciphertext, nonce, created_at, updated_at)
                    VALUES ($1, $2, $3, $4, $5, $5)
                    ON CONFLICT (project_id, kind) DO UPDATE
                    SET ciphertext = EXCLUDED.ciphertext,
                        nonce = EXCLUDED.nonce,
                        updated_at = EXCLUDED.updated_at
                    """,
                    _uuid(project_id),
                    kind,
                    sealed.ciphertext,
                    sealed.nonce,
                    now,
                )
                # The value is never audited, only that it changed and to which
                # kind -- the resource id carries the project and the kind.
                await AsyncpgAuthentication._append_audit(
                    connection,
                    actor_user_id=_uuid_or_none(actor_user_id),
                    action="project.credential.set",
                    resource_type="project_credential",
                    resource_id=f"{project_id}/{kind}",
                    outcome="succeeded",
                    now=now,
                )

    async def clear_project_credential(
        self, project_id: str, kind: str, actor_user_id: str | None, now: datetime
    ) -> bool:
        if kind not in VALID_CREDENTIAL_KINDS:
            raise ValueError(f"unknown credential kind: {kind}")
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                status = await connection.execute(
                    "DELETE FROM app.project_credentials WHERE project_id = $1 AND kind = $2",
                    _uuid(project_id),
                    kind,
                )
                removed = status != "DELETE 0"
                if removed:
                    await AsyncpgAuthentication._append_audit(
                        connection,
                        actor_user_id=_uuid_or_none(actor_user_id),
                        action="project.credential.cleared",
                        resource_type="project_credential",
                        resource_id=f"{project_id}/{kind}",
                        outcome="succeeded",
                        now=now,
                    )
                return removed

    async def resolve_job_secret(
        self, runner_id: str, job_id: str, generation: int, name: str, now: datetime
    ) -> tuple[str, str] | None:
        """One secret for a job a runner currently holds, or None if unconfigured.

        Returns `(value, delivery)`. Fenced on exactly the conditions
        `renew_lease` uses -- this runner, this job, this generation, an
        unexpired lease and a live status -- so a runner whose work was taken
        away by recovery cannot go on resolving secrets for it. A request that
        fails the fence raises `StaleLeaseError` rather than returning None:
        "you no longer hold this job" and "that project has no such credential"
        are different answers and must not look alike to the caller.
        """
        kind = CREDENTIAL_KIND_BY_ENVIRONMENT_NAME.get(name)
        if kind is None:
            raise ValueError(f"no project credential backs the environment reference {name!r}")
        job = await self._pool.fetchrow(
            """
            SELECT project_id
            FROM app.jobs
            WHERE id = $1 AND runner_id = $2 AND status IN ('preparing', 'running')
              AND lease_generation = $3 AND lease_expires_at > $4
            """,
            _uuid(job_id),
            _uuid(runner_id),
            generation,
            now,
        )
        if job is None:
            raise StaleLeaseError("runner does not hold this job at this lease generation")
        value = await self.project_credential(str(job["project_id"]), kind)
        if value is None:
            return None
        return value, CREDENTIAL_DELIVERY[kind]

    async def describe_project_credentials(self, project_id: str) -> list[dict[str, object]]:
        """What is configured, never the values. This is the only read the API has."""
        records = await self._pool.fetch(
            """
            SELECT kind, created_at, updated_at
            FROM app.project_credentials
            WHERE project_id = $1
            ORDER BY kind
            """,
            _uuid(project_id),
        )
        return [
            {
                "kind": str(record["kind"]),
                "created_at": record["created_at"],
                "updated_at": record["updated_at"],
            }
            for record in records
        ]

    async def project_credential(self, project_id: str, kind: str) -> str | None:
        """Opens one credential for the orchestrator's own use.

        Returns None when the project has none, so callers fall back to the
        deployment-wide token. A stored value that cannot be opened is an error
        rather than a None: silently falling back would use the wrong identity
        against the code host and look like a permissions problem.
        """
        if kind not in VALID_CREDENTIAL_KINDS:
            raise ValueError(f"unknown credential kind: {kind}")
        record = await self._pool.fetchrow(
            "SELECT ciphertext, nonce FROM app.project_credentials WHERE project_id = $1 AND kind = $2",
            _uuid(project_id),
            kind,
        )
        if record is None:
            return None
        return self._cipher().open(
            SealedSecret(ciphertext=bytes(record["ciphertext"]), nonce=bytes(record["nonce"]))
        )

    async def list_workflows(self) -> list[WorkflowDetailRecord]:
        # Same projection as get_workflow below: the management console renders
        # issue titles, pull requests and attempt budgets straight from the list,
        # and fetching them per row would be one request per workflow.
        records = await self._pool.fetch(
            """
            SELECT wr.id, wr.project_id, wr.status, wr.current_phase, i.external_id AS issue_external_id,
                   i.title AS issue_title, wr.branch_name,
                   COALESCE(pr.external_id, wr.pull_request_external_id) AS pull_request_external_id,
                   COALESCE(pr.url, wr.pull_request_url) AS pull_request_url, pr.state AS pull_request_state,
                   wr.blocking_reason, wr.planning_attempts, wr.implementation_attempts,
                   wr.pipeline_repair_attempts, wr.review_cycles, wr.ci_repair_attempts,
                   wr.total_agent_executions, wr.created_at, wr.updated_at
            FROM app.workflow_runs AS wr
            JOIN app.issues AS i ON i.id = wr.issue_id
            LEFT JOIN app.pull_requests AS pr ON pr.workflow_run_id = wr.id
            ORDER BY wr.created_at DESC, wr.id ASC
            """
        )
        return [_workflow_detail_record(record) for record in records]

    async def get_workflow(self, workflow_run_id: str) -> WorkflowDetailRecord | None:
        record = await self._pool.fetchrow(
            """
            SELECT wr.id, wr.project_id, wr.status, wr.current_phase, i.external_id AS issue_external_id,
                   i.title AS issue_title, wr.branch_name,
                   COALESCE(pr.external_id, wr.pull_request_external_id) AS pull_request_external_id,
                   COALESCE(pr.url, wr.pull_request_url) AS pull_request_url, pr.state AS pull_request_state,
                   wr.blocking_reason, wr.planning_attempts, wr.implementation_attempts,
                   wr.pipeline_repair_attempts, wr.review_cycles, wr.ci_repair_attempts,
                   wr.total_agent_executions, wr.created_at, wr.updated_at
            FROM app.workflow_runs AS wr
            JOIN app.issues AS i ON i.id = wr.issue_id
            LEFT JOIN app.pull_requests AS pr ON pr.workflow_run_id = wr.id
            WHERE wr.id = $1
            """,
            _uuid(workflow_run_id),
        )
        if record is None:
            return None
        return _workflow_detail_record(record)

    async def list_workflow_events(
        self, workflow_run_id: str, after_id: int, limit: int
    ) -> list[WorkflowEventRecord]:
        records = await self._pool.fetch(
            """
            SELECT id, event_type, payload, created_at
            FROM app.workflow_events
            WHERE workflow_run_id = $1 AND ($2 = 0 OR id < $2)
            ORDER BY id DESC
            LIMIT $3
            """,
            _uuid(workflow_run_id),
            after_id,
            limit,
        )
        return [
            {
                "id": str(record["id"]),
                "event_type": str(record["event_type"]),
                "payload_json": (
                    record["payload"]
                    if isinstance(record["payload"], str)
                    else json.dumps(record["payload"], separators=(",", ":"), sort_keys=True)
                ),
                "created_at": record["created_at"],
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
                    """UPDATE app.runners SET enabled = $2, operator_draining = $3, draining = $3,
                       revoked_at = COALESCE(revoked_at, $4), status = CASE WHEN $1 = 'revoke' THEN 'offline' ELSE status END
                       WHERE id = $5 AND ($1 != 'enable' OR revoked_at IS NULL)
                       RETURNING id, name, enabled, draining, status, labels, last_seen_at""",
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

    async def set_runner_draining(self, runner_id: str, draining: bool) -> None:
        updated = await self._pool.execute(
            """UPDATE app.runners
               SET draining = operator_draining OR $2
               WHERE id = $1 AND revoked_at IS NULL""",
            _uuid(runner_id),
            draining,
        )
        if updated != "UPDATE 1":
            raise ValueError("runner is unknown or revoked")

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
                    SELECT id, project_id, status, human_resume_phase
                    FROM app.workflow_runs
                    WHERE id = $1 AND status = 'waiting_human'
                    FOR UPDATE
                    """,
                    _uuid(workflow_run_id),
                )
                if workflow is None:
                    raise ValueError("workflow run is not awaiting human approval")
                guidance = (comment or "").strip()
                if workflow.get("human_resume_phase") is not None and not guidance:
                    raise ValueError("human guidance is required")
                if workflow.get("human_resume_phase") is not None:
                    await connection.execute(
                        "UPDATE app.workflow_runs SET human_guidance = $2, updated_at = $3 WHERE id = $1",
                        _uuid(workflow_run_id), guidance, now,
                    )
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

    async def retry_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        return await self._control_workflow("retry", workflow_run_id, reason, actor_user_id, now)

    async def cancel_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        return await self._control_workflow("cancel", workflow_run_id, reason, actor_user_id, now)

    async def block_workflow(
        self, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        return await self._control_workflow("block", workflow_run_id, reason, actor_user_id, now)

    async def _control_workflow(
        self, action: str, workflow_run_id: str, reason: str | None, actor_user_id: str | None, now: datetime
    ) -> dict[str, object]:
        if action not in {"retry", "cancel", "block"} or actor_user_id is None:
            raise ValueError("workflow control request is invalid")
        reason = (reason or "").strip()
        if len(reason) > 1024 or (action == "block" and not reason):
            raise ValueError("workflow control reason is invalid")
        if not reason:
            reason = f"operator {action}"
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                workflow = await connection.fetchrow(
                    """
                    SELECT id, project_id, status, current_phase, blocking_reason
                    FROM app.workflow_runs WHERE id = $1 FOR UPDATE
                    """,
                    _uuid(workflow_run_id),
                )
                if workflow is None:
                    raise ValueError("workflow run is unknown")
                status = str(workflow["status"])
                idempotent = (action == "retry" and status == "recovering") or (
                    action == "cancel" and status == "cancelled"
                ) or (action == "block" and status == "blocked")
                if not idempotent:
                    if action == "retry":
                        if status not in {"blocked", "failed", "cancelled"}:
                            raise ValueError("workflow run is not retryable")
                        acquired = await connection.fetchrow(
                            """
                            INSERT INTO app.project_locks (project_id, workflow_run_id, acquired_at, updated_at)
                            VALUES ($1, $2, $3, $3)
                            ON CONFLICT (project_id) DO NOTHING
                            RETURNING workflow_run_id
                            """,
                            workflow["project_id"], workflow["id"], now,
                        )
                        if acquired is None:
                            raise ValueError("project already has an active workflow")
                        await connection.execute(
                            """
                            UPDATE app.workflow_runs
                            SET status = 'recovering', current_phase = 'recovering', completed_at = NULL,
                                updated_at = $2
                            WHERE id = $1
                            """,
                            workflow["id"], now,
                        )
                        status = "recovering"
                    else:
                        if status in _TERMINAL_WORKFLOW_STATUSES:
                            raise ValueError("workflow run is already terminal")
                        terminal_status = "cancelled" if action == "cancel" else "blocked"
                        await connection.execute(
                            """
                            UPDATE app.workflow_runs
                            SET status = $2, current_phase = $2, blocking_reason = CASE WHEN $2 = 'blocked' THEN $3 ELSE blocking_reason END,
                                terminal_reason = $3, completed_at = COALESCE(completed_at, $4), updated_at = $4
                            WHERE id = $1
                            """,
                            workflow["id"], terminal_status, reason, now,
                        )
                        await connection.execute(
                            """
                            UPDATE app.jobs SET status = 'cancelled', finished_at = $2, recovery_reason = $3,
                                lease_generation = lease_generation + 1
                            WHERE workflow_run_id = $1 AND status IN ('offered', 'preparing', 'running', 'recovering')
                            """,
                            workflow["id"], now, reason,
                        )
                        await connection.execute(
                            """
                            UPDATE app.workflow_execution_requests SET status = 'cancelled'
                            WHERE workflow_run_id = $1 AND status IN ('queued', 'dispatched')
                            """,
                            workflow["id"],
                        )
                        await connection.execute(
                            "DELETE FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
                            workflow["project_id"], workflow["id"],
                        )
                        await reopen_probe_circuits(connection, workflow["id"], now)
                        status = terminal_status
                await AsyncpgAuthentication._append_audit(
                    connection,
                    actor_user_id=_uuid(actor_user_id),
                    action=f"workflow.{action}",
                    resource_type="workflow_run",
                    resource_id=workflow_run_id,
                    outcome="idempotent" if idempotent else "succeeded",
                    now=now,
                )
                cancellation = None
                if action in {"cancel", "block"} and not idempotent:
                    active = await connection.fetchrow(
                        """
                        SELECT j.runner_id, j.lease_generation - 1 AS lease_generation, request.id AS request_id, request.role
                        FROM app.jobs AS j
                        LEFT JOIN LATERAL (
                            SELECT id, role FROM app.workflow_execution_requests
                            WHERE workflow_run_id = j.workflow_run_id AND status = 'cancelled'
                            ORDER BY dispatched_at DESC NULLS LAST, id DESC LIMIT 1
                        ) AS request ON true
                        WHERE j.workflow_run_id = $1
                        ORDER BY j.finished_at DESC NULLS LAST, j.id DESC LIMIT 1
                        """,
                        workflow["id"],
                    )
                    if active is not None and active["runner_id"] is not None and active["request_id"] is not None:
                        cancellation = {
                            "runner_id": str(active["runner_id"]),
                            "lease_generation": int(active["lease_generation"]),
                            "execution_id": f"{active['request_id']}-{role_to_suffix(str(active['role']))}",
                        }
                return {
                    "id": str(workflow["id"]), "project_id": str(workflow["project_id"]),
                    "status": status, "phase": "recovering" if status == "recovering" else status,
                    "cancellation": cancellation,
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
                    w.last_gate_verdict, w.remaining_work, w.human_guidance,
                    planner.result AS planner_result, review.result AS review_result,
                    pipeline.result AS pipeline_result, latest.execution_type AS latest_execution_type,
                    latest.attempt AS latest_execution_attempt, latest.status AS latest_execution_status,
                    latest.exit_code AS latest_execution_exit_code, latest.result AS latest_execution_result,
                    request.id AS execution_request_id, request.role AS execution_role,
                    EXISTS (
                        SELECT 1 FROM app.workflow_execution_requests AS any_request
                        WHERE any_request.workflow_run_id = w.id
                    ) AS has_execution_history
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
            LEFT JOIN LATERAL (
                SELECT result
                FROM app.executions
                WHERE job_id = j.id AND execution_type = 'run_planner' AND status = 'completed'
                ORDER BY finished_at DESC NULLS LAST, id DESC
                LIMIT 1
            ) AS planner ON true
            LEFT JOIN LATERAL (
                SELECT result
                FROM app.ai_reviews
                WHERE workflow_run_id = w.id
                ORDER BY created_at DESC, id DESC
                LIMIT 1
            ) AS review ON true
            LEFT JOIN LATERAL (
                SELECT result
                FROM app.pipeline_runs
                WHERE workflow_run_id = w.id AND status = 'failed'
                ORDER BY finished_at DESC NULLS LAST, id DESC
                LIMIT 1
            ) AS pipeline ON true
            LEFT JOIN LATERAL (
                SELECT execution_type, attempt, status, exit_code, result
                FROM app.executions
                WHERE job_id = j.id AND status IN ('completed', 'failed', 'cancelled')
                ORDER BY finished_at DESC NULLS LAST, id DESC
                LIMIT 1
            ) AS latest ON true
            WHERE j.id = $1 AND j.status = 'offered'
            """,
            _uuid(scheduled.offer.job_id),
        )
        if record is None:
            raise ValueError("scheduled job is unavailable")
        if record.get("execution_request_id") is None and bool(record.get("has_execution_history")):
            # No dispatched request, but this run has queued executions before:
            # the planner fallback below is the *bootstrap* packet and would
            # send a run that is mid-implementation back to planning with an
            # execution ID (`{job_id}-plan`) that accept_event is guaranteed to
            # reject, aborting the runner's control stream on every retry.
            # Refuse instead. The scheduler skips a candidate whose packet fails
            # to build and lets its offer expire on its own TTL, which is
            # bounded by the unanswered-offer limit.
            raise ValueError("scheduled job has no dispatched execution request")
        job_id = str(record["job_id"])
        project_id = str(record["project_id"])
        issue_external_id = str(record["external_id"])
        issue_title = str(record["title"])
        issue_body = str(record["body"])
        repository_mode = str(record["repository_mode"])
        repository_url = _optional_text(record["repository_url"])
        local_repository_path = _optional_text(record["local_repository_path"])
        default_branch = str(record["default_branch"])
        planner_execution_result = _json_object(record.get("planner_result"))
        planner_result = _schema_result(planner_execution_result.get("result"), "planner-result")
        review_result = _json_object(record.get("review_result"))
        pipeline_result = _json_object(record.get("pipeline_result"))
        latest_execution_result = _json_object(record.get("latest_execution_result"))
        current_commit = _optional_text(record.get("current_commit")) or ""
        acceptance_criteria = _acceptance_criteria(issue_body, issue_title, planner_result)
        plan = _context_entries(planner_result.get("steps"))
        review_findings = _context_entries(review_result.get("findings") if review_result else None)
        failed_checks = _pipeline_failures(pipeline_result)
        previous_failures = _previous_failures(
            record,
            latest_execution_result,
        )
        diff_summary = _diff_summary(current_commit, latest_execution_result)
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
                    acceptance_criteria=acceptance_criteria,
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
                    acceptance_criteria=acceptance_criteria,
                    plan=plan,
                    previous_failures=previous_failures,
                    current_commit=current_commit,
                    diff_summary=diff_summary,
                    failed_checks=failed_checks,
                    review_findings=review_findings,
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
                plan=plan,
                previous_failures=previous_failures,
                current_commit=current_commit,
                diff_summary=diff_summary,
                failed_checks=failed_checks,
                review_findings=review_findings,
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
        """Claims the half-open probe on both this issue's circuits, or neither.

        The claim runs in its own savepoint (issue #92). Returning False from
        inside the caller's transaction used to *commit* whatever had already
        been claimed, so a project could be left `half_open` pointing at a
        workflow run the aborted `schedule()` never inserted -- and `schedule()`
        skips half-open projects, so that project never scheduled again.

        `probe_workflow_run_id` is deliberately not part of the claim
        predicate: the row is already locked and `state = 'open'` means no probe
        is outstanding, so any pointer still on it is stale and must not be
        allowed to block the claim forever.
        """
        circuits = (
            ("app.project_circuit_state", "project_id", project_id),
            ("app.provider_circuit_state", "provider", provider),
        )
        try:
            async with connection.transaction():
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
                        raise _CircuitProbeUnavailable
                    opened_at = record["opened_at"]
                    if (
                        not isinstance(opened_at, datetime)
                        or opened_at + self._circuit_probe_cooldown > now
                    ):
                        raise _CircuitProbeUnavailable
                for table, key, value, record in locked:
                    if record is None or str(record["state"]) == "closed":
                        continue
                    claimed = await connection.execute(
                        f"""
                        UPDATE {table}
                        SET state = 'half_open', probe_workflow_run_id = $2, updated_at = $3
                        WHERE {key} = $1 AND state = 'open'
                        """,
                        value,
                        workflow_id,
                        now,
                    )
                    if claimed != "UPDATE 1":
                        raise _CircuitProbeUnavailable
        except _CircuitProbeUnavailable:
            return False
        return True

    async def reap_orphaned_circuit_probes(self, now: datetime) -> dict[str, int]:
        """Reopens half-open circuits whose probe can no longer resolve them.

        The scheduler's maintenance pass calls this (issue #92). Without it a
        probe workflow that is deleted, or whose terminal write never reached
        the circuit, leaves the project or provider unschedulable forever.
        """
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                return await reap_orphaned_probes(connection, now, self._circuit_probe_cooldown)

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
                    f"""
                    SELECT i.id AS issue_id, i.project_id, i.provider, i.external_id, i.priority,
                           i.external_created_at, i.last_synced_at,
                           p.enabled, p.configuration, r.id AS runner_id, r.labels,
                           r.enabled AS runner_enabled, r.draining, r.status
                    FROM app.issues AS i
                    JOIN app.projects AS p ON p.id = i.project_id
                    JOIN app.runners AS r ON r.status = 'online'
                    WHERE {_scheduling_conditions("i", "p", "$1")}
                      AND {_runner_conditions("r")}
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

    async def list_queue(self, now: datetime, limit: int = 50) -> list[QueueEntryRecord]:
        """Return the eligible-issue queue in the scheduler's own ordering.

        Every eligible open issue appears, disabled projects included, so an
        operator can see *why* the top entries are not running. Each entry
        carries a machine-readable `blocked_reason`; it is empty exactly when
        `schedule()` would consider the issue schedulable, because the reason
        is computed from the same predicate fragments `schedule()` embeds.
        """
        if limit < 1 or limit > 100:
            raise ValueError("limit must be between 1 and 100")
        records = await self._pool.fetch(
            f"""
            SELECT i.external_id, i.title, i.priority,
                   p.id AS project_id, p.name AS project_name,
                   {_blocked_reason_case("i", "p", "$1")} AS blocked_reason
            FROM app.issues AS i
            JOIN app.projects AS p ON p.id = i.project_id
            WHERE i.eligible = true
              AND i.state = 'open'
            ORDER BY i.priority DESC, i.external_created_at, i.last_synced_at,
                     i.project_id, i.external_id
            LIMIT $2
            """,
            now - self._circuit_probe_cooldown,
            limit,
        )
        return [
            {
                "project_id": str(record["project_id"]),
                "project_name": str(record["project_name"]),
                "external_id": str(record["external_id"]),
                "title": str(record["title"]),
                "priority": int(record["priority"]),
                "blocked_reason": str(record["blocked_reason"]),
            }
            for record in records
        ]

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
                    if await self._release_unanswered_offer(
                        connection, offer["job_id"], None, "offer_expired", now
                    ):
                        expired.append(str(offer["job_id"]))
        return tuple(expired)

    async def _release_unanswered_offer(
        self, connection: Any, job_id: Any, runner_id: Any, reason: str, now: datetime
    ) -> bool:
        """Releases a job whose offer was never answered, without destroying
        work the run already completed (issue #91).

        A bootstrap offer -- a run `schedule()` just created that has never
        accepted a job -- is cancelled: it owns no branch, no pull request and
        no execution history, and cancelling it hands the issue straight back
        to the global queue.

        "Never accepted a job" is tested directly, against `app.job_offers`.
        It used to ride on `current_phase = 'offered'`, which held only because
        `accept_offer` overwrote the phase; now that it does not (issue #141),
        that proxy would also match a run whose first offer *was* accepted by a
        runner that then died silently, and cancelling such a run is not
        equivalent. Cancelling releases the project lock and leaves the issue
        eligible, so `schedule()` immediately builds a **new** run with a new
        job -- and the unanswered-offer streak below is counted per job, so
        every cycle would reset the budget that is supposed to stop it. The run
        would churn forever instead of blocking. Both columns are kept in the
        predicate: the phase says no node ever committed, the offer says no
        runner ever took the job, and only a run for which both hold has
        nothing whatsoever to preserve.

        Every other offer belongs to a run with progress, so it is returned to
        a schedulable state instead: the workflow keeps its phase and its
        project lock, a leaked `dispatched` execution request goes back to
        `queued` for `schedule_execution`, and a recovery offer goes back to
        `recovering` for `recover_one`. Runs cannot ping-pong forever: once the
        unanswered offers for a job reach `unanswered_offer_limit` and have been
        failing for longer than `unanswered_offer_grace`, the run is blocked
        with a specific reason and stops holding the project lock.

        Returns False when the job is no longer offered (it was accepted or
        released concurrently), leaving the caller to decide what that means.
        """
        context = await connection.fetchrow(
            """
            SELECT j.workflow_run_id, j.project_id, request.id AS request_id,
                   w.status IN ('completed', 'blocked', 'failed', 'cancelled') AS terminal,
                   (
                       w.status = 'offered' AND w.current_phase = 'offered'
                       AND w.branch_name IS NULL AND w.pull_request_external_id IS NULL
                       AND w.total_agent_executions = 0
                       AND NOT EXISTS (SELECT 1 FROM app.executions AS e WHERE e.job_id = j.id)
                       AND NOT EXISTS (
                           SELECT 1 FROM app.workflow_execution_requests AS r
                           WHERE r.workflow_run_id = w.id
                       )
                       AND NOT EXISTS (
                           SELECT 1 FROM app.job_offers AS taken
                           WHERE taken.job_id = j.id AND taken.status = 'accepted'
                       )
                   ) AS bootstrap
            FROM app.jobs AS j
            JOIN app.workflow_runs AS w ON w.id = j.workflow_run_id
            LEFT JOIN LATERAL (
                SELECT id FROM app.workflow_execution_requests
                WHERE workflow_run_id = w.id AND status = 'dispatched'
                ORDER BY dispatched_at DESC, id DESC
                LIMIT 1
            ) AS request ON true
            WHERE j.id = $1 AND j.status = 'offered'
              AND ($2::uuid IS NULL OR j.runner_id = $2)
            FOR UPDATE OF j, w
            """,
            job_id,
            runner_id,
        )
        if context is None:
            return False
        workflow_run_id = context["workflow_run_id"]
        if bool(context["terminal"]):
            # The run reached a terminal status while this offer was
            # outstanding: release the job without resurrecting the run.
            await self._cancel_offered_job(connection, job_id, context, reason, now, cancel_run=False)
            return True
        if bool(context["bootstrap"]):
            await self._cancel_offered_job(connection, job_id, context, reason, now)
            return True
        streak = await connection.fetchrow(
            """
            SELECT COUNT(*) AS unanswered, MIN(created_at) AS started_at
            FROM app.job_offers
            WHERE job_id = $1 AND status IN ('expired', 'rejected')
              AND created_at > COALESCE(
                  (SELECT MAX(created_at) FROM app.job_offers WHERE job_id = $1 AND status = 'accepted'),
                  '-infinity'::timestamptz
              )
            """,
            job_id,
        )
        unanswered = int(streak["unanswered"]) if streak is not None else 1
        started_at = streak["started_at"] if streak is not None else now
        exhausted = unanswered >= self._unanswered_offer_limit and (
            started_at is None or started_at <= now - self._unanswered_offer_grace
        )
        if exhausted:
            await self._block_unanswered_run(connection, job_id, context, reason, now)
        elif context["request_id"] is not None:
            # Hand the execution request back to schedule_execution, which
            # re-offers this same job on a later tick.
            await connection.execute(
                """
                UPDATE app.workflow_execution_requests
                SET status = 'queued', dispatched_at = NULL
                WHERE id = $1 AND status = 'dispatched'
                """,
                context["request_id"],
            )
            await connection.execute(
                """
                UPDATE app.jobs
                SET status = 'cancelled', finished_at = $2, recovery_reason = $3
                WHERE id = $1
                """,
                job_id,
                now,
                reason,
            )
            await connection.execute(
                "UPDATE app.workflow_runs SET updated_at = $2 WHERE id = $1",
                workflow_run_id,
                now,
            )
        else:
            # No execution request to requeue (a recovery offer, or a run whose
            # progress predates one): fence the lease and hand it to recover_one.
            await connection.execute(
                """
                UPDATE app.jobs
                SET status = 'recovering', lease_generation = lease_generation + 1,
                    finished_at = NULL, recovery_reason = $2
                WHERE id = $1
                """,
                job_id,
                reason,
            )
            await connection.execute(
                "UPDATE app.workflow_runs SET status = 'recovering', updated_at = $2 WHERE id = $1",
                workflow_run_id,
                now,
            )
        await connection.execute(
            """
            INSERT INTO app.workflow_events (workflow_run_id, event_type, severity, payload, created_at)
            VALUES ($1, 'offer_unanswered', $2, $3::jsonb, $4)
            """,
            workflow_run_id,
            "error" if exhausted else "warning",
            json.dumps(
                {
                    "job_id": str(job_id),
                    "reason": reason,
                    "unanswered_offers": unanswered,
                    "outcome": "blocked" if exhausted else "requeued",
                },
                separators=(",", ":"),
                sort_keys=True,
            ),
            now,
        )
        return True

    async def _cancel_offered_job(
        self,
        connection: Any,
        job_id: Any,
        context: Any,
        reason: str,
        now: datetime,
        *,
        cancel_run: bool = True,
    ) -> None:
        await connection.execute(
            """
            UPDATE app.jobs
            SET status = 'cancelled', finished_at = $2, recovery_reason = $3
            WHERE id = $1
            """,
            job_id,
            now,
            reason,
        )
        if cancel_run:
            await connection.execute(
                """
                UPDATE app.workflow_runs
                SET status = 'cancelled', current_phase = 'cancelled', terminal_reason = $3,
                    completed_at = $2, updated_at = $2
                WHERE id = $1
                """,
                context["workflow_run_id"],
                now,
                reason,
            )
            # This run reaches a terminal status without going through
            # AsyncpgWorkflowPersistence.transition, so its half-open probe (if
            # any) has to be released here too -- issue #92, wedge 2: the
            # bootstrap run cancelled by offer expiry is exactly the probe
            # `schedule()` just claimed.
            await reopen_probe_circuits(connection, context["workflow_run_id"], now)
        await connection.execute(
            "DELETE FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
            context["project_id"],
            context["workflow_run_id"],
        )

    async def _block_unanswered_run(
        self, connection: Any, job_id: Any, context: Any, reason: str, now: datetime
    ) -> None:
        await connection.execute(
            """
            UPDATE app.jobs
            SET status = 'cancelled', finished_at = $2, recovery_reason = $3
            WHERE id = $1
            """,
            job_id,
            now,
            reason,
        )
        await connection.execute(
            """
            UPDATE app.workflow_runs
            SET status = 'blocked', current_phase = 'blocked', blocking_reason = $3,
                terminal_reason = $3, completed_at = COALESCE(completed_at, $2), updated_at = $2
            WHERE id = $1
            """,
            context["workflow_run_id"],
            now,
            "unanswered_offer_limit",
        )
        await connection.execute(
            """
            UPDATE app.workflow_execution_requests
            SET status = 'expired'
            WHERE workflow_run_id = $1 AND status IN ('queued', 'dispatched')
            """,
            context["workflow_run_id"],
        )
        # Terminal without a workflow transition, so release the probe here too
        # (issue #92). The run never ran, so this is "the probe did not
        # deliver", not a verdict on the project.
        await reopen_probe_circuits(connection, context["workflow_run_id"], now)
        await connection.execute(
            "DELETE FROM app.project_locks WHERE project_id = $1 AND workflow_run_id = $2",
            context["project_id"],
            context["workflow_run_id"],
        )

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
                    # `recovering` is a scheduling state, not a phase: the
                    # re-offer `recover_one` makes carries the *same* dispatched
                    # execution request, so the phase that queued it has to
                    # survive for the eventual terminal event to transition
                    # (issue #141).
                    await connection.execute(
                        """
                        UPDATE app.workflow_runs
                        SET status = 'recovering', updated_at = $2
                        WHERE id = $1
                        """,
                        job["workflow_run_id"],
                        now,
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
                    SET status = 'offered', updated_at = $2
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
        """Records that the assigned runner took the job.

        Acceptance is a fact about the *job*, so it may only move the run's
        `status` -- the scheduling-lifecycle column. `current_phase` is the
        graph's, written by `AsyncpgWorkflowPersistence.transition` when a node
        commits a phase, and it is the only durable record of which node a
        suspended run is waiting on. Both `implement` and `push` dispatch the
        `developer` role, so the phase is the one thing that tells their
        terminal events apart; overwriting it here left a successful developer
        execution with no transition at all, so the graph was never resumed and
        the job ran until its lease expired, forever (issue #141).

        `preparing` still goes on `status` deliberately: it is what makes
        `find_stalled_workflow_runs` see a run whose execution can no longer
        report, and what the console renders while a runner is preparing a
        workspace.
        """
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
                    UPDATE app.workflow_runs SET status = 'preparing', updated_at = $2
                    WHERE id = (SELECT workflow_run_id FROM app.jobs WHERE id = $1)
                    """,
                    _uuid(job_id),
                    now,
                )
        return JobLease(str(job_id), runner_id, int(job["lease_generation"]), job["lease_expires_at"])

    async def reject_offer(self, job_id: str, runner_id: str, now: datetime) -> None:
        """Releases an offer the assigned runner refused (or the scheduler
        could not deliver). A refusal says nothing about the work the run has
        already done, so it takes the same non-destructive release path as an
        expired offer."""
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
                released = await self._release_unanswered_offer(
                    connection, _uuid(job_id), _uuid(runner_id), "runner_rejected_offer", now
                )
                if not released:
                    raise OfferError("job offer is no longer active")

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
                           j.last_event_sequence, w.current_phase AS workflow_phase,
                           w.last_diff_hash,
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
                resolved_request_id, resolved_role, resolved_attempt = (
                    resolved if resolved is not None else (None, None, None)
                )

                if summary.terminal and resolved_request_id is not None:
                    # The request this execution belongs to is finished: close it in
                    # the same transaction that records the outcome. Leaving it
                    # `dispatched` would keep the run permanently outside
                    # find_stalled_workflow_runs' predicate and would let
                    # schedule_execution re-offer work that already reported back
                    # (issue #94). The status mirrors the runner's terminal event.
                    await connection.execute(
                        """
                        UPDATE app.workflow_execution_requests
                        SET status = $2
                        WHERE id = $1 AND status = 'dispatched'
                        """,
                        resolved_request_id,
                        summary.event_type,
                    )

                # The phase, not the status: `accept_offer` stamps the run
                # `preparing` for the whole life of the execution, so the
                # status can never say `implementing` or `pushing` by the time
                # a developer's terminal event lands (issue #141). The phase is
                # what the dispatching node committed, and it survives both
                # acceptance and a recovery re-offer.
                current_phase = str(job["workflow_phase"])
                transition = workflow_transition_for_terminal_event(
                    summary, current_phase, role=resolved_role
                )
                progress_updates = await self._record_progress_evidence(
                    connection, job, summary, event.payload, now, resolved_role
                )
                repeats = progress_updates.get("non_progress_attempts", 0)
                # The stored counter is 0 for the first outcome of a run, so a
                # run of N identical outcomes stores N-1.
                identical_outcomes = repeats + 1 if isinstance(repeats, int) else 0
                if identical_outcomes >= NON_PROGRESS_OUTCOME_LIMIT:
                    transition = WorkflowTransition(
                        new_status="blocked",
                        state_updates={
                            **(transition.state_updates if transition is not None else {}),
                            **progress_updates,
                            "status": "blocked",
                            "blocking_reason": (
                                f"workflow stopped after {NON_PROGRESS_OUTCOME_LIMIT} "
                                "identical execution outcomes"
                            ),
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
                    VALUES ($1, $2, 'info', $3::jsonb, $4)
                    """,
                    job["workflow_run_id"],
                    summary.event_type,
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
                        result_json = json.dumps(
                            _execution_result(summary, event.payload), separators=(",", ":"), sort_keys=True
                        )
                        await connection.execute(
                            """
                            INSERT INTO app.executions
                                (id, job_id, execution_type, attempt, status, lease_generation, started_at, finished_at, timeout_seconds, exit_code, result)
                            VALUES (gen_random_uuid(), $1, $2, $8, $3, $7, $4, $4, 3600, $5, $6::jsonb)
                            ON CONFLICT (job_id, execution_type, attempt) DO NOTHING
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

                if summary.terminal:
                    final_revision = _final_revision(event.payload)
                    if final_revision:
                        await connection.execute(
                            "UPDATE app.workflow_runs SET current_commit = $2, updated_at = $3 WHERE id = $1",
                            job["workflow_run_id"],
                            final_revision,
                            now,
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
                        json.dumps(_execution_result(summary, event.payload), separators=(",", ":"), sort_keys=True),
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
        role: str | None,
    ) -> dict[str, object]:
        """Records the identity of a terminal outcome and counts how many times
        it has repeated.

        A terminal outcome is either a success, identified by a diff hash, or a
        failure, identified by a failure fingerprint. Both identities are scoped
        by the execution's role, and each is only ever compared with the stored
        identity of its own kind -- a failure is never measured against a diff
        hash. The two columns therefore hold the last outcome *per kind*: a
        success does not erase the failure a later failure must be compared
        with, and vice versa.
        """
        if not summary.terminal:
            return {}
        scope = role or "unknown"
        diff_hash: str | None = None
        fingerprint: str | None = None
        if summary.succeeded:
            diff_hash = current = _success_outcome_hash(scope, summary)
            previous = _stored_outcome(job.get("last_diff_hash"))
        else:
            fingerprint = current = _failure_outcome_hash(scope, summary, payload)
            previous = _stored_outcome(job.get("last_failure_fingerprint"))
        same_outcome = previous is not None and previous == current
        attempts = int(job.get("non_progress_attempts") or 0) + 1 if same_outcome else 0
        await connection.execute(
            """
            UPDATE app.workflow_runs
            SET last_diff_hash = COALESCE($2, last_diff_hash),
                last_failure_fingerprint = COALESCE($3, last_failure_fingerprint),
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
        # Only the column this outcome actually wrote is reported, so replaying
        # these updates through the workflow persistence layer cannot clear the
        # other kind's identity. "No diff" has one encoding -- SQL NULL -- so
        # the key is absent rather than "".
        updates: dict[str, object] = {
            "non_progress_attempts": attempts,
            "progressed": not same_outcome,
        }
        if diff_hash is not None:
            updates["last_diff_hash"] = diff_hash
        else:
            updates["last_failure_fingerprint"] = fingerprint
        return updates

    async def _resolve_dispatched_execution(
        self, connection: Any, workflow_run_id: str, job_id: str, execution_id: str
    ) -> tuple[UUID | None, str, int] | None:
        """Resolves (request id, role, attempt) for an execution ID by joining
        the dispatched request it claims to be -- never by trusting the string
        itself. Returns None when the execution ID does not correspond to
        anything this orchestrator actually dispatched for this job; the
        request id is None for the bootstrap planner dispatch, which has no
        request row to close.
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
                return None, "planner", 1
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
            SELECT id, role, attempt
            FROM app.workflow_execution_requests
            WHERE id = $1 AND workflow_run_id = $2 AND status = 'dispatched'
            FOR UPDATE
            """,
            request_id,
            _uuid(workflow_run_id),
        )
        if row is not None:
            return UUID(str(row["id"])), str(row["role"]), int(row["attempt"])
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
        """Delivers the row accept_event just committed, inline.

        The claim is what keeps this from racing the background drain: both
        take the same lease, so a maintenance tick that lands in the window
        between accept_event's commit and this call delivers the transition
        once, not twice. Losing the claim is not a failure -- whoever holds it
        is delivering the same transition.
        """
        claim = await self._claim_outbox_entry(outbox_id, now)
        if claim is None:
            return
        try:
            await on_transition(workflow_run_id, new_status, state_updates)
        except Exception as error:  # noqa: BLE001 - releases the outbox row for the background drain to retry
            await self._release_outbox_entry(outbox_id, claim, error)
            return
        await self._complete_outbox_entry(outbox_id, claim, now)

    async def _claim_outbox_entry(self, outbox_id: UUID, now: datetime) -> datetime | None:
        """Takes the processing lease on one row, returning the claim stamp.

        The stamp is the fence. A delivery that outlives its own lease has
        already had the row reclaimed by another drainer, and must not then
        release or complete work that is no longer its own: doing so would
        knock the current holder's claim out from under it and hand the same
        transition to a third drainer.
        """
        claimed = await self._pool.fetchrow(
            """
            UPDATE app.workflow_transition_outbox
            SET status = 'processing', processing_started_at = $2
            WHERE id = $1 AND status = 'pending'
            RETURNING processing_started_at
            """,
            outbox_id,
            now,
        )
        return None if claimed is None else now

    async def _release_outbox_entry(
        self, outbox_id: UUID, claim: datetime, error: Exception
    ) -> None:
        await self._pool.execute(
            """
            UPDATE app.workflow_transition_outbox
            SET status = 'pending', processing_started_at = NULL,
                attempts = attempts + 1, last_error = $3
            WHERE id = $1 AND status = 'processing' AND processing_started_at = $2
            """,
            outbox_id,
            claim,
            str(error)[:1024],
        )

    async def _complete_outbox_entry(
        self, outbox_id: UUID, claim: datetime, now: datetime
    ) -> None:
        await self._pool.execute(
            """
            UPDATE app.workflow_transition_outbox
            SET status = 'processed', processed_at = $3
            WHERE id = $1 AND status = 'processing' AND processing_started_at = $2
            """,
            outbox_id,
            claim,
            now,
        )

    async def drain_pending_transitions(
        self,
        on_transition: Callable[[str, str, dict[str, object]], Awaitable[None]],
        now: datetime,
        limit: int = 50,
        processing_lease: timedelta = _OUTBOX_PROCESSING_LEASE,
    ) -> int:
        """At-least-once delivery for transitions an earlier accept_event call
        committed but never finished invoking (process crash, DB blip, etc).
        Intended to be polled periodically by a background worker.

        `processing` is a lease, not a terminal state. A drainer marks the row,
        commits, and only then calls out to the graph runtime -- so a process
        that dies mid-delivery leaves the row claimed with nobody to release
        it, and selecting `pending` alone dropped that transition forever
        (issue #96). Rows whose claim is older than `processing_lease` are
        therefore taken back. The lease has to outlast a real delivery, which
        reaches GitHub through the graph, or a slow-but-live drain would be
        raced by the next tick; delivering twice is safe (nodes reuse the
        execution request they already have) but pointless.

        `now` is also the claim stamp, and every row in one pass shares it.
        Completing or releasing a row requires that stamp to still be on it, so
        a drainer whose delivery outlived its lease finds the row already
        reclaimed and leaves it to whoever holds it now.
        """
        async with self._pool.acquire() as connection:
            rows = await connection.fetch(
                """
                UPDATE app.workflow_transition_outbox
                SET status = 'processing', processing_started_at = $2
                WHERE id IN (
                    SELECT id FROM app.workflow_transition_outbox
                    WHERE status = 'pending'
                       OR (
                           status = 'processing'
                           AND (processing_started_at IS NULL OR processing_started_at <= $3)
                       )
                    ORDER BY created_at
                    LIMIT $1
                    FOR UPDATE SKIP LOCKED
                )
                RETURNING id, workflow_run_id, new_status, state_updates
                """,
                limit,
                now,
                now - processing_lease,
            )
        processed = 0
        for row in rows:
            state_updates = row["state_updates"]
            if isinstance(state_updates, str):
                state_updates = json.loads(state_updates)
            try:
                await on_transition(str(row["workflow_run_id"]), str(row["new_status"]), state_updates)
            except Exception as error:  # noqa: BLE001 - releases the outbox row for the next drain pass
                await self._release_outbox_entry(row["id"], now, error)
                continue
            await self._complete_outbox_entry(row["id"], now, now)
            processed += 1
        return processed

    async def close_orphaned_execution_requests(self, now: datetime, stale_after: timedelta) -> int:
        """Closes execution requests that can never be executed or reported on.

        Two kinds leak (issue #94). A request still open on a run that has
        already reached a terminal status would let `schedule_execution` offer
        work for a finished run. A `dispatched` request whose workflow run has
        no job that could still produce its terminal event -- the runner's job
        was cancelled or released without the request being requeued -- pins
        the run outside `find_stalled_workflow_runs`' predicate forever.

        The second rule is time-boxed by `stale_after` so it can never race a
        `schedule_execution` transaction that is still in flight. Returns the
        number of rows closed.
        """
        rows = await self._pool.fetch(
            """
            UPDATE app.workflow_execution_requests AS req
            SET status = $2
            WHERE req.status IN ('queued', 'dispatched')
              AND (
                  EXISTS (
                      SELECT 1 FROM app.workflow_runs AS wr
                      WHERE wr.id = req.workflow_run_id
                        AND wr.status IN ('completed', 'blocked', 'failed', 'cancelled')
                  )
                  OR (
                      req.status = 'dispatched'
                      AND COALESCE(req.dispatched_at, req.created_at) <= $1
                      AND NOT EXISTS (
                          SELECT 1 FROM app.jobs AS j
                          WHERE j.workflow_run_id = req.workflow_run_id
                            AND j.status IN ('offered', 'preparing', 'running', 'recovering')
                      )
                  )
              )
            RETURNING req.id
            """,
            now - stale_after,
            _ORPHANED_REQUEST_STATUS,
        )
        return len(rows)

    async def find_workflow_runs_waiting_for_checks(self, limit: int = 50) -> tuple[str, ...]:
        rows = await self._pool.fetch(
            """
            SELECT id
            FROM app.workflow_runs
            WHERE status = 'waiting_github_checks' AND current_phase = 'waiting_github_checks'
            ORDER BY updated_at, id
            LIMIT $1
            """,
            limit,
        )
        return tuple(str(row["id"]) for row in rows)

    async def find_stalled_workflow_runs(
        self, now: datetime, stale_after: timedelta, limit: int = 50
    ) -> tuple[str, ...]:
        """Workflow runs whose status says an agent execution should be in
        flight but which have no queued/dispatched execution request and no
        job that could still deliver one -- the symptom of a crash between
        committing a transition and invoking the graph runtime (see
        accept_event / the outbox above).

        The statuses are an allow-list rather than "everything non-terminal"
        on purpose. `pr_created`, `waiting_github_checks`, `waiting_human` and
        `merging` are parked on an external event (a GitHub check, a human
        decision), not on an execution, and re-entering the graph for those
        would resolve a pending human-approval interrupt as "not approved".
        `offered` is excluded because an unanswered offer already has an owner
        (`expire_offers`), while `preparing` -- the status `accept_offer`
        stamps for the whole life of an execution -- is included: once no job
        can report that execution any more, nothing else will ever move the
        run.

        Jobs in `recovering` count as active work: `recover_one` already owns
        them. A workflow run in `recovering` whose job is not, on the other
        hand, is unreachable for `recover_one` (it requires both) and is
        exactly the kind of stall this query exists to find.
        """
        rows = await self._pool.fetch(
            """
            SELECT wr.id
            FROM app.workflow_runs AS wr
            WHERE wr.status IN ('preparing', 'planning', 'implementing', 'local_pipeline',
                                'repairing', 'ai_review', 'pushing', 'recovering')
              AND wr.updated_at <= $1
              AND NOT EXISTS (
                  SELECT 1 FROM app.workflow_execution_requests AS req
                  WHERE req.workflow_run_id = wr.id AND req.status IN ('queued', 'dispatched')
              )
              AND NOT EXISTS (
                  SELECT 1 FROM app.jobs AS j
                  WHERE j.workflow_run_id = wr.id
                    AND j.status IN ('offered', 'preparing', 'running', 'recovering')
              )
            ORDER BY wr.updated_at, wr.id
            LIMIT $2
            """,
            now - stale_after,
            limit,
        )
        return tuple(str(row["id"]) for row in rows)

    async def recover_stalled_workflow_run(
        self,
        workflow_run_id: str,
        on_transition: Callable[[str, str, dict[str, object]], Awaitable[None]],
        now: datetime,
    ) -> bool:
        """Repairs one stalled run, in the one way its own history justifies.

        A stalled run is one of two things, and the distinction matters because
        the graph is an event-driven state machine: it is suspended on the edge
        out of a dispatching node, and the only thing that legitimately moves it
        forward is the terminal event for the execution it queued (issue #88).

        *The execution was lost* -- the last request was closed as `orphaned`,
        meaning no runner will ever report it. The phase has to run again, so a
        fresh `queued` request is written for the same role and the graph is
        left suspended exactly where it is. `schedule_execution` offers it, the
        runner runs it, and its terminal event resumes the graph through the
        normal path. Nothing is advanced on a verdict nobody produced: three of
        the six dispatching nodes (`implement`, `repair`, `push`) have
        unconditional outgoing edges, so merely clearing `awaiting_execution`
        there would *skip* the lost phase -- for `push` that means creating a
        pull request for a branch that was never pushed, then merging it.

        *The transition was committed but never handed to the graph* -- the
        last request was closed by its own terminal event
        (`completed`/`failed`/`cancelled`), so the execution really did report
        and `accept_event` really did commit the new status; only the graph
        invocation was lost (a crash, or an outbox row stuck mid-drain). Here
        advancing is exactly right, and `awaiting_execution` is cleared because
        the detector has already established nothing is in flight -- without
        that the resumed graph routes straight back to END. The state updates
        that rode on the outbox row are not replayed, so a gate the lost
        invocation would have set stays as it was and the phase is re-run
        rather than skipped; that is safe, and bounded by the same retry
        budgets.

        Either way the run's `updated_at` is bumped, so a run this cannot
        repair backs off a full stall window instead of reoccupying the
        maintenance loop's bounded batch on every tick.

        Returns False when nothing was repaired: the run disappeared, reached a
        terminal status between detection and recovery, or has already had
        `_LOST_EXECUTION_REQUEUE_LIMIT` executions of the same role lost.
        """
        async with self._pool.acquire() as connection:
            async with connection.transaction():
                record = await connection.fetchrow(
                    """
                    SELECT wr.status, request.role, request.status AS request_status,
                           request.next_attempt, request.lost_attempts
                    FROM app.workflow_runs AS wr
                    LEFT JOIN LATERAL (
                        SELECT r.role, r.status,
                               (
                                   SELECT MAX(a.attempt) + 1
                                   FROM app.workflow_execution_requests AS a
                                   WHERE a.workflow_run_id = wr.id AND a.role = r.role
                               ) AS next_attempt,
                               (
                                   SELECT COUNT(*)
                                   FROM app.workflow_execution_requests AS a
                                   WHERE a.workflow_run_id = wr.id AND a.role = r.role
                                     AND a.status = $2
                               ) AS lost_attempts
                        FROM app.workflow_execution_requests AS r
                        WHERE r.workflow_run_id = wr.id
                        ORDER BY r.created_at DESC, r.id DESC
                        LIMIT 1
                    ) AS request ON true
                    WHERE wr.id = $1
                    FOR UPDATE OF wr
                    """,
                    _uuid(workflow_run_id),
                    _ORPHANED_REQUEST_STATUS,
                )
                if record is None:
                    return False
                status = str(record["status"])
                if status in _TERMINAL_WORKFLOW_STATUSES:
                    return False
                # Whichever branch runs, this run has had its turn: the
                # timestamp is what the detector reads, so bumping it here --
                # rather than relying on the repair succeeding -- keeps a run
                # that fails to recover from occupying the batch every tick and
                # starving newer stalls.
                await connection.execute(
                    "UPDATE app.workflow_runs SET updated_at = $2 WHERE id = $1",
                    _uuid(workflow_run_id),
                    now,
                )
                if record["request_status"] == _ORPHANED_REQUEST_STATUS:
                    if int(record["lost_attempts"]) > _LOST_EXECUTION_REQUEUE_LIMIT:
                        _LOGGER.error(
                            "refusing to requeue a repeatedly lost execution",
                            extra={
                                "workflow_run_id": workflow_run_id,
                                "role": str(record["role"]),
                                "lost_attempts": int(record["lost_attempts"]),
                            },
                        )
                        return False
                    await self._requeue_lost_execution(
                        connection, workflow_run_id, str(record["role"]),
                        int(record["next_attempt"]), now,
                    )
                    return True
        await on_transition(workflow_run_id, status, {"awaiting_execution": False})
        return True

    async def _requeue_lost_execution(
        self, connection: Any, workflow_run_id: str, role: str, attempt: int, now: datetime
    ) -> None:
        # The attempt counters were already charged when the node dispatched
        # the execution that was lost, so the replacement deliberately spends
        # no further budget: it delivers the attempt that was already paid for.
        # `_LOST_EXECUTION_REQUEUE_LIMIT` is what bounds it instead, since no
        # retry budget would ever trip.
        await connection.execute(
            """
            INSERT INTO app.workflow_execution_requests
                (id, workflow_run_id, role, attempt, status, created_at)
            VALUES ($1, $2, $3, $4, 'queued', $5)
            """,
            uuid4(),
            _uuid(workflow_run_id),
            role,
            attempt,
            now,
        )
        await connection.execute(
            """
            INSERT INTO app.workflow_events (workflow_run_id, event_type, severity, payload, created_at)
            VALUES ($1, 'execution_requeued', 'warning', $2::jsonb, $3)
            """,
            _uuid(workflow_run_id),
            json.dumps(
                {"role": role, "attempt": attempt, "reason": "execution_lost"},
                separators=(",", ":"),
                sort_keys=True,
            ),
            now,
        )

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


async def _replace_pipeline_steps(connection: Any, project_id: UUID, steps: tuple[dict[str, object], ...]) -> None:
    await connection.execute("DELETE FROM app.project_pipeline_steps WHERE project_id = $1", project_id)
    for step in steps:
        await connection.execute(
            """
            INSERT INTO app.project_pipeline_steps
                (id, project_id, position, name, command, timeout_seconds, required)
            VALUES (gen_random_uuid(), $1, $2, $3, $3, $4, $5)
            """,
            project_id,
            step["position"],
            step["command"],
            step["timeout_seconds"],
            step["required"],
        )


def _pipeline_steps(steps: Iterable[dict[str, object]]) -> tuple[dict[str, object], ...]:
    normalized: list[dict[str, object]] = []
    positions: set[int] = set()
    for step in steps:
        command = step.get("command")
        timeout = step.get("timeout_seconds")
        position = step.get("position")
        required = step.get("required")
        if (
            not isinstance(command, str)
            or not command.strip()
            or not isinstance(timeout, int)
            or timeout <= 0
            or not isinstance(position, int)
            or position < 0
            or position in positions
            or not isinstance(required, bool)
        ):
            raise ValueError("pipeline steps are invalid")
        positions.add(position)
        normalized.append(
            {
                "command": command.strip(),
                "timeout_seconds": timeout,
                "position": position,
                "required": required,
            }
        )
    return tuple(sorted(normalized, key=lambda step: cast(int, step["position"])))


def _project_record(record: dict[str, Any]) -> ProjectRecord:
    configuration = record["configuration"]
    if isinstance(configuration, str):
        configuration = json.loads(configuration)
    labels = configuration.get("required_runner_labels") if isinstance(configuration, dict) else None
    steps = record.get("pipeline_steps", [])
    if isinstance(steps, str):
        steps = json.loads(steps)
    return {
        "id": str(record["id"]),
        "name": str(record["name"]),
        "enabled": bool(record["enabled"]),
        "repository_mode": str(record["repository_mode"]),
        "repository_url": _optional_text(record["repository_url"]),
        "local_repository_path": _optional_text(record["local_repository_path"]),
        "default_branch": str(record["default_branch"]),
        "required_runner_labels": list(labels) if isinstance(labels, list) else [],
        "pipeline_steps": list(steps) if isinstance(steps, list) else [],
    }


def _final_revision(payload: dict[str, Any]) -> str:
    value = payload.get("finalRevision")
    if not isinstance(value, str):
        return ""
    revision = value.strip()
    return revision if 0 < len(revision) <= 256 else ""


def _execution_result(summary: Any, payload: dict[str, Any]) -> dict[str, Any]:
    result: dict[str, Any] = {
        "changedFiles": summary.changed_files,
        "commandsRun": summary.commands_run,
    }
    for key in ("summary", "logTail", "pipelineResults", "remainingWork"):
        value = payload.get(key)
        if value is not None:
            result[key] = value
    if summary.result is not None:
        result["result"] = summary.result
    return result


def _schema_result(value: Any, schema_name: str) -> dict[str, Any]:
    result = _json_object(value)
    try:
        return result if not validate(result, load_schema(schema_name)) else {}
    except (SchemaNotFoundError, ValueError):
        return {}


def _json_object(value: Any) -> dict[str, Any]:
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except json.JSONDecodeError:
            return {}
    return value if isinstance(value, dict) else {}


def _bounded_context_entry(value: str) -> str:
    encoded = value.strip().encode()
    if len(encoded) <= _CONTEXT_ENTRY_BYTES:
        return encoded.decode()
    return encoded[:_CONTEXT_ENTRY_BYTES].decode(errors="ignore").rstrip()


def _context_entries(value: Any) -> tuple[str, ...]:
    if not isinstance(value, (list, tuple)):
        return ()
    entries: list[str] = []
    for item in value:
        if isinstance(item, str):
            text = item
        elif isinstance(item, (dict, list)):
            text = json.dumps(item, separators=(",", ":"), sort_keys=True)
        else:
            continue
        text = _bounded_context_entry(text)
        if text:
            entries.append(text)
        if len(entries) == _CONTEXT_ENTRY_LIMIT:
            break
    return tuple(entries)


def _acceptance_criteria(issue_body: str, issue_title: str, planner_result: dict[str, Any]) -> tuple[str, ...]:
    issue_criteria = _context_entries(_ISSUE_CHECKLIST_ITEM.findall(issue_body))
    planner_criteria = _context_entries(planner_result.get("acceptanceCriteria"))
    criteria = tuple(dict.fromkeys((*issue_criteria, *planner_criteria)))
    return criteria[:_CONTEXT_ENTRY_LIMIT] or (_bounded_context_entry(issue_title),)


def _tail_lines(value: Any) -> str:
    if not isinstance(value, str):
        return ""
    return _bounded_context_entry("\n".join(value.strip().splitlines()[-_CONTEXT_TAIL_LINES:]))


def _pipeline_failures(result: dict[str, Any]) -> tuple[str, ...]:
    failures: list[str] = []
    raw_results = result.get("pipelineResults")
    if isinstance(raw_results, list):
        for item in raw_results:
            if not isinstance(item, dict) or not isinstance(item.get("command"), str):
                continue
            exit_code = item.get("exitCode")
            if not isinstance(exit_code, int) or exit_code == 0:
                continue
            output = _tail_lines(item.get("output"))
            text = f"{item['command']} exited {exit_code}"
            failures.append(f"{text}:\n{output}" if output else text)
    if failures:
        return _context_entries(failures)
    exit_code = result.get("exitCode")
    if isinstance(exit_code, int) and exit_code != 0:
        output = _tail_lines(result.get("logTail"))
        text = f"pipeline exited {exit_code}"
        return _context_entries((f"{text}:\n{output}" if output else text,))
    return ()


def _previous_failures(record: Any, execution_result: dict[str, Any]) -> tuple[str, ...]:
    entries: list[str] = []
    status = _optional_text(record.get("latest_execution_status"))
    if status in {"failed", "cancelled"}:
        execution_type = _optional_text(record.get("latest_execution_type")) or "execution"
        role = {
            "run_planner": "planner",
            "run_developer": "developer",
            "run_local_pipeline": "pipeline",
            "run_reviewer": "reviewer",
            "run_repair": "repairer",
        }.get(execution_type, execution_type)
        attempt = record.get("latest_execution_attempt")
        exit_code = record.get("latest_execution_exit_code")
        detail = _tail_lines(execution_result.get("summary") or execution_result.get("logTail"))
        text = f"{role} attempt {attempt if isinstance(attempt, int) else '?'} {status}"
        if isinstance(exit_code, int):
            text += f" (exit {exit_code})"
        entries.append(f"{text}:\n{detail}" if detail else text)
    for value in (
        record.get("blocking_reason"),
        record.get("last_gate_verdict"),
        record.get("human_guidance"),
        *_text_list(record.get("remaining_work")),
    ):
        if isinstance(value, str) and value.strip():
            entries.append(value)
    return _context_entries(entries)


def _diff_summary(current_commit: str, execution_result: dict[str, Any]) -> str:
    changed_files = _context_entries(execution_result.get("changedFiles"))
    if changed_files:
        return _bounded_context_entry("Changed files: " + ", ".join(changed_files))
    return _bounded_context_entry(f"Current commit: {current_commit}") if current_commit else ""


def _optional_text(value: Any) -> str | None:
    if value is None:
        return None
    return str(value)


def _workflow_detail_record(record: Any) -> WorkflowDetailRecord:
    """Maps one row of the workflow-run projection shared by list and get."""
    return {
        "id": str(record["id"]),
        "project_id": str(record["project_id"]),
        "status": str(record["status"]),
        "phase": str(record["current_phase"]),
        "issue_external_id": str(record["issue_external_id"]),
        "issue_title": str(record["issue_title"]),
        "branch_name": _optional_text(record["branch_name"]),
        "pull_request_external_id": _optional_text(record["pull_request_external_id"]),
        "pull_request_url": _optional_text(record["pull_request_url"]),
        "pull_request_state": _optional_text(record["pull_request_state"]),
        "blocking_reason": _optional_text(record["blocking_reason"]),
        "planning_attempts": int(record["planning_attempts"]),
        "implementation_attempts": int(record["implementation_attempts"]),
        "pipeline_repair_attempts": int(record["pipeline_repair_attempts"]),
        "review_cycles": int(record["review_cycles"]),
        "ci_repair_attempts": int(record["ci_repair_attempts"]),
        "total_agent_executions": int(record["total_agent_executions"]),
        "created_at": record["created_at"],
        "updated_at": record["updated_at"],
    }


def _text_list(value: Any) -> tuple[str, ...]:
    if isinstance(value, str):
        try:
            value = json.loads(value)
        except json.JSONDecodeError:
            return ()
    return tuple(item for item in value if isinstance(item, str) and item) if isinstance(value, list) else ()


def _runner_failure_fingerprint(component: str, message: str) -> str:
    """Port of the runner's `dispatch.FailureFingerprint`
    (runner/internal/dispatch/fingerprint.go), so both ends of the protocol
    agree on one definition of "the same failure".

    The runner already sends this value as `failureFingerprint` on `failed`
    events; this reproduces it for terminal events that carry none (older
    runners, `cancelled` events), and any value it returns is therefore
    directly comparable with a runner-supplied one.
    """
    normalized = message.lower()
    for marker in _FINGERPRINT_SECRET_MARKERS:
        index = normalized.find(marker)
        if index >= 0:
            normalized = normalized[:index] + marker
    for category in _FINGERPRINT_CATEGORIES:
        if category in normalized:
            component = category
            break
    digest = sha256(f"{component}\n{normalized}".encode()).hexdigest()
    return f"{component}:{digest[:16]}"


def _failure_message(summary: Any, payload: dict[str, Any]) -> str:
    """The most specific human-readable failure text a terminal payload
    carries, truncated to its first few lines so that trailing volatile
    detail cannot destabilise the fingerprint."""
    for key in ("error", "summary", "message"):
        value = payload.get(key)
        if isinstance(value, str) and value.strip():
            lines = value.strip().splitlines()[:_FINGERPRINT_MESSAGE_LINES]
            return "\n".join(lines)
    return f"{summary.event_type} exit={summary.exit_code}"


def _stable_result(result: Any) -> dict[str, Any] | None:
    """The agent result document without its per-attempt identifiers. Those
    change on every execution, so leaving them in would make two identical
    outcomes look different."""
    if not isinstance(result, dict):
        return None
    return {key: value for key, value in result.items() if key not in _VOLATILE_RESULT_KEYS}


def _success_outcome_hash(role: str, summary: Any) -> str:
    """Identity of a successful terminal outcome. Scoped by role so a zero-diff
    planner success can never collide with a zero-diff pipeline or reviewer
    success, and keyed on the result document so two attempts that produced
    different plans or review findings count as progress."""
    return sha256(
        json.dumps(
            {
                "kind": "success",
                "role": role,
                "changed_files": sorted(summary.changed_files),
                "exit_code": summary.exit_code,
                "result": _stable_result(summary.result),
            },
            separators=(",", ":"),
            sort_keys=True,
            default=str,
        ).encode()
    ).hexdigest()


def _failure_outcome_hash(role: str, summary: Any, payload: dict[str, Any]) -> str:
    """Identity of a failed or cancelled terminal outcome. Built from the
    runner's own stable fingerprint when it sent one, otherwise from the same
    algorithm applied to the failure text -- never from the raw payload, whose
    durations and counters differ on every execution."""
    supplied = payload.get("failureFingerprint")
    core = (
        supplied.strip()
        if isinstance(supplied, str) and supplied.strip()
        else _runner_failure_fingerprint("execution", _failure_message(summary, payload))
    )
    return sha256(
        json.dumps(
            {
                "kind": "failure",
                "role": role,
                "event_type": summary.event_type,
                "exit_code": summary.exit_code,
                "fingerprint": core,
            },
            separators=(",", ":"),
            sort_keys=True,
        ).encode()
    ).hexdigest()


def _stored_outcome(value: Any) -> str | None:
    """Reads a stored outcome hash, treating the legacy empty-string encoding
    of "no diff" as the absent value it was always meant to be."""
    text = _optional_text(value)
    return text if text else None


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
