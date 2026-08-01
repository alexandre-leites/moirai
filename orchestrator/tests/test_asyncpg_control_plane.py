from __future__ import annotations

import json
import unittest
from datetime import UTC, datetime, timedelta
from hashlib import sha256
from typing import Self
from uuid import UUID, uuid4

from moirai.domain.control_plane import AuthenticationError, OfferError, RegistrationError
from moirai.domain.leases import StaleLeaseError
from moirai.domain.models import ExecutionEvent
from moirai.persistence.control_plane import AsyncpgControlPlane, _runner_failure_fingerprint
from moirai.workflows.runner_events import ROLE_TO_SUFFIX

NOW = datetime(2026, 1, 1, tzinfo=UTC)


# `accept_event` decides a terminal transition from whichever `app.workflow_runs`
# column its SELECT aliases as `workflow_phase`. Every fake that answers that
# query resolves the alias through this map instead of hard-coding the key, so a
# production query that goes back to reading `w.status` is served the *status*
# and the developer transition tests fail (issue #141).
_PHASE_ALIAS_SOURCES = {"current_phase": "workflow_phase", "status": "workflow_status"}


def _selected_phase(query: str, status: object, phase: object) -> object:
    for column, key in _PHASE_ALIAS_SOURCES.items():
        if f"w.{column} AS workflow_phase" in query:
            return phase if key == "workflow_phase" else status
    raise AssertionError(f"query aliases no app.workflow_runs column as workflow_phase: {query}")


def _assigned_literal(query: str, column: str) -> str | None:
    """The literal a `SET ... <column> = '<literal>'` statement assigns, or None
    when the statement does not assign that column at all."""
    marker = f"{column} = '"
    if marker not in query:
        return None
    return query.split(marker, 1)[1].split("'", 1)[0]


def _probe_releases(queries: list[str]) -> list[str]:
    """The statements that hand a half-open circuit back to `open` (issue #92)."""
    return [
        query
        for query in queries
        if "WHERE probe_workflow_run_id = $1 AND state = 'half_open'" in query
        and "SET state = 'open'" in query
    ]


class _Transaction:
    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None


class _Connection:
    def __init__(self, pool: _Pool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "runner_registration_tokens" not in query:
            raise AssertionError(query)
        token = self.pool.tokens.get(arguments[0])
        if token is None or token["used_at"] is not None or token["expires_at"] <= arguments[1]:
            return None
        return {"id": token["id"], "allowed_labels": token["allowed_labels"]}

    async def execute(self, query: str, *arguments: object) -> str:
        if "INSERT INTO app.runners" in query:
            self.pool.runner = {
                "id": arguments[0],
                "labels": arguments[2],
                "enabled": True,
                "draining": False,
                "status": "offline",
            }
            return "INSERT 0 1"
        if "INSERT INTO app.runner_credentials" in query:
            self.pool.credential_hash = str(arguments[2])
            return "INSERT 0 1"
        if "UPDATE app.runner_registration_tokens" in query:
            for token in self.pool.tokens.values():
                if token["id"] == arguments[1] and token["used_at"] is None:
                    token["used_at"] = arguments[0]
                    return "UPDATE 1"
            return "UPDATE 0"
        raise AssertionError(query)


class _Pool:
    def __init__(self) -> None:
        self.tokens: dict[str, dict[str, object]] = {}
        self.runner: dict[str, object] | None = None
        self.credential_hash = ""

    def acquire(self) -> _Connection:
        return _Connection(self)

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "JOIN app.runner_credentials" in query:
            if self.runner is None or str(self.runner["id"]) != str(arguments[0]):
                return None
            return {**self.runner, "credential_hash": self.credential_hash}
        if "UPDATE app.runners" in query:
            if self.runner is None or str(self.runner["id"]) != str(arguments[0]):
                return None
            self.runner["status"] = "online"
            return self.runner
        raise AssertionError(query)


class _CircuitReapConnection:
    def __init__(self, pool: _CircuitReapPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def execute(self, query: str, *arguments: object) -> str:
        self.pool.calls.append((query, arguments))
        return self.pool.tags.pop(0)


class _CircuitReapPool:
    """A pool that hands back the command tags asyncpg would return, so the
    reaper's row counting can be exercised without a database."""

    def __init__(self, tags: list[str]) -> None:
        self.tags = list(tags)
        self.calls: list[tuple[str, tuple[object, ...]]] = []

    def acquire(self) -> _CircuitReapConnection:
        return _CircuitReapConnection(self)


class _DurableConnection:
    def __init__(self, pool: _DurablePool) -> None:
        self.pool = pool
        self.transactions_opened = 0

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        self.transactions_opened += 1
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "FROM app.workflow_execution_requests AS request" in query:
            if self.pool.execution_request_status != "queued":
                return None
            return {
                "request_id": self.pool.execution_request_id,
                "job_id": self.pool.job_id,
                "workflow_run_id": self.pool.workflow_id,
                "issue_id": "00000000-0000-0000-0000-000000000004",
                "project_id": self.pool.project_id,
                "external_id": "42",
                "priority": 100,
                "external_created_at": NOW - timedelta(days=1),
                "last_synced_at": NOW - timedelta(hours=1),
                "runner_id": self.pool.runner_id,
                "labels": ["docker"],
                "runner_enabled": True,
                "draining": False,
                "status": "online",
            }
        if "SET runner_id = $2, status = 'offered', lease_generation = lease_generation + 1" in query:
            if self.pool.job_status != "completed":
                return None
            self.pool.job_status = "offered"
            self.pool.lease_expires_at = arguments[2]
            self.pool.last_event_sequence = 0
            return {"lease_generation": 2}
        if "FROM app.jobs AS j" in query and "j.status = 'recovering'" in query:
            if self.pool.job_status != "recovering":
                return None
            return {
                "job_id": self.pool.job_id,
                "workflow_run_id": self.pool.workflow_id,
                "project_id": self.pool.project_id,
                "lease_generation": 2,
                "issue_id": "00000000-0000-0000-0000-000000000004",
                "external_id": "42",
                "priority": 100,
                "external_created_at": NOW - timedelta(days=1),
                "last_synced_at": NOW - timedelta(hours=1),
                "runner_id": self.pool.recovery_runner_id,
                "labels": ["docker"],
                "runner_enabled": True,
                "draining": False,
            }
        if "SET runner_id = $2" in query:
            if self.pool.job_status != "recovering" or arguments[4] != 2:
                return None
            self.pool.job_status = "offered"
            self.pool.runner_id = str(arguments[1])
            self.pool.lease_expires_at = arguments[2]
            self.pool.last_event_sequence = 0
            return {"lease_generation": 2}
        if "SET status = 'expired'" in query:
            if self.pool.offer_status != "offered" or self.pool.offer_expires_at > arguments[0]:
                return None
            self.pool.offer_status = "expired"
            self.pool.record_unanswered_offer(arguments[0])
            return {"job_id": self.pool.job_id}
        if "AS bootstrap" in query:
            if self.pool.job_status != "offered":
                return None
            if arguments[1] is not None and str(arguments[1]) != str(self.pool.runner_id):
                return None
            return {
                "workflow_run_id": self.pool.workflow_id,
                "project_id": self.pool.project_id,
                "terminal": self.pool.workflow_status
                in {"completed", "blocked", "failed", "cancelled"},
                "request_id": (
                    self.pool.execution_request_id
                    if self.pool.execution_request_status == "dispatched"
                    else None
                ),
                "bootstrap": self.pool.is_bootstrap(query),
            }
        if "AS unanswered" in query:
            return {
                "unanswered": self.pool.unanswered_offers,
                "started_at": self.pool.unanswered_since,
            }
        if "SET status = 'recovering'" in query:
            if self.pool.job_status not in {"preparing", "running"} or self.pool.lease_expires_at > arguments[0]:
                return None
            self.pool.job_status = "recovering"
            return {
                "id": self.pool.job_id,
                "workflow_run_id": self.pool.workflow_id,
                "runner_id": self.pool.runner_id,
                "lease_generation": 2,
            }
        if "SELECT state, opened_at FROM app.project_circuit_state" in query:
            return self.pool.project_circuit
        if "SELECT state, opened_at FROM app.provider_circuit_state" in query:
            return self.pool.provider_circuit
        if "FROM app.issues AS i" in query:
            return self.pool.candidate
        if "UPDATE app.job_offers" in query:
            if self.pool.offer_status != "offered" or self.pool.offer_expires_at <= arguments[2]:
                return None
            if "SET status = 'rejected'" in query:
                self.pool.offer_status = "rejected"
                self.pool.record_unanswered_offer(arguments[2])
            else:
                self.pool.offer_status = "accepted"
                self.pool.accepted_offer = True
                self.pool.unanswered_offers = 0
                self.pool.unanswered_since = None
            return {"job_id": self.pool.job_id}
        if "SET status = 'preparing'" in query:
            if self.pool.job_status != "offered":
                return None
            self.pool.job_status = "preparing"
            return {"lease_generation": 1, "lease_expires_at": self.pool.lease_expires_at}
        if "FOR UPDATE OF j" in query:
            if self.pool.job_status not in {"preparing", "running"} or self.pool.lease_expires_at <= arguments[3]:
                return None
            if int(arguments[2]) < 1 or self.pool.last_event_sequence >= int(arguments[4]):
                return None
            return {
                "id": self.pool.job_id,
                "workflow_run_id": self.pool.workflow_id,
                "lease_generation": int(arguments[2]),
                "lease_expires_at": self.pool.lease_expires_at,
                "last_event_sequence": self.pool.last_event_sequence,
                "workflow_phase": _selected_phase(
                    query, self.pool.workflow_status, self.pool.workflow_phase
                ),
            }
        if "SET last_event_sequence" in query:
            if (
                self.pool.job_status not in {"preparing", "running"}
                or arguments[2] != 1
                or self.pool.lease_expires_at <= arguments[4]
                or self.pool.last_event_sequence >= arguments[3]
            ):
                return None
            self.pool.last_event_sequence = int(arguments[3])
            self.pool.job_status = "running"
            return {
                "workflow_run_id": self.pool.workflow_id,
                "lease_generation": 1,
                "lease_expires_at": self.pool.lease_expires_at,
                "last_event_sequence": self.pool.last_event_sequence,
            }
        if "SELECT 1 AS present FROM app.workflow_execution_requests" in query:
            return {"present": 1} if self.pool.dispatched_requests else None
        if "SELECT id, role, attempt" in query and "workflow_execution_requests" in query:
            for request_id, role, attempt in self.pool.dispatched_requests:
                if str(request_id) == str(arguments[0]):
                    return {"id": request_id, "role": role, "attempt": attempt}
            return None
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        self.pool.queries.append(query)
        if "SET state = 'half_open'" in query:
            if "provider_circuit_state" in query and self.pool.provider_claim_fails:
                # Drives the defensive `claimed != "UPDATE 1"` arm. Both rows
                # are held by `SELECT … FOR UPDATE` before either write, so
                # under READ COMMITTED no concurrent claim can produce this;
                # what can is a write the database refuses or suppresses, which
                # is what the PostgreSQL test injects with a BEFORE UPDATE
                # trigger. The arm exists so that arriving here still cannot
                # commit the claim that already succeeded.
                return "UPDATE 0"
            if "project_circuit_state" in query and self.pool.project_circuit is not None:
                self.pool.project_circuit["state"] = "half_open"
            if "provider_circuit_state" in query and self.pool.provider_circuit is not None:
                self.pool.provider_circuit["state"] = "half_open"
            return "UPDATE 1"
        if "UPDATE app.workflow_execution_requests" in query:
            if "SET status = 'queued'" in query:
                if self.pool.execution_request_status != "dispatched":
                    return "UPDATE 0"
                self.pool.execution_request_status = "queued"
                return "UPDATE 1"
            if "SET status = 'expired'" in query:
                self.pool.execution_request_status = "expired"
                return "UPDATE 1"
            if self.pool.execution_request_status != "queued":
                return "UPDATE 0"
            self.pool.execution_request_status = "dispatched"
            return "UPDATE 1"
        if "UPDATE app.jobs" in query and "recovery_reason" in query:
            self.pool.job_status = "recovering" if "SET status = 'recovering'" in query else "cancelled"
            if self.pool.job_status == "recovering":
                self.pool.lease_generation += 1
            return "UPDATE 1"
        if "UPDATE app.workflow_runs" in query and "SET status = '" in query:
            # Each column moves only if the statement actually assigns it, and
            # to the literal that statement assigns -- the two are not always
            # the same value (`recover_one` used to write 'offered'/'recovering').
            self.pool.workflow_status = _assigned_literal(query, "status")
            phase = _assigned_literal(query, "current_phase")
            if phase is not None:
                self.pool.workflow_phase = phase
            return "UPDATE 1"
        if "DELETE FROM app.project_locks" in query:
            self.pool.project_locked = False
            return "DELETE 1"
        if "app.jobs" in query and "last_event_sequence" in query:
            if self.pool.last_event_sequence >= int(arguments[1]):
                return "UPDATE 0"
            self.pool.last_event_sequence = int(arguments[1])
            if arguments[2] in {"started", "completed", "failed", "cancelled"}:
                if "CASE WHEN status = 'preparing' THEN 'running' ELSE status END" in query:
                    self.pool.job_status = "running"
                elif "finished_at" in query:
                    self.pool.job_status = str(arguments[2])
            return "UPDATE 1"
        return "INSERT 0 1"


class _DurablePool:
    def __init__(self) -> None:
        self.workflow_id = "00000000-0000-0000-0000-000000000001"
        self.project_id = "00000000-0000-0000-0000-000000000005"
        self.job_id = "00000000-0000-0000-0000-000000000002"
        self.runner_id = "00000000-0000-0000-0000-000000000003"
        self.recovery_runner_id = "00000000-0000-0000-0000-000000000006"
        self.lease_expires_at = NOW + timedelta(minutes=5)
        self.offer_expires_at = NOW + timedelta(minutes=5)
        self.offer_status = "offered"
        # Whether any offer for this job was ever accepted -- the durable fact
        # `app.job_offers` carries, which `offer_status` (the *current* offer)
        # does not.
        self.accepted_offer = False
        self.job_status = "offered"
        self.workflow_status = "planning"
        self.workflow_phase = "planning"
        self.branch_name: str | None = "moirai/issue-91"
        self.project_locked = True
        self.lease_generation = 1
        self.unanswered_offers = 0
        self.unanswered_since: datetime | None = None
        self.last_event_sequence = 0
        self.execution_request_id = "00000000-0000-0000-0000-000000000007"
        self.last_failure_fingerprint: str | None = None
        self.blocking_reason: str | None = None
        self.last_gate_verdict: str | None = None
        self.remaining_work: list[str] = []
        self.human_guidance: str | None = None
        self.current_commit: str | None = None
        self.planner_result: dict[str, object] | None = None
        self.review_result: dict[str, object] | None = None
        self.pipeline_result: dict[str, object] | None = None
        self.latest_execution_type: str | None = None
        self.latest_execution_attempt: int | None = None
        self.latest_execution_status: str | None = None
        self.latest_execution_exit_code: int | None = None
        self.latest_execution_result: dict[str, object] | None = None
        self.issue_body = "Add durable delivery."
        # A bootstrap offer by default: the run has never queued an execution
        # request, which is the one case the planner fallback packet is for.
        self.has_execution_history = False
        self.execution_request_status = "none"
        self.dispatched_requests: list[tuple[str, str, int]] = []
        self.project_circuit: dict[str, object] | None = None
        self.provider_circuit: dict[str, object] | None = None
        self.provider_claim_fails = False
        self.queries: list[str] = []
        self.candidate: dict[str, object] | None = {
            "issue_id": "00000000-0000-0000-0000-000000000004",
            "project_id": "00000000-0000-0000-0000-000000000005",
            "provider": "github",
            "external_id": "42",
            "priority": 100,
            "external_created_at": NOW - timedelta(days=1),
            "last_synced_at": NOW - timedelta(hours=1),
            "enabled": True,
            "runner_id": self.runner_id,
            "runner_enabled": True,
            "draining": False,
            "status": "online",
            "labels": ["docker"],
        }

    def record_unanswered_offer(self, now: object) -> None:
        self.unanswered_offers += 1
        if self.unanswered_since is None and isinstance(now, datetime):
            self.unanswered_since = now

    def bootstrap_run(self) -> None:
        """Shapes the pool like a run schedule() just created: no branch, no
        execution request, nothing accepted yet."""
        self.workflow_status = "offered"
        self.workflow_phase = "offered"
        self.branch_name = None
        self.execution_request_status = "none"
        self.accepted_offer = False

    def is_bootstrap(self, query: str) -> bool:
        """The bootstrap predicate, evaluated only over the guards this fake
        carries state for and only over the guards the statement actually
        contains. The real predicate also requires no pull request, no
        `app.executions` row and `total_agent_executions = 0`; those are
        covered by the PostgreSQL suite.
        """
        bootstrap = (
            self.workflow_status == "offered"
            and self.workflow_phase == "offered"
            and self.branch_name is None
            and self.execution_request_status == "none"
        )
        if "AND taken.status = 'accepted'" in query:
            bootstrap = bootstrap and not self.accepted_offer
        return bootstrap

    def acquire(self) -> _DurableConnection:
        return _DurableConnection(self)

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "FROM app.jobs AS j" in query:
            if self.job_status != "offered":
                return None
            record: dict[str, object] = {
                "job_id": self.job_id,
                "external_id": "42",
                "title": "Implement scheduler",
                "body": self.issue_body,
                "project_id": self.project_id,
                "repository_mode": "managed_clone",
                "repository_url": "https://example.test/repo.git",
                "local_repository_path": None,
                "default_branch": "main",
                "current_commit": self.current_commit,
                "last_failure_fingerprint": self.last_failure_fingerprint,
                "blocking_reason": self.blocking_reason,
                "last_gate_verdict": self.last_gate_verdict,
                "remaining_work": self.remaining_work,
                "human_guidance": self.human_guidance,
                "planner_result": self.planner_result,
                "review_result": self.review_result,
                "pipeline_result": self.pipeline_result,
                "latest_execution_type": self.latest_execution_type,
                "latest_execution_attempt": self.latest_execution_attempt,
                "latest_execution_status": self.latest_execution_status,
                "latest_execution_exit_code": self.latest_execution_exit_code,
                "latest_execution_result": self.latest_execution_result,
            }
            record["has_execution_history"] = self.has_execution_history
            if self.execution_request_status == "dispatched":
                record["execution_request_id"] = self.execution_request_id
                record["execution_role"] = "developer"
            return record
        if "SET lease_expires_at" not in query:
            raise AssertionError(query)
        if (
            self.job_status not in {"preparing", "running"}
            or arguments[2] != 1
            or self.lease_expires_at <= arguments[3]
        ):
            return None
        self.lease_expires_at = arguments[4]
        return {
            "lease_generation": 1,
            "lease_expires_at": self.lease_expires_at,
            "last_event_sequence": self.last_event_sequence,
        }


class _ProjectConnection:
    def __init__(self, pool: _ProjectPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "UPDATE app.runner_registration_tokens" in query:
            token_id = str(arguments[0])
            token = next((item for item in self.pool.tokens if str(item["id"]) == token_id), None)
            if token is None or token["used_at"] is not None or token["revoked_at"] is not None:
                return None
            token["revoked_at"] = arguments[1]
            return token
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        if "INSERT INTO app.runner_registration_tokens" in query:
            self.pool.tokens.append(
                {
                    "id": arguments[0],
                    "actor": arguments[2],
                    "allowed_labels": arguments[3],
                    "created_at": arguments[4],
                    "expires_at": arguments[5],
                    "used_at": None,
                    "revoked_at": None,
                }
            )
            return "INSERT 0 1"
        if "INSERT INTO app.audit_events" in query:
            self.pool.audits.append({"actor": str(arguments[1]), "action": arguments[2], "target": arguments[4]})
            return "INSERT 0 1"
        raise AssertionError(query)


class _ProjectPool:
    def __init__(self) -> None:
        self.projects: dict[str, dict[str, object]] = {}
        self.tokens: list[dict[str, object]] = []
        self.audits: list[dict[str, object]] = []

    def acquire(self) -> _ProjectConnection:
        return _ProjectConnection(self)

    async def execute(self, query: str, *arguments: object) -> str:
        if "INSERT INTO app.audit_events" not in query:
            raise AssertionError(query)
        self.audits.append({"actor": str(arguments[1]), "action": arguments[2], "target": arguments[4]})
        return "INSERT 0 1"

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "INSERT INTO app.projects" in query:
            record = {
                "id": arguments[0],
                "name": arguments[1],
                "enabled": True,
                "repository_mode": arguments[2],
                "repository_url": arguments[3],
                "local_repository_path": arguments[4],
                "default_branch": arguments[5],
                "configuration": arguments[6],
            }
            self.projects[str(arguments[0])] = record
            return record
        if "UPDATE app.projects" in query:
            record = self.projects.get(str(arguments[0]))
            if record is None:
                return None
            if "SET enabled" in query:
                record["enabled"] = arguments[1]
            else:
                record["name"] = arguments[1]
                record["repository_mode"] = arguments[2]
                record["repository_url"] = arguments[3]
                record["local_repository_path"] = arguments[4]
                record["default_branch"] = arguments[5]
                record["configuration"] = arguments[6]
            return record
        raise AssertionError(query)

    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        if "FROM app.projects" in query:
            return sorted(self.projects.values(), key=lambda record: str(record["name"]))
        if "FROM app.runner_registration_tokens" in query:
            return list(reversed(self.tokens))
        raise AssertionError(query)


class AsyncpgControlPlaneTests(unittest.IsolatedAsyncioTestCase):
    async def test_registration_is_single_use_and_heartbeat_requires_credential(self) -> None:
        pool = _Pool()
        token = "registration-token"
        token_hash = sha256(token.encode("utf-8")).hexdigest()
        pool.tokens[token_hash] = {
            "id": "token-id",
            "allowed_labels": ["docker"],
            "expires_at": NOW + timedelta(minutes=1),
            "used_at": None,
        }
        control_plane = AsyncpgControlPlane(pool)

        runner, credential = await control_plane.register_runner(token, "runner-a", {"docker"}, NOW)
        self.assertFalse(runner.connected)
        self.assertTrue(credential)
        with self.assertRaises(RegistrationError):
            await control_plane.register_runner(token, "runner-b", {"docker"}, NOW)
        with self.assertRaises(AuthenticationError):
            await control_plane.heartbeat(runner.id, "wrong", NOW)

        connected = await control_plane.heartbeat(runner.id, credential, NOW)
        self.assertTrue(connected.available)

    async def test_registration_rejects_labels_outside_token_scope(self) -> None:
        pool = _Pool()
        token = "registration-token"
        pool.tokens[sha256(token.encode("utf-8")).hexdigest()] = {
            "id": "token-id",
            "allowed_labels": ["docker"],
            "expires_at": NOW + timedelta(minutes=1),
            "used_at": None,
        }

        with self.assertRaises(RegistrationError):
            await AsyncpgControlPlane(pool).register_runner(token, "runner-a", {"opencode"}, NOW)

    async def test_build_task_packet_is_planner_restricted_and_repository_scoped(self) -> None:
        pool = _DurablePool()
        control_plane = AsyncpgControlPlane(pool)
        scheduled = await control_plane.schedule(NOW, timedelta(seconds=30))
        assert scheduled is not None
        packet = await control_plane.build_task_packet(scheduled)
        self.assertEqual(packet["protocolVersion"], "1.0")
        self.assertEqual(packet["role"], "planner")
        self.assertEqual(packet["repository"]["url"], "https://example.test/repo.git")
        self.assertEqual(packet["constraints"], {"mayModifyFiles": False, "mayPush": False, "mayMerge": False})

    async def test_build_task_packet_refuses_a_run_with_history_but_no_dispatched_request(self) -> None:
        """The planner fallback packet is the *bootstrap* packet. Handing it to
        a run that is mid-implementation would send `{job_id}-plan`, which
        accept_event rejects, aborting the runner's control stream on every
        retry (issue #94: the request row is now closed on its terminal event,
        so a recovery re-offer can reach this state)."""
        pool = _DurablePool()
        pool.has_execution_history = True
        control_plane = AsyncpgControlPlane(pool)
        scheduled = await control_plane.schedule(NOW, timedelta(seconds=30))
        assert scheduled is not None

        with self.assertRaises(ValueError):
            await control_plane.build_task_packet(scheduled)

    async def test_schedule_execution_claims_a_queued_developer_request_and_builds_its_packet(self) -> None:
        pool = _DurablePool()
        pool.job_status = "completed"
        pool.execution_request_status = "queued"
        control_plane = AsyncpgControlPlane(pool)

        scheduled = await control_plane.schedule_execution(NOW, timedelta(seconds=30))

        self.assertIsNotNone(scheduled)
        assert scheduled is not None
        self.assertEqual(scheduled.offer.job_id, pool.job_id)
        self.assertEqual(scheduled.offer.lease.generation, 2)
        self.assertEqual(pool.execution_request_status, "dispatched")
        packet = await control_plane.build_task_packet(scheduled)
        self.assertEqual(packet["role"], "developer")
        self.assertTrue(packet["executionId"].endswith("-implement"))
        self.assertEqual(packet["constraints"], {"mayModifyFiles": True, "mayPush": True, "mayMerge": False})
        self.assertEqual(
            [reference["name"] for reference in packet["environmentRefs"]],
            ["GITHUB_TOKEN"],
        )
        self.assertTrue(any("workflow_execution_requests" in query for query in pool.queries))

    async def test_non_delivery_evidence_is_sent_to_the_continuation_packet(self) -> None:
        pool = _DurablePool()
        pool.job_status = "completed"
        pool.execution_request_status = "queued"
        pool.last_gate_verdict = "returned without evidence: no changed files"
        pool.remaining_work = ["finish tests"]
        control_plane = AsyncpgControlPlane(pool)

        scheduled = await control_plane.schedule_execution(NOW, timedelta(seconds=30))
        assert scheduled is not None
        packet = await control_plane.build_task_packet(scheduled)

        self.assertEqual(
            packet["previousFailures"],
            ["returned without evidence: no changed files", "finish tests"],
        )

    async def test_continuation_packet_carries_planning_review_and_pipeline_context(self) -> None:
        pool = _DurablePool()
        pool.job_status = "completed"
        pool.execution_request_status = "queued"
        pool.issue_body = "\n- [ ] Preserve planner context\n- [x] Explain failures\n"
        pool.current_commit = "cafebabe"
        pool.planner_result = {
            "result": {
                "status": "ready",
                "summary": "plan",
                "assumptions": [],
                "questions": [],
                "risk": "low",
                "steps": ["Inspect workflow state", "Implement packet hydration"],
                "acceptanceCriteria": ["Keep planner criteria"],
            }
        }
        pool.review_result = {"findings": ["Handle failed commands"]}
        pool.pipeline_result = {
            "exitCode": 1,
            "pipelineResults": [{"command": "pytest -q", "exitCode": 1, "output": "old\nfailed"}],
        }
        pool.latest_execution_type = "run_developer"
        pool.latest_execution_attempt = 2
        pool.latest_execution_status = "failed"
        pool.latest_execution_exit_code = 1
        pool.latest_execution_result = {
            "changedFiles": ["orchestrator/control_plane.py"],
            "summary": "agent stopped after failing test",
        }
        pool.last_failure_fingerprint = "pipeline:deadbeef"
        control_plane = AsyncpgControlPlane(pool)

        scheduled = await control_plane.schedule_execution(NOW, timedelta(seconds=30))
        assert scheduled is not None
        packet = await control_plane.build_task_packet(scheduled)

        self.assertEqual(packet["acceptanceCriteria"], ["Preserve planner context", "Explain failures", "Keep planner criteria"])
        self.assertEqual(packet["plan"], ["Inspect workflow state", "Implement packet hydration"])
        self.assertEqual(packet["reviewFindings"], ["Handle failed commands"])
        self.assertEqual(packet["failedChecks"], ["pytest -q exited 1:\nold\nfailed"])
        self.assertEqual(packet["currentCommit"], "cafebabe")
        self.assertEqual(packet["diffSummary"], "Changed files: orchestrator/control_plane.py")
        self.assertEqual(packet["previousFailures"], ["developer attempt 2 failed (exit 1):\nagent stopped after failing test"])
        self.assertNotIn("pipeline:deadbeef", packet["previousFailures"])

    async def test_invalid_planner_result_is_not_included_in_a_continuation_packet(self) -> None:
        pool = _DurablePool()
        pool.job_status = "completed"
        pool.execution_request_status = "queued"
        pool.planner_result = {"result": {"status": "ready", "steps": ["unsafe"]}}
        control_plane = AsyncpgControlPlane(pool)

        scheduled = await control_plane.schedule_execution(NOW, timedelta(seconds=30))
        assert scheduled is not None
        packet = await control_plane.build_task_packet(scheduled)

        self.assertEqual(packet["plan"], [])
        self.assertEqual(packet["acceptanceCriteria"], ["Implement scheduler"])

    async def test_schedule_creates_an_atomic_offer_and_project_lock(self) -> None:
        pool = _DurablePool()
        scheduled = await AsyncpgControlPlane(pool).schedule(NOW, timedelta(seconds=30))

        self.assertIsNotNone(scheduled)
        assert scheduled is not None
        self.assertEqual(scheduled.assignment.issue.priority, 100)
        self.assertEqual(scheduled.assignment.runner.id, pool.runner_id)
        self.assertEqual(scheduled.offer.lease.generation, 1)
        self.assertTrue(any("INSERT INTO app.project_locks" in query for query in pool.queries))
        self.assertTrue(any("INSERT INTO app.job_offers" in query for query in pool.queries))

    async def test_expired_circuits_allow_one_half_open_probe_and_block_another(self) -> None:
        pool = _DurablePool()
        pool.project_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        pool.provider_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        control_plane = AsyncpgControlPlane(pool, circuit_probe_cooldown=timedelta(minutes=5))

        probe = await control_plane.schedule(NOW, timedelta(seconds=30))
        duplicate = await control_plane.schedule(NOW, timedelta(seconds=30))

        self.assertIsNotNone(probe)
        self.assertIsNone(duplicate)
        self.assertEqual(pool.project_circuit["state"], "half_open")
        self.assertEqual(pool.provider_circuit["state"], "half_open")
        self.assertEqual(sum("SET state = 'half_open'" in query for query in pool.queries), 2)

    async def test_unexpired_open_circuit_is_not_eligible_for_a_probe(self) -> None:
        pool = _DurablePool()
        pool.project_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=4)}
        control_plane = AsyncpgControlPlane(pool, circuit_probe_cooldown=timedelta(minutes=5))

        self.assertFalse(await control_plane._claim_circuit_probes(
            _DurableConnection(pool), pool.project_id, "github", uuid4(), NOW
        ))

    async def test_provider_probe_contention_does_not_claim_the_project_circuit(self) -> None:
        pool = _DurablePool()
        pool.project_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        pool.provider_circuit = {"state": "half_open", "opened_at": NOW - timedelta(minutes=5)}
        control_plane = AsyncpgControlPlane(pool, circuit_probe_cooldown=timedelta(minutes=5))

        self.assertFalse(await control_plane._claim_circuit_probes(
            _DurableConnection(pool), pool.project_id, "github", uuid4(), NOW
        ))
        self.assertEqual(pool.project_circuit["state"], "open")

    async def test_a_failed_second_claim_unwinds_the_claim_transaction(self) -> None:
        """Issue #92: the project claim used to be committed by the caller's
        `return None`, wedging the project at `half_open` with a probe pointing
        at a workflow run `schedule()` never inserted.

        This asserts the two halves of the mechanism a fake can observe: the
        failed claim is reported as such, and the claim ran inside its own
        nested transaction, which asyncpg issues as a SAVEPOINT because
        `schedule()` already holds the outer one. That the savepoint really
        discards the first claim is proved against PostgreSQL by
        `test_postgres_integration.py`'s `test_a_partial_probe_claim_is_never_committed`.
        """
        pool = _DurablePool()
        pool.project_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        pool.provider_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        pool.provider_claim_fails = True
        connection = _DurableConnection(pool)
        control_plane = AsyncpgControlPlane(pool, circuit_probe_cooldown=timedelta(minutes=5))

        claimed = await control_plane._claim_circuit_probes(
            connection, pool.project_id, "github", uuid4(), NOW
        )

        self.assertFalse(claimed)
        self.assertEqual(connection.transactions_opened, 1)
        self.assertEqual(sum("SET state = 'half_open'" in query for query in pool.queries), 2)

    async def test_schedule_places_no_offer_when_a_probe_claim_fails(self) -> None:
        pool = _DurablePool()
        pool.project_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        pool.provider_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        pool.provider_claim_fails = True
        control_plane = AsyncpgControlPlane(pool, circuit_probe_cooldown=timedelta(minutes=5))

        self.assertIsNone(await control_plane.schedule(NOW, timedelta(seconds=30)))

        self.assertFalse(any("INSERT INTO app.workflow_runs" in query for query in pool.queries))
        self.assertFalse(any("INSERT INTO app.job_offers" in query for query in pool.queries))

    async def test_a_stale_probe_pointer_does_not_veto_a_new_claim(self) -> None:
        """`state = 'open'` means no probe is outstanding, so a pointer still on
        the row is stale. Requiring it to be NULL made a circuit that had been
        closed and reopened impossible to probe ever again (issue #92)."""
        pool = _DurablePool()
        pool.project_circuit = {"state": "open", "opened_at": NOW - timedelta(minutes=5)}
        pool.provider_circuit = {
            "state": "open",
            "opened_at": NOW - timedelta(minutes=5),
            "probe_workflow_run_id": uuid4(),
        }
        control_plane = AsyncpgControlPlane(pool, circuit_probe_cooldown=timedelta(minutes=5))

        self.assertTrue(await control_plane._claim_circuit_probes(
            _DurableConnection(pool), pool.project_id, "github", uuid4(), NOW
        ))
        claims = [query for query in pool.queries if "SET state = 'half_open'" in query]
        self.assertEqual(len(claims), 2)
        for claim in claims:
            self.assertNotIn("probe_workflow_run_id IS NULL", claim)

    async def test_reaping_orphaned_probes_reports_each_circuit_table(self) -> None:
        pool = _CircuitReapPool(["UPDATE 2", "UPDATE 0"])
        control_plane = AsyncpgControlPlane(pool, circuit_probe_cooldown=timedelta(minutes=5))

        reaped = await control_plane.reap_orphaned_circuit_probes(NOW)

        self.assertEqual(reaped, {"project_circuits": 2, "provider_circuits": 0})
        self.assertIn("app.project_circuit_state", pool.calls[0][0])
        self.assertIn("app.provider_circuit_state", pool.calls[1][0])
        for query, arguments in pool.calls:
            self.assertIn("WHERE state = 'half_open'", query)
            self.assertIn("probe.status NOT IN ('completed', 'blocked', 'failed', 'cancelled')", query)
            self.assertEqual(arguments, (NOW, NOW - timedelta(minutes=5)))

    async def test_accept_renew_and_fence_events_for_a_durable_job(self) -> None:
        pool = _DurablePool()
        control_plane = AsyncpgControlPlane(pool)
        lease = await control_plane.accept_offer(pool.job_id, pool.runner_id, NOW)
        self.assertEqual(lease.generation, 1)

        renewed = await control_plane.renew_lease(
            pool.job_id, pool.runner_id, 1, NOW + timedelta(minutes=10), NOW
        )
        self.assertEqual(renewed.expires_at, NOW + timedelta(minutes=10))
        event_lease = await control_plane.accept_event(
            ExecutionEvent(
                job_id=pool.job_id,
                runner_id=pool.runner_id,
                lease_generation=1,
                event_sequence=1,
                event_type="started",
                execution_id=f"{pool.job_id}-plan",
                payload={"status": "running"},
            ),
            NOW,
        )
        self.assertEqual(event_lease.last_event_sequence, 1)
        with self.assertRaises(StaleLeaseError):
            await control_plane.accept_event(
                ExecutionEvent(
                    job_id=pool.job_id,
                    runner_id=pool.runner_id,
                    lease_generation=1,
                    event_sequence=1,
                    event_type="started",
                    execution_id=f"{pool.job_id}-plan",
                    payload={"status": "running"},
                ),
                NOW,
            )

    async def test_expire_leases_fences_job_and_preserves_project_lock(self) -> None:
        pool = _DurablePool()
        pool.job_status = "preparing"
        pool.lease_expires_at = NOW
        expired = await AsyncpgControlPlane(pool).expire_leases(NOW)
        self.assertEqual(expired, (pool.job_id,))
        self.assertEqual(pool.job_status, "recovering")
        self.assertFalse(any("SET status = 'offline'" in query for query in pool.queries))
        self.assertFalse(any("DELETE FROM app.project_locks" in query for query in pool.queries))

    async def test_expire_leases_preserves_the_phase_of_the_run_it_fences(self) -> None:
        """Issue #141: the recovery re-offer hands the *same* dispatched
        request back to a runner, so the terminal event it produces still has
        to know which phase queued it."""
        pool = _DurablePool()
        pool.job_status = "preparing"
        pool.workflow_status = "preparing"
        pool.workflow_phase = "implementing"
        pool.lease_expires_at = NOW

        self.assertEqual(await AsyncpgControlPlane(pool).expire_leases(NOW), (pool.job_id,))

        self.assertEqual(pool.workflow_status, "recovering")
        self.assertEqual(pool.workflow_phase, "implementing")

    async def test_recovery_reoffers_one_fenced_job_without_releasing_project_lock(self) -> None:
        pool = _DurablePool()
        pool.job_status = "recovering"
        pool.lease_expires_at = NOW
        previous_runner_id = pool.runner_id
        control_plane = AsyncpgControlPlane(pool)
        recovered = await control_plane.recover_one(NOW, timedelta(seconds=30))

        self.assertIsNotNone(recovered)
        assert recovered is not None
        self.assertEqual(recovered.offer.job_id, pool.job_id)
        self.assertEqual(recovered.offer.runner_id, pool.recovery_runner_id)
        self.assertEqual(recovered.offer.lease.generation, 2)
        self.assertEqual(pool.job_status, "offered")
        self.assertEqual(pool.last_event_sequence, 0)
        self.assertFalse(any("DELETE FROM app.project_locks" in query for query in pool.queries))
        self.assertTrue(any("lease_recovery_offered" in query for query in pool.queries))
        self.assertIsNone(await control_plane.recover_one(NOW, timedelta(seconds=30)))
        with self.assertRaises(StaleLeaseError):
            await control_plane.accept_event(
                ExecutionEvent(
                    job_id=pool.job_id,
                    runner_id=previous_runner_id,
                    lease_generation=1,
                    event_sequence=1,
                    event_type="started",
                    execution_id=f"{pool.job_id}-plan",
                ),
                NOW,
            )

    async def test_recovery_reoffer_preserves_the_phase_it_is_recovering(self) -> None:
        """The re-offer only makes the run schedulable again (`status`); which
        phase it is in is still the graph's, so a developer execution recovered
        from a lease expiry can still transition when it reports (issue #141)."""
        pool = _DurablePool()
        pool.job_status = "recovering"
        pool.workflow_status = "recovering"
        pool.workflow_phase = "implementing"
        pool.lease_expires_at = NOW

        recovered = await AsyncpgControlPlane(pool).recover_one(NOW, timedelta(seconds=30))

        self.assertIsNotNone(recovered)
        self.assertEqual(pool.workflow_status, "offered")
        self.assertEqual(pool.workflow_phase, "implementing")

    async def test_expire_offers_cancels_a_bootstrap_job_and_releases_project_lock(self) -> None:
        pool = _DurablePool()
        pool.bootstrap_run()
        pool.offer_expires_at = NOW
        expired = await AsyncpgControlPlane(pool).expire_offers(NOW)
        self.assertEqual(expired, (pool.job_id,))
        self.assertEqual(pool.offer_status, "expired")
        self.assertEqual(pool.job_status, "cancelled")
        self.assertEqual(pool.workflow_status, "cancelled")
        self.assertFalse(pool.project_locked)

    async def test_expire_offers_never_cancels_a_run_a_runner_already_accepted(self) -> None:
        """Issue #141: once `accept_offer` stopped overwriting `current_phase`,
        the phase alone no longer said "no runner has ever taken this job", and
        a run whose first offer was accepted by a runner that then died
        silently matched the bootstrap arm. Cancelling it releases the project
        lock and leaves the issue eligible, so `schedule()` builds a fresh run
        with a fresh job -- and the unanswered-offer streak is per job, so the
        limit that is supposed to stop the churn resets every cycle."""
        pool = _DurablePool()
        pool.bootstrap_run()
        await AsyncpgControlPlane(pool).accept_offer(pool.job_id, pool.runner_id, NOW)
        # The runner reported nothing at all, so its lease expires and the job
        # is fenced and re-offered.
        pool.lease_expires_at = NOW
        await AsyncpgControlPlane(pool).expire_leases(NOW)
        pool.job_status = "offered"
        pool.workflow_status = "offered"
        pool.offer_status = "offered"
        pool.offer_expires_at = NOW

        self.assertEqual(await AsyncpgControlPlane(pool).expire_offers(NOW), (pool.job_id,))

        self.assertNotEqual(pool.workflow_status, "cancelled")
        self.assertEqual(pool.workflow_status, "recovering")
        self.assertTrue(pool.project_locked)
        self.assertTrue(any("offer_unanswered" in query for query in pool.queries))

    async def test_expire_offers_requeues_an_in_flight_run(self) -> None:
        """An unanswered re-offer must not cancel a run that already has work
        (issue #91): the execution request goes back to the queue and the
        project lock stays with the run."""
        pool = _DurablePool()
        pool.execution_request_status = "dispatched"
        pool.offer_expires_at = NOW
        expired = await AsyncpgControlPlane(pool).expire_offers(NOW)
        self.assertEqual(expired, (pool.job_id,))
        self.assertEqual(pool.workflow_status, "planning")
        self.assertEqual(pool.execution_request_status, "queued")
        self.assertEqual(pool.job_status, "cancelled")
        self.assertTrue(pool.project_locked)
        self.assertTrue(any("offer_unanswered" in query for query in pool.queries))

    async def test_expire_offers_returns_a_recovery_offer_to_recovering(self) -> None:
        pool = _DurablePool()
        pool.workflow_status = "offered"
        pool.workflow_phase = "recovering"
        pool.offer_expires_at = NOW
        expired = await AsyncpgControlPlane(pool).expire_offers(NOW)
        self.assertEqual(expired, (pool.job_id,))
        self.assertEqual(pool.job_status, "recovering")
        self.assertEqual(pool.lease_generation, 2)
        self.assertEqual(pool.workflow_status, "recovering")
        self.assertTrue(pool.project_locked)

    async def test_expire_offers_does_not_resurrect_a_terminal_run(self) -> None:
        pool = _DurablePool()
        pool.workflow_status = "completed"
        pool.workflow_phase = "completed"
        pool.execution_request_status = "dispatched"
        pool.offer_expires_at = NOW
        expired = await AsyncpgControlPlane(pool).expire_offers(NOW)
        self.assertEqual(expired, (pool.job_id,))
        self.assertEqual(pool.workflow_status, "completed")
        self.assertEqual(pool.job_status, "cancelled")
        self.assertEqual(pool.execution_request_status, "dispatched")
        self.assertFalse(pool.project_locked)

    async def test_expire_offers_blocks_a_run_that_never_answers(self) -> None:
        pool = _DurablePool()
        pool.execution_request_status = "dispatched"
        pool.offer_expires_at = NOW
        pool.unanswered_offers = 4
        pool.unanswered_since = NOW - timedelta(hours=1)
        control_plane = AsyncpgControlPlane(pool, unanswered_offer_limit=5)
        self.assertEqual(await control_plane.expire_offers(NOW), (pool.job_id,))
        self.assertEqual(pool.workflow_status, "blocked")
        self.assertEqual(pool.execution_request_status, "expired")
        self.assertFalse(pool.project_locked)

    async def test_expire_offers_keeps_retrying_inside_the_grace_window(self) -> None:
        pool = _DurablePool()
        pool.execution_request_status = "dispatched"
        pool.offer_expires_at = NOW
        pool.unanswered_offers = 9
        pool.unanswered_since = NOW - timedelta(minutes=1)
        control_plane = AsyncpgControlPlane(
            pool, unanswered_offer_limit=5, unanswered_offer_grace=timedelta(minutes=15)
        )
        self.assertEqual(await control_plane.expire_offers(NOW), (pool.job_id,))
        self.assertEqual(pool.workflow_status, "planning")
        self.assertEqual(pool.execution_request_status, "queued")
        self.assertTrue(pool.project_locked)

    async def test_offer_release_paths_that_end_a_run_release_its_probe(self) -> None:
        """Issue #92, wedge 2: these two paths take a run terminal in raw SQL,
        never reaching AsyncpgWorkflowPersistence.transition -- and the run
        offer expiry cancels is typically the probe `schedule()` just claimed."""
        cancelled = _DurablePool()
        cancelled.bootstrap_run()
        cancelled.offer_expires_at = NOW
        await AsyncpgControlPlane(cancelled).expire_offers(NOW)

        blocked = _DurablePool()
        blocked.execution_request_status = "dispatched"
        blocked.offer_expires_at = NOW
        blocked.unanswered_offers = 4
        blocked.unanswered_since = NOW - timedelta(hours=1)
        await AsyncpgControlPlane(blocked, unanswered_offer_limit=5).expire_offers(NOW)

        for pool in (cancelled, blocked):
            released = _probe_releases(pool.queries)
            self.assertEqual(len(released), 2)
            self.assertTrue(any("app.project_circuit_state" in query for query in released))
            self.assertTrue(any("app.provider_circuit_state" in query for query in released))

    async def test_releasing_an_offer_for_an_already_terminal_run_leaves_circuits_alone(self) -> None:
        """That run resolved its own circuits when it transitioned; touching
        them again could reopen a circuit its success had closed."""
        pool = _DurablePool()
        pool.workflow_status = "completed"
        pool.workflow_phase = "completed"
        pool.offer_expires_at = NOW

        await AsyncpgControlPlane(pool).expire_offers(NOW)

        self.assertEqual(_probe_releases(pool.queries), [])

    async def test_accept_offer_rejects_an_expired_durable_offer(self) -> None:
        pool = _DurablePool()
        pool.offer_expires_at = NOW
        with self.assertRaises(OfferError):
            await AsyncpgControlPlane(pool).accept_offer(pool.job_id, pool.runner_id, NOW)

    async def test_accept_offer_records_readiness_without_clobbering_the_phase(self) -> None:
        """Issue #141: acceptance is a fact about the *job*, so it may only
        move `app.workflow_runs.status`. `current_phase` belongs to the graph,
        and it is what tells a terminal developer event whether the run is
        coming out of `implement` or out of `push`; stamping it `preparing`
        here is what made a successful developer execution produce no
        transition at all."""
        pool = _DurablePool()
        pool.workflow_status = "implementing"
        pool.workflow_phase = "implementing"

        await AsyncpgControlPlane(pool).accept_offer(pool.job_id, pool.runner_id, NOW)

        self.assertEqual(pool.job_status, "preparing")
        self.assertEqual(pool.workflow_status, "preparing")
        self.assertEqual(pool.workflow_phase, "implementing")

    async def test_reject_offer_cancels_a_bootstrap_job_and_releases_the_lock(self) -> None:
        pool = _DurablePool()
        pool.bootstrap_run()
        await AsyncpgControlPlane(pool).reject_offer(pool.job_id, pool.runner_id, NOW)
        self.assertEqual(pool.offer_status, "rejected")
        self.assertEqual(pool.job_status, "cancelled")
        self.assertEqual(pool.workflow_status, "cancelled")
        self.assertFalse(pool.project_locked)

    async def test_reject_offer_requeues_an_in_flight_run(self) -> None:
        pool = _DurablePool()
        pool.execution_request_status = "dispatched"
        await AsyncpgControlPlane(pool).reject_offer(pool.job_id, pool.runner_id, NOW)
        self.assertEqual(pool.offer_status, "rejected")
        self.assertEqual(pool.workflow_status, "planning")
        self.assertEqual(pool.execution_request_status, "queued")
        self.assertTrue(pool.project_locked)

    async def test_reject_offer_rejects_a_runner_that_does_not_own_the_job(self) -> None:
        pool = _DurablePool()
        pool.job_status = "preparing"
        with self.assertRaises(OfferError):
            await AsyncpgControlPlane(pool).reject_offer(pool.job_id, pool.runner_id, NOW)

    async def test_project_creation_listing_and_disable_are_durable(self) -> None:
        pool = _ProjectPool()
        control_plane = AsyncpgControlPlane(pool)
        created = await control_plane.create_project(
            "Example",
            "managed_clone",
            "https://example.test/repo.git",
            None,
            "main",
            {"linux", "docker"},
            NOW,
            "00000000-0000-0000-0000-000000000099",
        )
        self.assertTrue(created["enabled"])
        self.assertEqual(created["repository_mode"], "managed_clone")
        self.assertEqual(created["repository_url"], "https://example.test/repo.git")
        self.assertEqual(created["default_branch"], "main")
        self.assertEqual(sorted(created["required_runner_labels"]), ["docker", "linux"])
        listed = (await control_plane.list_projects())[0]
        self.assertEqual(listed["name"], "Example")
        self.assertEqual(listed["repository_mode"], "managed_clone")
        self.assertEqual(listed["required_runner_labels"], ["docker", "linux"])
        updated = await control_plane.update_project(
            str(created["id"]),
            "Renamed",
            "existing_path",
            None,
            "/repositories/example",
            "trunk",
            {"linux"},
            NOW,
            "00000000-0000-0000-0000-000000000099",
        )
        self.assertEqual(updated["name"], "Renamed")
        self.assertEqual(updated["repository_mode"], "existing_path")
        self.assertEqual(updated["repository_url"], None)
        self.assertEqual(updated["local_repository_path"], "/repositories/example")
        self.assertEqual(updated["default_branch"], "trunk")
        self.assertEqual(updated["required_runner_labels"], ["linux"])
        disabled = await control_plane.set_project_enabled(
            str(created["id"]), False, NOW, "00000000-0000-0000-0000-000000000099"
        )
        self.assertFalse(disabled["enabled"])
        self.assertEqual(
            [record["action"] for record in pool.audits],
            ["project.create", "project.update", "project.disable"],
        )

    async def test_registration_token_records_actor_and_audit_event(self) -> None:
        pool = _ProjectPool()
        actor_id = "00000000-0000-0000-0000-000000000099"
        token = await AsyncpgControlPlane(pool).create_registration_token(
            {"docker", "linux"}, NOW + timedelta(minutes=15), actor_id, NOW
        )
        self.assertTrue(token)
        self.assertEqual(len(pool.tokens), 1)
        self.assertEqual(str(pool.tokens[0]["actor"]), actor_id)
        self.assertEqual(pool.audits, [{"actor": actor_id, "action": "runner.registration_token.create", "target": str(pool.tokens[0]["id"])}])
        listed = await AsyncpgControlPlane(pool).list_registration_tokens()
        self.assertEqual(listed[0]["allowed_labels"], ["docker", "linux"])
        revoked = await AsyncpgControlPlane(pool).revoke_registration_token(
            str(pool.tokens[0]["id"]), actor_id, NOW
        )
        self.assertEqual(revoked["id"], str(pool.tokens[0]["id"]))
        self.assertEqual(len(pool.audits), 2)
        self.assertEqual(pool.audits[1]["action"], "runner.registration_token.revoke")
        with self.assertRaises(ValueError):
            await AsyncpgControlPlane(pool).revoke_registration_token(
                str(pool.tokens[0]["id"]), actor_id, NOW
            )

    async def test_project_creation_rejects_invalid_repository_sources(self) -> None:
        control_plane = AsyncpgControlPlane(_ProjectPool())
        with self.assertRaises(ValueError):
            await control_plane.create_project(
                "Example", "existing_path", None, "relative/repo", "main", {"linux"}, NOW
            )
        with self.assertRaises(ValueError):
            await control_plane.create_project(
                "Example", "managed_clone", None, "/repo", "main", {"linux"}, NOW
            )

    def test_pool_property_exposes_the_underlying_pool(self) -> None:
        pool = _ProjectPool()
        control_plane = AsyncpgControlPlane(pool)
        self.assertIs(control_plane.pool, pool)


class _HumanDecisionConnection:
    def __init__(self, pool: _HumanDecisionPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        if "FROM app.workflow_runs" in query:
            workflow = self.pool.workflows.get(str(arguments[0]))
            if workflow is None or workflow["status"] != "waiting_human":
                return None
            return dict(workflow)
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        if "INSERT INTO app.human_approvals" in query:
            self.pool.approvals.append(arguments)
            return "INSERT 0 1"
        if "INSERT INTO app.audit_events" in query:
            self.pool.audits.append(arguments)
            return "INSERT 0 1"
        raise AssertionError(query)


class _HumanDecisionPool:
    def __init__(self) -> None:
        self.workflows: dict[str, dict[str, object]] = {}
        self.approvals: list[tuple[object, ...]] = []
        self.audits: list[tuple[object, ...]] = []

    def acquire(self) -> _HumanDecisionConnection:
        return _HumanDecisionConnection(self)


class RecordHumanDecisionTests(unittest.IsolatedAsyncioTestCase):
    _WORKFLOW_ID = "00000000-0000-0000-0000-000000000001"
    _PROJECT_ID = "00000000-0000-0000-0000-000000000002"
    _ACTOR_ID = "00000000-0000-0000-0000-000000000099"

    def _control_plane(self, status: str = "waiting_human") -> tuple[AsyncpgControlPlane, _HumanDecisionPool]:
        pool = _HumanDecisionPool()
        pool.workflows[self._WORKFLOW_ID] = {
            "id": self._WORKFLOW_ID, "project_id": self._PROJECT_ID, "status": status,
        }
        return AsyncpgControlPlane(pool), pool

    async def test_records_the_decision_and_an_audit_event(self) -> None:
        control_plane, pool = self._control_plane()
        result = await control_plane.record_human_decision(
            self._WORKFLOW_ID, "approved", "looks good", self._ACTOR_ID, NOW
        )
        self.assertEqual(result["id"], self._WORKFLOW_ID)
        self.assertEqual(result["status"], "waiting_human")
        self.assertEqual(len(pool.approvals), 1)
        self.assertEqual(pool.approvals[0][3], "approved")
        self.assertEqual(len(pool.audits), 1)
        self.assertEqual(pool.audits[0][2], "workflow.human_decision.approved")

    async def test_rejects_an_unknown_decision_value(self) -> None:
        control_plane, _ = self._control_plane()
        with self.assertRaises(ValueError):
            await control_plane.record_human_decision(self._WORKFLOW_ID, "maybe", None, self._ACTOR_ID, NOW)

    async def test_requires_an_authenticated_actor(self) -> None:
        control_plane, _ = self._control_plane()
        with self.assertRaises(ValueError):
            await control_plane.record_human_decision(self._WORKFLOW_ID, "approved", None, None, NOW)

    async def test_rejects_a_workflow_not_awaiting_approval(self) -> None:
        control_plane, _ = self._control_plane(status="merging")
        with self.assertRaises(ValueError):
            await control_plane.record_human_decision(self._WORKFLOW_ID, "approved", None, self._ACTOR_ID, NOW)


class _EventConnection:
    def __init__(self, pool: _EventPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        self.pool.calls.append((query, arguments))
        if "FROM app.jobs AS j" in query and "JOIN app.workflow_runs" in query:
            if self.pool.job_row is None:
                return None
            row = dict(self.pool.job_row)
            row["workflow_phase"] = _selected_phase(
                query, row.pop("workflow_status"), row["workflow_phase"]
            )
            return row
        if "SELECT 1 AS present FROM app.workflow_execution_requests" in query:
            return {"present": 1} if self.pool.has_existing_requests else None
        if "SELECT id, role, attempt" in query and "workflow_execution_requests" in query:
            request = self.pool.dispatched.get(str(arguments[0]))
            return None if request is None else {"id": arguments[0], **request}
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        self.pool.calls.append((query, arguments))
        return "INSERT 0 1"


class _EventPool:
    def __init__(self) -> None:
        self.calls: list[tuple[str, tuple[object, ...]]] = []
        self.job_row: dict[str, object] | None = {
            "id": "00000000-0000-0000-0000-000000000002",
            "workflow_run_id": "00000000-0000-0000-0000-000000000001",
            "lease_generation": 1,
            "lease_expires_at": NOW + timedelta(minutes=5),
            "last_event_sequence": 0,
            # What `accept_offer` leaves behind: the run's *status* is the job
            # lifecycle's, the *phase* is the graph's. Only the phase decides a
            # transition (issue #141).
            "workflow_status": "preparing",
            "workflow_phase": "ai_review",
        }
        self.has_existing_requests = True
        self.dispatched: dict[str, dict[str, object]] = {}
        # Whether this process wins the outbox row's processing lease; False
        # models a background drain having claimed it first.
        self.outbox_claimed = True

    def acquire(self) -> _EventConnection:
        return _EventConnection(self)

    async def execute(self, query: str, *arguments: object) -> str:
        self.calls.append((query, arguments))
        return "INSERT 0 1"

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        self.calls.append((query, arguments))
        return {"id": arguments[0]} if self.outbox_claimed else None


class AcceptEventRoleResolutionTests(unittest.IsolatedAsyncioTestCase):
    REQUEST_ID = "00000000-0000-0000-0000-0000000000aa"
    RUNNER_ID = "00000000-0000-0000-0000-000000000003"

    def _event(self, execution_id: str, event_type: str, payload: dict[str, object] | None = None) -> ExecutionEvent:
        return ExecutionEvent(
            job_id="00000000-0000-0000-0000-000000000002",
            runner_id=self.RUNNER_ID,
            lease_generation=1,
            event_sequence=1,
            event_type=event_type,
            execution_id=execution_id,
            payload=payload or {},
        )

    async def test_terminal_event_with_undispatched_execution_id_is_rejected(self) -> None:
        pool = _EventPool()
        pool.dispatched = {}
        control_plane = AsyncpgControlPlane(pool)
        with self.assertRaises(StaleLeaseError):
            await control_plane.accept_event(
                self._event(f"{self.REQUEST_ID}-review", "completed", {"result": {"verdict": "approved"}}),
                NOW,
            )
        self.assertFalse(any("INSERT INTO app.ai_reviews" in query for query, _ in pool.calls))

    async def test_terminal_event_resolves_role_from_dispatched_request_not_suffix(self) -> None:
        """A dispatched request whose actual role differs from the execution
        ID's suffix must be trusted over the (attacker-controlled) suffix."""
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 3}}
        control_plane = AsyncpgControlPlane(pool)
        # Suffix claims "-implement" (developer) but the dispatched row says reviewer.
        lease = await control_plane.accept_event(
            self._event(f"{self.REQUEST_ID}-implement", "completed", {"result": {"verdict": "approved"}}),
            NOW,
        )
        self.assertIsNotNone(lease)
        executions_update = next(
            (args for query, args in pool.calls if "UPDATE app.executions" in query),
            None,
        )
        self.assertIsNotNone(executions_update)
        assert executions_update is not None
        self.assertEqual(executions_update[1], "run_reviewer")

    async def test_started_and_terminal_events_use_the_dispatched_attempt_not_event_sequence(self) -> None:
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 3}}
        control_plane = AsyncpgControlPlane(pool)

        started_event = self._event(f"{self.REQUEST_ID}-review", "started", {"status": "running"})
        started_event = ExecutionEvent(
            job_id=started_event.job_id,
            runner_id=started_event.runner_id,
            lease_generation=1,
            event_sequence=7,
            event_type="started",
            execution_id=started_event.execution_id,
            payload={"status": "running"},
        )
        await control_plane.accept_event(started_event, NOW)
        insert_call = next(args for query, args in pool.calls if "INSERT INTO app.executions" in query)
        # attempt (index 2) must be the dispatched attempt (3), not event_sequence (7).
        self.assertEqual(insert_call[2], 3)

        assert pool.job_row is not None
        pool.job_row["last_event_sequence"] = 0
        terminal_event = ExecutionEvent(
            job_id=started_event.job_id,
            runner_id=started_event.runner_id,
            lease_generation=1,
            event_sequence=9,
            event_type="completed",
            execution_id=started_event.execution_id,
            payload={"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
        )
        await control_plane.accept_event(terminal_event, NOW)
        update_call = next(
            args for query, args in pool.calls
            if "UPDATE app.executions" in query and "attempt = $8" in query
        )
        self.assertEqual(update_call[7], 3)

    async def test_terminal_planner_result_is_persisted_without_a_started_event(self) -> None:
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "planner", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)
        plan = {
            "status": "ready",
            "summary": "plan",
            "assumptions": [],
            "questions": [],
            "risk": "low",
            "acceptanceCriteria": ["ship context"],
            "steps": ["persist result"],
        }

        await control_plane.accept_event(
            self._event(f"{self.REQUEST_ID}-plan", "completed", {"result": plan}),
            NOW,
        )

        insert = next(args for query, args in pool.calls if "INSERT INTO app.executions" in query and "finished_at" in query)
        self.assertEqual(json.loads(str(insert[5]))["result"], plan)

    async def test_terminal_final_revision_updates_workflow_current_commit(self) -> None:
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "developer", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)

        await control_plane.accept_event(
            self._event(
                f"{self.REQUEST_ID}-implement",
                "completed",
                {"finalRevision": "cafebabe", "changedFiles": ["src/packet.py"], "result": {"status": "completed"}},
            ),
            NOW,
        )

        commit_update = next(
            (args for query, args in pool.calls if "SET current_commit = $2" in query),
            None,
        )
        self.assertEqual(commit_update, (pool.job_row["workflow_run_id"], "cafebabe", NOW))

    async def test_reviewer_approval_persists_ai_review_and_outbox_entry(self) -> None:
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)
        transitions: list[tuple[str, str, dict[str, object]]] = []

        async def on_transition(workflow_run_id: str, new_status: str, updates: dict[str, object]) -> None:
            transitions.append((workflow_run_id, new_status, updates))

        await control_plane.accept_event(
            self._event(
                f"{self.REQUEST_ID}-review",
                "completed",
                {"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
            ),
            NOW,
            on_transition=on_transition,
        )
        self.assertTrue(any("INSERT INTO app.ai_reviews" in query for query, _ in pool.calls))
        self.assertTrue(any("INSERT INTO app.workflow_transition_outbox" in query for query, _ in pool.calls))
        self.assertEqual(transitions, [("00000000-0000-0000-0000-000000000001", "pushing", {"status": "pushing", "review_approved": True, "awaiting_execution": False})])
        self.assertTrue(any("status = 'processed'" in query for query, _ in pool.calls))

    async def test_outbox_entry_stays_pending_when_on_transition_raises(self) -> None:
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)

        async def failing_on_transition(*_args: object) -> None:
            raise RuntimeError("graph runtime unavailable")

        await control_plane.accept_event(
            self._event(
                f"{self.REQUEST_ID}-review",
                "completed",
                {"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
            ),
            NOW,
            on_transition=failing_on_transition,
        )
        self.assertTrue(any("attempts = attempts + 1" in query for query, _ in pool.calls))
        self.assertFalse(any("status = 'processed'" in query for query, _ in pool.calls))

    async def test_the_inline_drain_takes_the_processing_lease_before_delivering(self) -> None:
        """Both drainers claim through the same lease, so a maintenance tick
        landing between accept_event's commit and this delivery cannot deliver
        the same transition a second time."""
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)
        delivered: list[str] = []

        async def on_transition(workflow_run_id: str, *_args: object) -> None:
            delivered.append(workflow_run_id)

        await control_plane.accept_event(
            self._event(
                f"{self.REQUEST_ID}-review",
                "completed",
                {"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
            ),
            NOW,
            on_transition=on_transition,
        )

        self.assertEqual(len(delivered), 1)
        claim = next(
            (query, arguments) for query, arguments in pool.calls
            if "SET status = 'processing'" in query
        )
        self.assertIn("WHERE id = $1 AND status = 'pending'", claim[0])
        self.assertEqual(claim[1][1], NOW)
        self.assertTrue(any("status = 'processed'" in query for query, _ in pool.calls))

    async def test_the_inline_drain_delivers_nothing_when_it_loses_the_claim(self) -> None:
        """Another drainer already holds the row. Delivering anyway would be the
        duplicate delivery the lease exists to prevent, and marking the row
        processed would cut that drainer's own delivery short."""
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
        pool.outbox_claimed = False
        control_plane = AsyncpgControlPlane(pool)
        delivered: list[str] = []

        async def on_transition(workflow_run_id: str, *_args: object) -> None:
            delivered.append(workflow_run_id)

        await control_plane.accept_event(
            self._event(
                f"{self.REQUEST_ID}-review",
                "completed",
                {"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
            ),
            NOW,
            on_transition=on_transition,
        )

        self.assertEqual(delivered, [])
        self.assertFalse(any("status = 'processed'" in query for query, _ in pool.calls))
        self.assertFalse(any("attempts = attempts + 1" in query for query, _ in pool.calls))

    async def test_completing_an_outbox_row_is_fenced_on_the_claim_it_took(self) -> None:
        """A delivery that outlives its own lease has had the row reclaimed by
        another drainer. Completing or releasing it on `id` alone would knock
        the current holder's claim out from under it and hand the same
        transition to a third drainer."""
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)

        async def on_transition(*_args: object) -> None:
            return None

        await control_plane.accept_event(
            self._event(
                f"{self.REQUEST_ID}-review",
                "completed",
                {"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
            ),
            NOW,
            on_transition=on_transition,
        )

        complete, arguments = next(
            (query, arguments) for query, arguments in pool.calls
            if "status = 'processed'" in query
        )
        self.assertIn("AND status = 'processing' AND processing_started_at = $2", complete)
        self.assertEqual(arguments[1], NOW)

    async def test_releasing_a_failed_outbox_row_is_fenced_on_the_claim_it_took(self) -> None:
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)

        async def failing(*_args: object) -> None:
            raise RuntimeError("graph runtime unavailable")

        await control_plane.accept_event(
            self._event(
                f"{self.REQUEST_ID}-review",
                "completed",
                {"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
            ),
            NOW,
            on_transition=failing,
        )

        release, arguments = next(
            (query, arguments) for query, arguments in pool.calls
            if "attempts = attempts + 1" in query
        )
        self.assertIn("AND status = 'processing' AND processing_started_at = $2", release)
        self.assertEqual(arguments[1], NOW)

    def _request_status_writes(self, pool: _EventPool) -> list[tuple[object, ...]]:
        return [
            arguments
            for query, arguments in pool.calls
            if "UPDATE app.workflow_execution_requests" in query and "status = $2" in query
        ]

    async def test_terminal_event_closes_its_execution_request(self) -> None:
        """Issue #94: the request row stayed `dispatched` forever, which kept
        the run permanently outside find_stalled_workflow_runs' predicate."""
        for event_type, payload in (
            ("completed", {"result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}}),
            ("failed", {"exitCode": 1}),
            ("cancelled", {}),
        ):
            with self.subTest(event_type=event_type):
                pool = _EventPool()
                pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
                control_plane = AsyncpgControlPlane(pool)

                await control_plane.accept_event(
                    self._event(f"{self.REQUEST_ID}-review", event_type, payload), NOW
                )

                self.assertEqual(
                    self._request_status_writes(pool), [(UUID(self.REQUEST_ID), event_type)]
                )

    async def test_non_terminal_event_leaves_its_execution_request_open(self) -> None:
        pool = _EventPool()
        pool.dispatched = {self.REQUEST_ID: {"role": "reviewer", "attempt": 1}}
        control_plane = AsyncpgControlPlane(pool)

        await control_plane.accept_event(
            self._event(f"{self.REQUEST_ID}-review", "started", {"status": "running"}), NOW
        )

        self.assertEqual(self._request_status_writes(pool), [])

    async def _transition_for_developer_event(self, phase: str) -> list[tuple[str, str, dict[str, object]]]:
        pool = _EventPool()
        assert pool.job_row is not None
        # Exactly the row `accept_offer` leaves behind: the run is `preparing`
        # for the whole life of the execution, and the phase underneath it is
        # the one the dispatching node committed.
        pool.job_row["workflow_status"] = "preparing"
        pool.job_row["workflow_phase"] = phase
        pool.dispatched = {self.REQUEST_ID: {"role": "developer", "attempt": 1}}
        transitions: list[tuple[str, str, dict[str, object]]] = []

        async def on_transition(workflow_run_id: str, new_status: str, updates: dict[str, object]) -> None:
            transitions.append((workflow_run_id, new_status, dict(updates)))

        await AsyncpgControlPlane(pool).accept_event(
            self._event(
                f"{self.REQUEST_ID}-implement", "completed",
                {
                    "exitCode": 0,
                    "changedFiles": ["a.py"],
                    "result": {
                        "protocolVersion": "1.0", "executionId": f"{self.REQUEST_ID}-implement",
                        "status": "completed", "summary": "implemented", "changedFiles": ["a.py"],
                        "commandsRun": [], "remainingWork": [], "knownLimitations": [],
                    },
                },
            ),
            NOW,
            on_transition=on_transition,
        )
        return transitions

    async def test_successful_developer_event_advances_the_implementing_phase(self) -> None:
        """Issue #141: `accept_offer` stamps the run `preparing`, so the run's
        *status* can never read `implementing` by the time the developer's
        terminal event lands. Deciding from the status produced no transition
        at all, which left the graph suspended on the edge out of `implement`
        and the job running until its lease expired."""
        self.assertEqual(
            await self._transition_for_developer_event("implementing"),
            [(
                "00000000-0000-0000-0000-000000000001",
                "local_pipeline",
                {
                    "status": "local_pipeline",
                    "last_delivery_outcome": "delivered",
                    "awaiting_execution": False,
                },
            )],
        )

    async def test_successful_developer_event_advances_the_pushing_phase(self) -> None:
        """`push` dispatches the same `developer` role as `implement`, so the
        phase is the only thing that separates them."""
        self.assertEqual(
            await self._transition_for_developer_event("pushing"),
            [(
                "00000000-0000-0000-0000-000000000001",
                "pr_created",
                {"status": "pr_created", "awaiting_execution": False},
            )],
        )

    async def test_bootstrap_planner_event_closes_no_request(self) -> None:
        """The first planning dispatch predates any request row, so there is
        nothing to close and nothing may be guessed at."""
        pool = _EventPool()
        pool.has_existing_requests = False
        pool.dispatched = {}
        control_plane = AsyncpgControlPlane(pool)

        await control_plane.accept_event(
            self._event(
                "00000000-0000-0000-0000-000000000002-plan",
                "completed",
                {"result": {"status": "ready", "summary": "", "assumptions": [], "questions": [],
                            "risk": "low", "acceptanceCriteria": [], "steps": []}},
            ),
            NOW,
        )

        self.assertEqual(self._request_status_writes(pool), [])


class _OutboxConnection:
    def __init__(self, pool: _OutboxPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        self.pool.calls.append((query, arguments))
        rows = self.pool.pending_rows
        self.pool.pending_rows = []
        return rows

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        return await self.pool.fetchrow(query, *arguments)

    async def execute(self, query: str, *arguments: object) -> str:
        return await self.pool.execute(query, *arguments)


class _OutboxPool:
    def __init__(
        self,
        workflow_status: str = "implementing",
        last_request: dict[str, object] | None = None,
    ) -> None:
        self.calls: list[tuple[str, tuple[object, ...]]] = []
        self.pending_rows: list[dict[str, object]] = []
        self.stalled_rows: list[dict[str, object]] = []
        self.orphaned_rows: list[dict[str, object]] = []
        self.workflow_status = workflow_status
        self.last_request = last_request or {
            "role": None, "request_status": None, "next_attempt": None, "lost_attempts": 0
        }

    def acquire(self) -> _OutboxConnection:
        return _OutboxConnection(self)

    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        self.calls.append((query, arguments))
        if "UPDATE app.workflow_execution_requests" in query:
            return self.orphaned_rows
        return self.stalled_rows

    async def execute(self, query: str, *arguments: object) -> str:
        self.calls.append((query, arguments))
        return "UPDATE 1"

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        self.calls.append((query, arguments))
        return {"status": self.workflow_status, **self.last_request}


class OutboxAndReconcilerTests(unittest.IsolatedAsyncioTestCase):
    async def test_drain_pending_transitions_marks_processed_on_success(self) -> None:
        pool = _OutboxPool()
        pool.pending_rows = [
            {"id": "e1", "workflow_run_id": "wf-1", "new_status": "pushing", "state_updates": '{"review_approved": true}'},
        ]
        control_plane = AsyncpgControlPlane(pool)
        delivered: list[tuple[str, str, dict[str, object]]] = []

        async def on_transition(workflow_run_id: str, new_status: str, updates: dict[str, object]) -> None:
            delivered.append((workflow_run_id, new_status, updates))

        processed = await control_plane.drain_pending_transitions(on_transition, NOW)
        self.assertEqual(processed, 1)
        self.assertEqual(delivered, [("wf-1", "pushing", {"review_approved": True})])
        self.assertTrue(any("status = 'processed'" in query for query, _ in pool.calls))

    async def test_drain_pending_transitions_retries_on_failure(self) -> None:
        pool = _OutboxPool()
        pool.pending_rows = [
            {"id": "e1", "workflow_run_id": "wf-1", "new_status": "pushing", "state_updates": {}},
        ]
        control_plane = AsyncpgControlPlane(pool)

        async def failing(*_args: object) -> None:
            raise RuntimeError("boom")

        processed = await control_plane.drain_pending_transitions(failing, NOW)
        self.assertEqual(processed, 0)
        release = next(
            query for query, _ in pool.calls if "attempts = attempts + 1" in query
        )
        # Released, not left claimed: the row goes back to pending with its
        # lease cleared so the next pass picks it up immediately.
        self.assertIn("SET status = 'pending', processing_started_at = NULL", release)

    async def test_drain_reclaims_processing_rows_whose_lease_expired(self) -> None:
        """Issue #96: `processing` is committed before the delivery starts, so a
        drainer that dies mid-delivery cannot release its own row. Selecting
        `pending` alone dropped that transition forever."""
        pool = _OutboxPool()
        control_plane = AsyncpgControlPlane(pool)

        async def on_transition(*_args: object) -> None:
            return None

        await control_plane.drain_pending_transitions(
            on_transition, NOW, processing_lease=timedelta(seconds=90)
        )

        claim, arguments = next(
            (query, arguments) for query, arguments in pool.calls
            if "UPDATE app.workflow_transition_outbox" in query and "FOR UPDATE SKIP LOCKED" in query
        )
        self.assertIn("status = 'pending'", claim)
        self.assertIn("status = 'processing'", claim)
        self.assertIn("processing_started_at IS NULL OR processing_started_at <= $3", claim)
        self.assertIn("SET status = 'processing', processing_started_at = $2", claim)
        self.assertEqual(arguments[1], NOW)
        self.assertEqual(arguments[2], NOW - timedelta(seconds=90))

    async def test_the_drain_lease_defaults_to_a_bounded_window(self) -> None:
        """The caller in main.py does not pass one, so the default is what
        actually bounds how long a row can stay in `processing`."""
        pool = _OutboxPool()
        control_plane = AsyncpgControlPlane(pool)

        async def on_transition(*_args: object) -> None:
            return None

        await control_plane.drain_pending_transitions(on_transition, NOW)

        arguments = next(
            arguments for query, arguments in pool.calls
            if "FOR UPDATE SKIP LOCKED" in query and "workflow_transition_outbox" in query
        )
        reclaim_before = arguments[2]
        self.assertIsInstance(reclaim_before, datetime)
        self.assertGreater(NOW - reclaim_before, timedelta())
        self.assertLessEqual(NOW - reclaim_before, timedelta(minutes=2))

    async def test_find_workflow_runs_waiting_for_checks_returns_ids(self) -> None:
        pool = _OutboxPool()
        pool.stalled_rows = [{"id": "wf-1"}, {"id": "wf-2"}]

        waiting = await AsyncpgControlPlane(pool).find_workflow_runs_waiting_for_checks()

        self.assertEqual(waiting, ("wf-1", "wf-2"))
        query = next(query for query, _ in pool.calls if "FROM app.workflow_runs" in query)
        self.assertIn("status = 'waiting_github_checks'", query)
        self.assertIn("current_phase = 'waiting_github_checks'", query)

    async def test_find_stalled_workflow_runs_returns_ids(self) -> None:
        pool = _OutboxPool()
        pool.stalled_rows = [{"id": "wf-1"}, {"id": "wf-2"}]
        control_plane = AsyncpgControlPlane(pool)
        stalled = await control_plane.find_stalled_workflow_runs(NOW, timedelta(minutes=5))
        self.assertEqual(stalled, ("wf-1", "wf-2"))

    async def test_find_stalled_workflow_runs_only_considers_execution_bound_statuses(self) -> None:
        """A run parked on a GitHub check or a human decision has no execution
        in flight by design; re-entering its graph would resolve the pending
        approval interrupt as "not approved"."""
        pool = _OutboxPool()
        control_plane = AsyncpgControlPlane(pool)

        await control_plane.find_stalled_workflow_runs(NOW, timedelta(minutes=5))

        query = next(query for query, _ in pool.calls if "FROM app.workflow_runs AS wr" in query)
        for parked in ("waiting_human", "waiting_github_checks", "pr_created", "merging"):
            self.assertNotIn(f"'{parked}'", query)
        for active in ("preparing", "planning", "implementing", "local_pipeline", "repairing",
                       "ai_review", "pushing", "recovering"):
            self.assertIn(f"'{active}'", query)
        # A job handed to recover_one is not stalled work.
        self.assertIn("'offered', 'preparing', 'running', 'recovering'", query)

    RUN_ID = "00000000-0000-0000-0000-000000000001"

    async def _recover(self, pool: _OutboxPool) -> tuple[bool, list[tuple[str, str, dict[str, object]]]]:
        delivered: list[tuple[str, str, dict[str, object]]] = []

        async def on_transition(workflow_run_id: str, new_status: str, updates: dict[str, object]) -> None:
            delivered.append((workflow_run_id, new_status, updates))

        recovered = await AsyncpgControlPlane(pool).recover_stalled_workflow_run(
            self.RUN_ID, on_transition, NOW
        )
        return recovered, delivered

    async def test_recover_stalled_workflow_run_clears_the_suspension_flag(self) -> None:
        """A request closed by its own terminal event means the execution
        reported and only the graph invocation was lost, so the graph is
        re-entered. `awaiting_execution` has to be cleared: it is what suspends
        the graph on the edge out of a dispatching node, so leaving it set
        would route the resumed graph straight back to END (issue #94)."""
        pool = _OutboxPool(
            last_request={"role": "planner", "request_status": "completed", "next_attempt": 2, "lost_attempts": 0}
        )

        recovered, delivered = await self._recover(pool)

        self.assertTrue(recovered)
        self.assertEqual(delivered, [(self.RUN_ID, "implementing", {"awaiting_execution": False})])
        self.assertFalse(
            any("INSERT INTO app.workflow_execution_requests" in query for query, _ in pool.calls)
        )

    async def test_recover_stalled_workflow_run_requeues_a_lost_execution(self) -> None:
        """An orphaned request means no runner will ever report that execution.
        Re-entering the graph would let the unconditional edges out of
        `implement`, `repair` and `push` fire, skipping the phase nobody ran --
        for `push`, that opens and merges a pull request for a branch that was
        never pushed. The phase is queued again instead, and the graph stays
        suspended waiting for it."""
        pool = _OutboxPool(
            last_request={"role": "developer", "request_status": "orphaned", "next_attempt": 2, "lost_attempts": 1}
        )

        recovered, delivered = await self._recover(pool)

        self.assertTrue(recovered)
        self.assertEqual(delivered, [])
        insert = next(
            arguments for query, arguments in pool.calls
            if "INSERT INTO app.workflow_execution_requests" in query
        )
        self.assertEqual(insert[2:5], ("developer", 2, NOW))
        self.assertTrue(
            any("'execution_requeued'" in query for query, _ in pool.calls)
        )

    async def test_recover_stalled_workflow_run_stops_replacing_a_repeatedly_lost_execution(self) -> None:
        """A re-queue spends no retry budget, so nothing else would ever bound
        how many agent executions one wedged phase can buy."""
        pool = _OutboxPool(
            last_request={
                "role": "developer", "request_status": "orphaned",
                "next_attempt": 7, "lost_attempts": 6,
            }
        )

        recovered, delivered = await self._recover(pool)

        self.assertFalse(recovered)
        self.assertEqual(delivered, [])
        self.assertFalse(
            any("INSERT INTO app.workflow_execution_requests" in query for query, _ in pool.calls)
        )

    async def test_recover_stalled_workflow_run_defers_a_run_it_could_not_repair(self) -> None:
        """`updated_at` is what the detector reads. Bumping it for every run the
        loop touches -- not only the ones it repairs -- stops a permanently
        failing run from occupying the bounded batch on every tick."""
        pool = _OutboxPool(
            last_request={"role": "planner", "request_status": "completed", "next_attempt": 2, "lost_attempts": 0}
        )

        await self._recover(pool)

        self.assertTrue(
            any(
                "UPDATE app.workflow_runs SET updated_at" in query and arguments[1] == NOW
                for query, arguments in pool.calls
            )
        )

    async def test_recover_stalled_workflow_run_skips_a_run_that_became_terminal(self) -> None:
        pool = _OutboxPool(
            workflow_status="blocked",
            last_request={"role": "planner", "request_status": "orphaned", "next_attempt": 2, "lost_attempts": 1},
        )

        recovered, delivered = await self._recover(pool)

        self.assertFalse(recovered)
        self.assertEqual(delivered, [])
        self.assertFalse(
            any("INSERT INTO app.workflow_execution_requests" in query for query, _ in pool.calls)
        )

    async def test_close_orphaned_execution_requests_reports_the_rows_it_closed(self) -> None:
        pool = _OutboxPool()
        pool.orphaned_rows = [{"id": "r1"}, {"id": "r2"}]
        control_plane = AsyncpgControlPlane(pool)

        closed = await control_plane.close_orphaned_execution_requests(NOW, timedelta(minutes=2))

        self.assertEqual(closed, 2)
        _, arguments = next(
            call for call in pool.calls if "UPDATE app.workflow_execution_requests" in call[0]
        )
        self.assertEqual(arguments, (NOW - timedelta(minutes=2), "orphaned"))


class _ListWorkflowsPool:
    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        if "FROM app.workflow_events" in query:
            assert arguments[1] == 12
            assert arguments[2] == 2
            return [
                {"id": 11, "event_type": "log", "payload": '{"message":"agent output"}', "created_at": NOW},
                {"id": 10, "event_type": "started", "payload": {}, "created_at": NOW},
            ]
        assert "FROM app.workflow_runs" in query
        # The list shares get_workflow's projection, so it joins the issue and
        # any pull request rather than reading workflow_runs alone.
        assert "JOIN app.issues" in query
        return [
            {
                "id": "00000000-0000-0000-0000-000000000001",
                "project_id": "00000000-0000-0000-0000-000000000002",
                "status": "blocked",
                "current_phase": "blocked",
                "issue_external_id": "42",
                "issue_title": "Fix workflow visibility",
                "branch_name": "agent/42/fix",
                "pull_request_external_id": "42",
                "pull_request_url": "https://github.com/example/repo/pull/42",
                "pull_request_state": "open",
                "blocking_reason": "workflow retry budget exhausted",
                "planning_attempts": 1,
                "implementation_attempts": 2,
                "pipeline_repair_attempts": 0,
                "review_cycles": 3,
                "ci_repair_attempts": 0,
                "total_agent_executions": 6,
                "created_at": NOW,
                "updated_at": NOW,
            }
        ]

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        assert "JOIN app.issues" in query
        assert arguments[0] == UUID("00000000-0000-0000-0000-000000000001")
        return {
            "id": "00000000-0000-0000-0000-000000000001",
            "project_id": "00000000-0000-0000-0000-000000000002",
            "status": "blocked", "current_phase": "blocked", "issue_external_id": "42",
            "issue_title": "Fix workflow visibility", "branch_name": "agent/42/fix",
            "pull_request_external_id": "42", "pull_request_url": "https://github.com/example/repo/pull/42",
            "pull_request_state": "open", "blocking_reason": "workflow retry budget exhausted",
            "planning_attempts": 1, "implementation_attempts": 2, "pipeline_repair_attempts": 0,
            "review_cycles": 3, "ci_repair_attempts": 0, "total_agent_executions": 6,
            "created_at": NOW, "updated_at": NOW,
        }


class ListWorkflowsTests(unittest.IsolatedAsyncioTestCase):
    async def test_list_workflows_surfaces_durable_columns(self) -> None:
        control_plane = AsyncpgControlPlane(_ListWorkflowsPool())
        workflows = await control_plane.list_workflows()
        self.assertEqual(len(workflows), 1)
        workflow = workflows[0]
        self.assertEqual(workflow["status"], "blocked")
        self.assertEqual(workflow["pull_request_url"], "https://github.com/example/repo/pull/42")
        self.assertEqual(workflow["blocking_reason"], "workflow retry budget exhausted")
        self.assertEqual(workflow["review_cycles"], 3)
        self.assertEqual(workflow["total_agent_executions"], 6)
        # The console's list view renders these without a per-row detail fetch.
        self.assertEqual(workflow["issue_external_id"], "42")
        self.assertEqual(workflow["issue_title"], "Fix workflow visibility")
        self.assertEqual(workflow["branch_name"], "agent/42/fix")
        self.assertEqual(workflow["pull_request_state"], "open")
        self.assertEqual(workflow["created_at"], NOW)

    async def test_reads_workflow_detail_and_cursor_page(self) -> None:
        control_plane = AsyncpgControlPlane(_ListWorkflowsPool())
        workflow = await control_plane.get_workflow("00000000-0000-0000-0000-000000000001")
        events = await control_plane.list_workflow_events("00000000-0000-0000-0000-000000000001", 12, 2)
        self.assertIsNotNone(workflow)
        assert workflow is not None
        self.assertEqual(workflow["issue_title"], "Fix workflow visibility")
        self.assertEqual(workflow["pull_request_state"], "open")
        self.assertEqual([event["id"] for event in events], ["11", "10"])
        self.assertEqual(events[0]["payload_json"], '{"message":"agent output"}')


_PROGRESS_JOB_ID = "00000000-0000-0000-0000-0000000000b2"
_PROGRESS_WORKFLOW_ID = "00000000-0000-0000-0000-0000000000b1"
_PROGRESS_RUNNER_ID = "00000000-0000-0000-0000-0000000000b3"


class _ProgressConnection:
    def __init__(self, pool: _ProgressPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
        return _Transaction()

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        self.pool.calls.append((query, arguments))
        if "FROM app.jobs AS j" in query and "JOIN app.workflow_runs" in query:
            return {
                "id": _PROGRESS_JOB_ID,
                "workflow_run_id": _PROGRESS_WORKFLOW_ID,
                "lease_generation": 1,
                "lease_expires_at": NOW + timedelta(minutes=5),
                "last_event_sequence": self.pool.last_event_sequence,
                # `preparing` stands in for the status accept_offer leaves on
                # every run with an execution in flight: reading it instead of
                # the phase transitions nothing at all (issue #141).
                "workflow_phase": _selected_phase(query, "preparing", self.pool.workflow_phase),
                "last_diff_hash": self.pool.last_diff_hash,
                "last_failure_fingerprint": self.pool.last_failure_fingerprint,
                "non_progress_attempts": self.pool.non_progress_attempts,
            }
        if "SELECT 1 AS present FROM app.workflow_execution_requests" in query:
            return {"present": 1}
        if "SELECT id, role, attempt" in query and "workflow_execution_requests" in query:
            request = self.pool.dispatched.get(str(arguments[0]))
            return None if request is None else {"id": arguments[0], **request}
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        self.pool.calls.append((query, arguments))
        if "UPDATE app.workflow_runs" in query and "last_diff_hash" in query:
            self.pool.apply_progress_update(query, arguments)
        elif "UPDATE app.workflow_runs" in query and "SET status = $2, current_phase = $2" in query:
            self.pool.workflow_phase = str(arguments[1])
        elif "INSERT INTO app.workflow_events" in query:
            self.pool.events.append({
                "id": len(self.pool.events) + 1,
                "event_type": arguments[1],
                "payload": json.loads(str(arguments[2])),
                "created_at": arguments[3],
            })
        return "INSERT 0 1"


class _ProgressPool:
    """Stateful fake for the non-progress evidence path.

    It keeps the three ``app.workflow_runs`` progress columns across events so a
    whole terminal-event sequence can be replayed. ``apply_progress_update``
    interprets the statement that was actually issued (plain assignment versus
    ``COALESCE``) so the fake matches PostgreSQL for either writer.
    """

    def __init__(self, workflow_phase: str = "planning") -> None:
        self.calls: list[tuple[str, tuple[object, ...]]] = []
        self.dispatched: dict[str, dict[str, object]] = {}
        self.workflow_phase = workflow_phase
        self.last_event_sequence = 0
        self.last_diff_hash: str | None = None
        self.last_failure_fingerprint: str | None = None
        self.non_progress_attempts = 0
        self.events: list[dict[str, object]] = []

    def acquire(self) -> _ProgressConnection:
        return _ProgressConnection(self)

    async def execute(self, query: str, *arguments: object) -> str:
        self.calls.append((query, arguments))
        return "INSERT 0 1"

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        self.calls.append((query, arguments))
        return {"id": arguments[0]}

    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        self.calls.append((query, arguments))
        if "FROM app.workflow_events" not in query:
            raise AssertionError(query)
        return list(reversed(self.events))[: int(arguments[2])]

    def apply_progress_update(self, query: str, arguments: tuple[object, ...]) -> None:
        diff_hash = arguments[1]
        fingerprint = arguments[2]
        preserves_unset = "COALESCE($2" in query
        if not preserves_unset or diff_hash is not None:
            self.last_diff_hash = None if diff_hash is None else str(diff_hash)
        if not preserves_unset or fingerprint is not None:
            self.last_failure_fingerprint = None if fingerprint is None else str(fingerprint)
        self.non_progress_attempts = int(arguments[3])  # type: ignore[call-overload]


class NonProgressEvidenceTests(unittest.IsolatedAsyncioTestCase):
    """Covers issue #101 / review finding F14.

    The non-progress detector must increment only for same-phase, semantically
    identical terminal outcomes, and must block at the threshold the README
    documents (four identical outcomes).
    """

    def setUp(self) -> None:
        self.pool = _ProgressPool()
        self.control_plane = AsyncpgControlPlane(self.pool)
        self.transitions: list[tuple[str, str, dict[str, object]]] = []

    async def _on_transition(
        self, workflow_run_id: str, new_status: str, updates: dict[str, object]
    ) -> None:
        self.transitions.append((workflow_run_id, new_status, updates))

    async def _emit(
        self,
        role: str,
        event_type: str,
        payload: dict[str, object] | None = None,
    ) -> None:
        request_id = str(uuid4())
        self.pool.dispatched[request_id] = {"role": role, "attempt": 1}
        self.pool.last_event_sequence += 1
        await self.control_plane.accept_event(
            ExecutionEvent(
                job_id=_PROGRESS_JOB_ID,
                runner_id=_PROGRESS_RUNNER_ID,
                lease_generation=1,
                event_sequence=self.pool.last_event_sequence,
                event_type=event_type,
                execution_id=f"{request_id}-{ROLE_TO_SUFFIX.get(role, 'implement')}",
                payload=payload or {},
            ),
            NOW,
            on_transition=self._on_transition,
        )

    @staticmethod
    def _failure_payload(duration_ms: int, fingerprint: str = "pipeline:b455761a24ca33ba") -> dict[str, object]:
        """A realistic runner `failed` payload. `durationMs` is the volatile
        field that used to make two identical failures hash differently."""
        return {
            "status": "failed",
            "exitCode": 1,
            "changedFiles": [],
            "commandsRun": ["ruff check ."],
            "finalRevision": "abc123",
            "committed": False,
            "pushed": False,
            "durationMs": duration_ms,
            "changedFileCount": 0,
            "commandCount": 1,
            "pipelineCommandCount": 1,
            "failureFingerprint": fingerprint,
            "error": "pipeline failed: ruff check exited 1",
        }

    async def test_log_event_written_by_accept_event_is_readable(self) -> None:
        await self._emit("developer", "log", {"message": "agent output"})
        events = await self.control_plane.list_workflow_events(_PROGRESS_WORKFLOW_ID, 0, 100)
        self.assertEqual(events[0]["event_type"], "log")
        self.assertEqual(json.loads(events[0]["payload_json"])["payload"]["message"], "agent output")

    async def test_zero_diff_successes_from_different_phases_do_not_collide(self) -> None:
        """Defect 1: the success hash covered only the sorted changed-file list,
        so every zero-diff success collided on sha256("[]") across phases."""
        await self._emit(
            "planner",
            "completed",
            {"exitCode": 0, "changedFiles": [], "result": {"status": "invalid", "summary": "no plan"}},
        )
        self.assertEqual(self.pool.non_progress_attempts, 0)
        await self._emit("pipeline", "completed", {"exitCode": 0, "changedFiles": []})
        self.assertEqual(
            self.pool.non_progress_attempts,
            0,
            "a zero-diff pipeline success must not repeat a zero-diff planner success",
        )

    async def test_healthy_plan_implement_pipeline_review_never_increments(self) -> None:
        """A healthy multi-phase run must leave the counter at zero throughout."""
        await self._emit(
            "planner",
            "completed",
            {
                "exitCode": 0,
                "changedFiles": [],
                "result": {"status": "ready", "summary": "plan", "acceptanceCriteria": [], "steps": []},
            },
        )
        self.assertEqual(self.pool.non_progress_attempts, 0)
        await self._emit(
            "developer",
            "completed",
            {"exitCode": 0, "changedFiles": ["src/a.py"], "result": {"status": "completed", "summary": "done"}},
        )
        self.assertEqual(self.pool.non_progress_attempts, 0)
        await self._emit("pipeline", "completed", {"exitCode": 0, "changedFiles": []})
        self.assertEqual(self.pool.non_progress_attempts, 0)
        await self._emit(
            "reviewer",
            "completed",
            {"exitCode": 0, "changedFiles": [], "result": {"verdict": "approved", "acceptanceCriteria": [], "findings": []}},
        )
        self.assertEqual(self.pool.non_progress_attempts, 0)
        self.assertFalse(
            any(status == "blocked" for _, status, _ in self.transitions),
            "a healthy plan -> implement -> pipeline -> review run must never block",
        )

    async def test_identical_failures_survive_volatile_payload_fields(self) -> None:
        """Defect 2: the failure fingerprint hashed the whole raw payload, so
        durations and counters made genuinely identical failures differ."""
        await self._emit("pipeline", "failed", self._failure_payload(1200))
        self.assertEqual(self.pool.non_progress_attempts, 0)
        await self._emit("pipeline", "failed", self._failure_payload(3400))
        self.assertEqual(
            self.pool.non_progress_attempts,
            1,
            "two identical pipeline failures differing only in durationMs are one outcome",
        )

    async def test_four_identical_failures_block_at_the_documented_threshold(self) -> None:
        """Defect 4: README documents four identical outcomes; the code needed five."""
        for index in range(3):
            await self._emit("pipeline", "failed", self._failure_payload(1000 + index))
            self.assertFalse(
                any(status == "blocked" for _, status, _ in self.transitions),
                f"blocked after only {index + 1} identical outcomes",
            )
        await self._emit("pipeline", "failed", self._failure_payload(9999))
        blocked = [updates for _, status, updates in self.transitions if status == "blocked"]
        self.assertEqual(len(blocked), 1, "the fourth identical outcome must block the workflow")
        self.assertIn("identical execution outcomes", str(blocked[0]["blocking_reason"]))

    async def test_a_success_does_not_erase_a_same_phase_failure_run(self) -> None:
        """Defect 3: `current` could be a failure fingerprint while `previous`
        preferred `last_diff_hash`, so failures were compared against successes."""
        await self._emit("pipeline", "failed", self._failure_payload(100))
        await self._emit(
            "repairer",
            "completed",
            {"exitCode": 0, "changedFiles": ["src/a.py"], "result": {"status": "completed", "summary": "repair"}},
        )
        await self._emit("pipeline", "failed", self._failure_payload(200))
        self.assertEqual(
            self.pool.non_progress_attempts,
            1,
            "an intervening success of another role must not hide a repeated pipeline failure",
        )

    async def test_different_failures_reset_the_counter(self) -> None:
        await self._emit("pipeline", "failed", self._failure_payload(100))
        await self._emit("pipeline", "failed", self._failure_payload(200))
        self.assertEqual(self.pool.non_progress_attempts, 1)
        await self._emit(
            "pipeline", "failed", self._failure_payload(300, fingerprint="pipeline:0000000000000000")
        )
        self.assertEqual(self.pool.non_progress_attempts, 0, "a different failure is progress")

    async def test_no_diff_is_recorded_as_null_never_as_an_empty_string(self) -> None:
        """Defect 5: `last_diff_hash` had two encodings for "no diff"."""
        for index in range(4):
            await self._emit("pipeline", "failed", self._failure_payload(index))
        progress_writes = [
            arguments
            for query, arguments in self.pool.calls
            if "UPDATE app.workflow_runs" in query and "last_diff_hash" in query
        ]
        self.assertTrue(progress_writes)
        self.assertFalse(
            any(arguments[1] == "" for arguments in progress_writes),
            "the SQL writer must use NULL, never an empty string",
        )
        blocked = [updates for _, status, updates in self.transitions if status == "blocked"]
        self.assertEqual(len(blocked), 1)
        self.assertNotEqual(
            blocked[0].get("last_diff_hash", None), "", "state updates must not encode 'no diff' as ''"
        )


class FailureFingerprintDefinitionTests(unittest.TestCase):
    """`_runner_failure_fingerprint` is a port of the runner's
    `dispatch.FailureFingerprint` (runner/internal/dispatch/fingerprint.go).
    The expected values below were produced by the Go implementation itself."""

    def test_matches_the_runner_implementation(self) -> None:
        cases = {
            "execute agent: agent exited 0 without a valid result document "
            "(.loop/result.json): agent result was not written": "agent:42051f1c5fc5560d",
            "pipeline failed: ruff check exited 1": "pipeline:b455761a24ca33ba",
            "pipeline failed token=secret-value": "pipeline:9556f5fa7d015562",
            "Some Mixed CASE Failure With No Category": "execution:8a734b398eae890c",
            "git push rejected: non-fast-forward": "git:18d3405ee44dffb8",
            "executor timeout after 3600s": "executor:895cb601a71e2fc6",
        }
        for message, expected in cases.items():
            with self.subTest(message=message):
                self.assertEqual(_runner_failure_fingerprint("execution", message), expected)

    def test_redacts_secret_bearing_suffixes(self) -> None:
        first = _runner_failure_fingerprint("execution", "pipeline failed token=secret-value")
        second = _runner_failure_fingerprint("execution", "pipeline failed token=other-value")
        self.assertEqual(first, second)
        self.assertNotIn("secret-value", first)


class _QueuePool:
    """Answers `list_queue`'s single SELECT over app.issues AS i."""

    def __init__(self) -> None:
        self.rows: list[dict[str, object]] = []
        self.query: str | None = None

    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        del arguments
        self.query = query
        return self.rows


class QueueDefinitionTests(unittest.IsolatedAsyncioTestCase):
    async def test_list_queue_selects_open_eligible_issues_with_reason(self) -> None:
        pool = _QueuePool()
        pool.rows = [
            {
                "project_id": "00000000-0000-0000-0000-000000000001",
                "project_name": "Alpha",
                "external_id": "42",
                "title": "Implement queue",
                "priority": 100,
                "blocked_reason": "no_matching_runner",
            },
            {
                "project_id": "00000000-0000-0000-0000-000000000002",
                "project_name": "Beta",
                "external_id": "7",
                "title": "Lower priority",
                "priority": 10,
                "blocked_reason": "",
            },
        ]
        entries = await AsyncpgControlPlane(pool).list_queue(NOW, 25)
        assert pool.query is not None
        self.assertIn("i.state = 'open'", pool.query)
        self.assertIn("i.eligible = true", pool.query)
        self.assertIn("ORDER BY i.priority DESC", pool.query)
        self.assertIn("LIMIT $2", pool.query)
        self.assertEqual(entries[0]["external_id"], "42")
        self.assertEqual(entries[0]["blocked_reason"], "no_matching_runner")
        self.assertEqual(entries[1]["blocked_reason"], "")


if __name__ == "__main__":
    unittest.main()
