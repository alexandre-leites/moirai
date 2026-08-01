from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass, replace
from datetime import datetime, timedelta
from hashlib import sha256
from secrets import token_urlsafe
from threading import RLock
from typing import Any
from uuid import uuid4

from .credentials import CREDENTIAL_DELIVERY, CREDENTIAL_KIND_BY_ENVIRONMENT_NAME
from .leases import StaleLeaseError, accept_event, renew_lease
from .models import ExecutionEvent, Issue, JobLease, Project, Runner, Workflow, WorkflowStatus
from .scheduling import Assignment, select_assignment


class RegistrationError(ValueError):
    pass


class AuthenticationError(ValueError):
    pass


class OfferError(ValueError):
    pass


@dataclass(frozen=True)
class RegistrationToken:
    token_hash: str
    allowed_labels: frozenset[str]
    expires_at: datetime
    used_at: datetime | None = None
    revoked_at: datetime | None = None


@dataclass(frozen=True)
class RunnerCredential:
    runner_id: str
    secret_hash: str
    expires_at: datetime | None = None
    revoked_at: datetime | None = None


@dataclass(frozen=True)
class JobOffer:
    job_id: str
    workflow_id: str
    issue_id: str
    runner_id: str
    expires_at: datetime
    lease: JobLease
    accepted_at: datetime | None = None


@dataclass(frozen=True)
class ScheduledJob:
    assignment: Assignment
    workflow: Workflow
    offer: JobOffer


class InMemoryControlPlane:
    def __init__(self) -> None:
        self._lock = RLock()
        self._tokens: dict[str, RegistrationToken] = {}
        self._credentials: dict[str, RunnerCredential] = {}
        self._runners: dict[str, Runner] = {}
        self._projects: dict[str, Project] = {}
        self._issues: dict[str, Issue] = {}
        self._workflows: dict[str, Workflow] = {}
        self._offers: dict[str, JobOffer] = {}
        self._leases: dict[str, JobLease] = {}
        self._project_locks: dict[str, str] = {}
        self._project_credentials: dict[tuple[str, str], str] = {}
        self._audit: list[tuple[str | None, str, str, str, str, datetime]] = []

    @staticmethod
    def _hash(secret: str) -> str:
        return sha256(secret.encode("utf-8")).hexdigest()

    def create_registration_token(
        self, allowed_labels: Iterable[str], expires_at: datetime
    ) -> str:
        raw_token = token_urlsafe(32)
        with self._lock:
            self._tokens[self._hash(raw_token)] = RegistrationToken(
                token_hash=self._hash(raw_token),
                allowed_labels=frozenset(allowed_labels),
                expires_at=expires_at,
            )
        return raw_token

    def register_runner(
        self, token: str, name: str, labels: Iterable[str], now: datetime, capacity: int = 1
    ) -> tuple[Runner, str]:
        if capacity < 1:
            raise RegistrationError("runner capacity must be a positive integer")
        token_hash = self._hash(token)
        labels_set = frozenset(labels)
        with self._lock:
            registration = self._tokens.get(token_hash)
            if registration is None or registration.revoked_at is not None:
                raise RegistrationError("registration token is invalid")
            if registration.used_at is not None or registration.expires_at <= now:
                raise RegistrationError("registration token cannot be used")
            if not labels_set.issubset(registration.allowed_labels):
                raise RegistrationError("runner labels exceed token permissions")
            runner = Runner(str(uuid4()), labels_set, False, True, False, False, capacity=capacity)
            credential = token_urlsafe(32)
            self._tokens[token_hash] = replace(registration, used_at=now)
            self._runners[runner.id] = runner
            self._credentials[runner.id] = RunnerCredential(runner.id, self._hash(credential))
            return runner, credential

    def authenticate_runner(self, runner_id: str, credential: str, now: datetime) -> Runner:
        with self._lock:
            stored = self._credentials.get(runner_id)
            runner = self._runners.get(runner_id)
            if stored is None or runner is None:
                raise AuthenticationError("runner is unknown")
            if stored.revoked_at is not None or (stored.expires_at is not None and stored.expires_at <= now):
                raise AuthenticationError("runner credential is inactive")
            if stored.secret_hash != self._hash(credential):
                raise AuthenticationError("runner credential is invalid")
            return runner

    def heartbeat(self, runner_id: str, credential: str, now: datetime) -> Runner:
        with self._lock:
            runner = self.authenticate_runner(runner_id, credential, now)
            updated = replace(runner, connected=True, healthy=True)
            self._runners[runner_id] = updated
            return updated

    def set_runner_draining(self, runner_id: str, draining: bool) -> None:
        # The asyncpg control plane additionally refuses a revoked runner; this
        # one has no runner revocation to refuse (only credentials carry
        # `revoked_at`, and `authenticate_runner` already rejects those).
        with self._lock:
            runner = self._runners.get(runner_id)
            if runner is None:
                raise ValueError("runner is unknown")
            self._runners[runner_id] = replace(runner, draining=draining)

    def add_project(self, project: Project) -> None:
        with self._lock:
            if project.id in self._projects:
                raise ValueError("project already exists")
            self._projects[project.id] = project

    def add_issue(self, issue: Issue) -> None:
        with self._lock:
            if issue.project_id not in self._projects:
                raise ValueError("issue project is unknown")
            self._issues[issue.id] = issue

    def schedule(self, now: datetime, offer_ttl: timedelta) -> ScheduledJob | None:
        if offer_ttl <= timedelta():
            raise ValueError("offer_ttl must be positive")
        with self._lock:
            assignment = select_assignment(
                self._projects.values(), self._issues.values(), self._workflows.values(), self._runners.values()
            )
            if assignment is None:
                return None
            workflow_id = str(uuid4())
            job_id = str(uuid4())
            workflow = Workflow(workflow_id, assignment.issue.project_id, assignment.issue.id, WorkflowStatus.OFFERED)
            lease = JobLease(job_id, assignment.runner.id, 1, now + offer_ttl)
            offer = JobOffer(job_id, workflow_id, assignment.issue.id, assignment.runner.id, now + offer_ttl, lease)
            self._workflows[workflow_id] = workflow
            self._offers[job_id] = offer
            self._leases[job_id] = lease
            self._project_locks[workflow.project_id] = workflow_id
            self._runners[assignment.runner.id] = replace(assignment.runner, active_job_id=job_id)
            return ScheduledJob(assignment, workflow, offer)

    def accept_offer(self, job_id: str, runner_id: str, now: datetime) -> JobLease:
        with self._lock:
            offer = self._offers.get(job_id)
            if offer is None or offer.runner_id != runner_id:
                raise OfferError("job offer is not assigned to this runner")
            if offer.accepted_at is not None or offer.expires_at <= now:
                raise OfferError("job offer is no longer active")
            accepted = replace(offer, accepted_at=now)
            self._offers[job_id] = accepted
            workflow = self._workflows[offer.workflow_id]
            self._workflows[workflow.id] = replace(workflow, status=WorkflowStatus.PREPARING)
            return self._leases[job_id]

    def reject_offer(self, job_id: str, runner_id: str, now: datetime) -> None:
        with self._lock:
            offer = self._offers.get(job_id)
            if offer is None or offer.runner_id != runner_id:
                raise OfferError("job offer is not assigned to this runner")
            if offer.accepted_at is not None or offer.expires_at <= now:
                raise OfferError("job offer is no longer active")
            workflow = self._workflows[offer.workflow_id]
            self._workflows[workflow.id] = replace(workflow, status=WorkflowStatus.CANCELLED)
            self._project_locks.pop(workflow.project_id, None)
            runner = self._runners[runner_id]
            self._runners[runner_id] = replace(runner, active_job_id=None)
            del self._offers[job_id]
            del self._leases[job_id]

    def accept_event(self, event: ExecutionEvent, now: datetime, on_transition: Any | None = None) -> JobLease:
        with self._lock:
            offer = self._offers.get(event.job_id)
            if offer is None or offer.accepted_at is None:
                raise StaleLeaseError("job has not been accepted")
            lease = accept_event(self._leases[event.job_id], event, now)
            self._leases[event.job_id] = lease
            workflow = self._workflows.get(offer.workflow_id)
        if on_transition is not None and workflow is not None:
            from moirai.workflows.runner_events import (
                validate_runner_event,
                workflow_transition_for_terminal_event,
            )
            summary = validate_runner_event(event.event_type, event.execution_id, event.payload)
            transition = workflow_transition_for_terminal_event(summary, workflow.status.value)
            if transition is not None:
                on_transition(workflow.id, transition.new_status, transition.state_updates)
        return lease

    def renew_lease(
        self, job_id: str, runner_id: str, generation: int, expires_at: datetime, now: datetime
    ) -> JobLease:
        with self._lock:
            offer = self._offers.get(job_id)
            if offer is None or offer.accepted_at is None or offer.runner_id != runner_id:
                raise StaleLeaseError("runner does not own an accepted job")
            lease = renew_lease(self._leases[job_id], generation, expires_at, now)
            self._leases[job_id] = lease
            return lease

    def resolve_job_secret(
        self, runner_id: str, job_id: str, generation: int, name: str, now: datetime
    ) -> tuple[str, str] | None:
        """The in-memory twin of the asyncpg resolver, fenced the same way.

        Credentials are held per project by `set_project_credential` below;
        tests that never set one get None, which is the "fall back to the
        runner's own environment" answer.
        """
        kind = CREDENTIAL_KIND_BY_ENVIRONMENT_NAME.get(name)
        if kind is None:
            raise ValueError(f"no project credential backs the environment reference {name!r}")
        with self._lock:
            offer = self._offers.get(job_id)
            lease = self._leases.get(job_id)
            if (
                offer is None
                or offer.accepted_at is None
                or offer.runner_id != runner_id
                or lease is None
                or lease.generation != generation
                or lease.expires_at <= now
            ):
                raise StaleLeaseError("runner does not hold this job at this lease generation")
            workflow = self._workflows[offer.workflow_id]
            value = self._project_credentials.get((workflow.project_id, kind))
        if value is None:
            return None
        return value, CREDENTIAL_DELIVERY[kind]

    def append_audit(
        self,
        actor_user_id: str | None,
        action: str,
        resource_type: str,
        resource_id: str,
        outcome: str,
        now: datetime,
    ) -> None:
        """Records an audit entry in memory.

        Present so the reduced-capability path is not missing a method the
        servicers call -- handing out a secret without recording it would
        otherwise be an AttributeError at the worst possible moment.
        """
        with self._lock:
            self._audit.append((actor_user_id, action, resource_type, resource_id, outcome, now))

    def set_project_credential(self, project_id: str, kind: str, value: str) -> None:
        """Test seam: the real control plane seals this, here it is just held."""
        if kind not in CREDENTIAL_DELIVERY:
            raise ValueError(f"unknown credential kind: {kind}")
        with self._lock:
            self._project_credentials[(project_id, kind)] = value

    def expire_offers(self, now: datetime) -> tuple[str, ...]:
        expired: list[str] = []
        with self._lock:
            for job_id, offer in tuple(self._offers.items()):
                if offer.accepted_at is not None or offer.expires_at > now:
                    continue
                expired.append(job_id)
                workflow = self._workflows[offer.workflow_id]
                self._workflows[workflow.id] = replace(workflow, status=WorkflowStatus.CANCELLED)
                self._project_locks.pop(workflow.project_id, None)
                runner = self._runners[offer.runner_id]
                self._runners[runner.id] = replace(runner, active_job_id=None)
                del self._offers[job_id]
                del self._leases[job_id]
        return tuple(expired)

    def expire_leases(self, now: datetime) -> tuple[str, ...]:
        expired: list[str] = []
        with self._lock:
            for job_id, offer in self._offers.items():
                if offer.accepted_at is None:
                    continue
                lease = self._leases[job_id]
                workflow = self._workflows[offer.workflow_id]
                if lease.expires_at > now or workflow.status is WorkflowStatus.RECOVERING:
                    continue
                expired.append(job_id)
                self._leases[job_id] = replace(lease, generation=lease.generation + 1)
                self._workflows[workflow.id] = replace(workflow, status=WorkflowStatus.RECOVERING)
                runner = self._runners[offer.runner_id]
                self._runners[runner.id] = replace(runner, active_job_id=None, connected=False, healthy=False)
        return tuple(expired)

    def snapshot(self) -> tuple[tuple[Workflow, ...], tuple[Runner, ...], tuple[str, ...]]:
        with self._lock:
            return (
                tuple(self._workflows.values()),
                tuple(self._runners.values()),
                tuple(self._project_locks.values()),
            )
