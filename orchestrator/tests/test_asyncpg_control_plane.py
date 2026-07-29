from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta
from hashlib import sha256
from typing import Self
from uuid import uuid4

from moirai.domain.control_plane import AuthenticationError, OfferError, RegistrationError
from moirai.domain.leases import StaleLeaseError
from moirai.domain.models import ExecutionEvent
from moirai.persistence.control_plane import AsyncpgControlPlane

NOW = datetime(2026, 1, 1, tzinfo=UTC)


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


class _DurableConnection:
    def __init__(self, pool: _DurablePool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    def transaction(self) -> _Transaction:
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
            return {"job_id": self.pool.job_id}
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
        if "SET status = 'cancelled'" in query:
            if self.pool.job_status != "offered":
                return None
            self.pool.job_status = "cancelled"
            return {"workflow_run_id": self.pool.workflow_id, "project_id": self.pool.project_id}
        if "SELECT state, opened_at FROM app.project_circuit_state" in query:
            return self.pool.project_circuit
        if "SELECT state, opened_at FROM app.provider_circuit_state" in query:
            return self.pool.provider_circuit
        if "FROM app.issues AS i" in query:
            return self.pool.candidate
        if "UPDATE app.job_offers" in query:
            if self.pool.offer_status != "offered" or self.pool.offer_expires_at <= arguments[2]:
                return None
            self.pool.offer_status = "rejected" if "SET status = 'rejected'" in query else "accepted"
            return {"job_id": self.pool.job_id}
        if "SET status = 'cancelled'" in query:
            if self.pool.job_status != "offered":
                return None
            self.pool.job_status = "cancelled"
            return {"workflow_run_id": self.pool.workflow_id, "project_id": self.pool.project_id}
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
                "workflow_status": self.pool.workflow_status,
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
        if "SELECT role, attempt" in query and "workflow_execution_requests" in query:
            for request_id, role, attempt in self.pool.dispatched_requests:
                if str(request_id) == str(arguments[0]):
                    return {"role": role, "attempt": attempt}
            return None
        raise AssertionError(query)

    async def execute(self, query: str, *arguments: object) -> str:
        self.pool.queries.append(query)
        if "SET state = 'half_open'" in query:
            if "project_circuit_state" in query and self.pool.project_circuit is not None:
                self.pool.project_circuit["state"] = "half_open"
            if "provider_circuit_state" in query and self.pool.provider_circuit is not None:
                self.pool.provider_circuit["state"] = "half_open"
            return "UPDATE 1"
        if "UPDATE app.workflow_execution_requests" in query:
            if self.pool.execution_request_status != "queued":
                return "UPDATE 0"
            self.pool.execution_request_status = "dispatched"
            return "UPDATE 1"
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
        self.job_status = "offered"
        self.workflow_status = "planning"
        self.last_event_sequence = 0
        self.execution_request_id = "00000000-0000-0000-0000-000000000007"
        self.execution_request_status = "none"
        self.dispatched_requests: list[tuple[str, str, int]] = []
        self.project_circuit: dict[str, object] | None = None
        self.provider_circuit: dict[str, object] | None = None
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
                "body": "Add durable delivery.",
                "project_id": self.project_id,
                "repository_mode": "managed_clone",
                "repository_url": "https://example.test/repo.git",
                "local_repository_path": None,
                "default_branch": "main",
            }
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
            record = {"id": arguments[0], "name": arguments[1], "enabled": True}
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
        self.assertTrue(any("workflow_execution_requests" in query for query in pool.queries))

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
        self.assertTrue(any("SET status = 'offline'" in query for query in pool.queries))
        self.assertFalse(any("DELETE FROM app.project_locks" in query for query in pool.queries))

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

    async def test_expire_offers_cancels_job_and_releases_project_lock(self) -> None:
        pool = _DurablePool()
        pool.offer_expires_at = NOW
        expired = await AsyncpgControlPlane(pool).expire_offers(NOW)
        self.assertEqual(expired, (pool.job_id,))
        self.assertEqual(pool.offer_status, "expired")
        self.assertEqual(pool.job_status, "cancelled")
        self.assertTrue(any("DELETE FROM app.project_locks" in query for query in pool.queries))

    async def test_accept_offer_rejects_an_expired_durable_offer(self) -> None:
        pool = _DurablePool()
        pool.offer_expires_at = NOW
        with self.assertRaises(OfferError):
            await AsyncpgControlPlane(pool).accept_offer(pool.job_id, pool.runner_id, NOW)

    async def test_reject_offer_cancels_the_durable_job_and_releases_the_lock(self) -> None:
        pool = _DurablePool()
        await AsyncpgControlPlane(pool).reject_offer(pool.job_id, pool.runner_id, NOW)
        self.assertEqual(pool.offer_status, "rejected")
        self.assertEqual(pool.job_status, "cancelled")
        self.assertTrue(any("DELETE FROM app.project_locks" in query for query in pool.queries))

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
        self.assertEqual((await control_plane.list_projects())[0]["name"], "Example")
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
            return dict(self.pool.job_row) if self.pool.job_row is not None else None
        if "SELECT 1 AS present FROM app.workflow_execution_requests" in query:
            return {"present": 1} if self.pool.has_existing_requests else None
        if "SELECT role, attempt" in query and "workflow_execution_requests" in query:
            return self.pool.dispatched.get(str(arguments[0]))
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
            "workflow_status": "ai_review",
        }
        self.has_existing_requests = True
        self.dispatched: dict[str, dict[str, object]] = {}

    def acquire(self) -> _EventConnection:
        return _EventConnection(self)

    async def execute(self, query: str, *arguments: object) -> str:
        self.calls.append((query, arguments))
        return "INSERT 0 1"


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


class _OutboxConnection:
    def __init__(self, pool: _OutboxPool) -> None:
        self.pool = pool

    async def __aenter__(self) -> Self:
        return self

    async def __aexit__(self, exc_type: object, exc: object, traceback: object) -> None:
        return None

    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        self.pool.calls.append((query, arguments))
        rows = self.pool.pending_rows
        self.pool.pending_rows = []
        return rows


class _OutboxPool:
    def __init__(self) -> None:
        self.calls: list[tuple[str, tuple[object, ...]]] = []
        self.pending_rows: list[dict[str, object]] = []
        self.stalled_rows: list[dict[str, object]] = []

    def acquire(self) -> _OutboxConnection:
        return _OutboxConnection(self)

    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        self.calls.append((query, arguments))
        return self.stalled_rows

    async def execute(self, query: str, *arguments: object) -> str:
        self.calls.append((query, arguments))
        return "UPDATE 1"

    async def fetchrow(self, query: str, *arguments: object) -> dict[str, object] | None:
        self.calls.append((query, arguments))
        return {"status": "implementing"}


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
        self.assertTrue(any("attempts = attempts + 1" in query for query, _ in pool.calls))

    async def test_find_stalled_workflow_runs_returns_ids(self) -> None:
        pool = _OutboxPool()
        pool.stalled_rows = [{"id": "wf-1"}, {"id": "wf-2"}]
        control_plane = AsyncpgControlPlane(pool)
        stalled = await control_plane.find_stalled_workflow_runs(NOW, timedelta(minutes=5))
        self.assertEqual(stalled, ("wf-1", "wf-2"))

    async def test_recover_stalled_workflow_run_replays_current_status_with_empty_updates(self) -> None:
        pool = _OutboxPool()
        control_plane = AsyncpgControlPlane(pool)
        delivered: list[tuple[str, str, dict[str, object]]] = []

        async def on_transition(workflow_run_id: str, new_status: str, updates: dict[str, object]) -> None:
            delivered.append((workflow_run_id, new_status, updates))

        recovered = await control_plane.recover_stalled_workflow_run(
            "00000000-0000-0000-0000-000000000001", on_transition
        )
        self.assertTrue(recovered)
        self.assertEqual(delivered, [("00000000-0000-0000-0000-000000000001", "implementing", {})])


class _ListWorkflowsPool:
    async def fetch(self, query: str, *arguments: object) -> list[dict[str, object]]:
        assert "FROM app.workflow_runs" in query
        return [
            {
                "id": "00000000-0000-0000-0000-000000000001",
                "project_id": "00000000-0000-0000-0000-000000000002",
                "status": "blocked",
                "current_phase": "blocked",
                "pull_request_external_id": "42",
                "pull_request_url": "https://github.com/example/repo/pull/42",
                "blocking_reason": "workflow retry budget exhausted",
                "planning_attempts": 1,
                "implementation_attempts": 2,
                "pipeline_repair_attempts": 0,
                "review_cycles": 3,
                "ci_repair_attempts": 0,
                "total_agent_executions": 6,
            }
        ]


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


if __name__ == "__main__":
    unittest.main()
