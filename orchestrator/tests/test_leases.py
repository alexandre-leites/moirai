import unittest
from datetime import UTC, datetime, timedelta

from moirai.domain import (
    EventSequenceError,
    ExecutionEvent,
    JobLease,
    StaleLeaseError,
    accept_event,
    renew_lease,
)

NOW = datetime(2026, 1, 1, tzinfo=UTC)


def lease() -> JobLease:
    return JobLease("job", "runner", 2, NOW + timedelta(seconds=60))


class LeaseTests(unittest.TestCase):
    def test_accepts_new_event_for_current_generation(self) -> None:
        updated = accept_event(lease(), ExecutionEvent("job", "runner", 2, 1), NOW)
        self.assertEqual(updated.last_event_sequence, 1)

    def test_rejects_stale_generation(self) -> None:
        with self.assertRaises(StaleLeaseError):
            accept_event(lease(), ExecutionEvent("job", "runner", 1, 1), NOW)

    def test_rejects_duplicate_sequence(self) -> None:
        with self.assertRaises(EventSequenceError):
            accept_event(lease(), ExecutionEvent("job", "runner", 2, 0), NOW)

    def test_renewal_requires_current_generation_and_future_expiry(self) -> None:
        renewed = renew_lease(lease(), 2, NOW + timedelta(seconds=120), NOW)
        self.assertEqual(renewed.expires_at, NOW + timedelta(seconds=120))
        with self.assertRaises(StaleLeaseError):
            renew_lease(lease(), 1, NOW + timedelta(seconds=120), NOW)
        with self.assertRaises(ValueError):
            renew_lease(lease(), 2, NOW, NOW)
