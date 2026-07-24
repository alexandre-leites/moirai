from __future__ import annotations

from dataclasses import replace
from datetime import datetime

from .models import ExecutionEvent, JobLease


class StaleLeaseError(ValueError):
    pass


class EventSequenceError(ValueError):
    pass


def accept_event(lease: JobLease, event: ExecutionEvent, now: datetime) -> JobLease:
    if lease.job_id != event.job_id or lease.runner_id != event.runner_id:
        raise StaleLeaseError("event does not belong to the active lease")
    if lease.generation != event.lease_generation or lease.expires_at <= now:
        raise StaleLeaseError("lease generation is stale or expired")
    if event.event_sequence <= lease.last_event_sequence:
        raise EventSequenceError("event sequence must be strictly increasing")
    return replace(lease, last_event_sequence=event.event_sequence)


def renew_lease(lease: JobLease, generation: int, expires_at: datetime, now: datetime) -> JobLease:
    if generation != lease.generation or lease.expires_at <= now:
        raise StaleLeaseError("cannot renew a stale lease")
    if expires_at <= now:
        raise ValueError("renewed lease expiry must be in the future")
    return replace(lease, expires_at=expires_at)
