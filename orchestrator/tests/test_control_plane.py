from datetime import UTC, datetime, timedelta
import unittest

from moirai.domain import (
    AuthenticationError,
    ExecutionEvent,
    InMemoryControlPlane,
    Issue,
    OfferError,
    Project,
    RegistrationError,
    StaleLeaseError,
    WorkflowStatus,
)


NOW = datetime(2026, 1, 1, tzinfo=UTC)


class ControlPlaneTests(unittest.TestCase):
    def setUp(self) -> None:
        self.control_plane = InMemoryControlPlane()
        self.control_plane.add_project(Project("project-a", True, frozenset({"docker"})))
        self.control_plane.add_project(Project("project-b", True, frozenset({"docker"})))

    def register_connected_runner(self) -> tuple[str, str]:
        token = self.control_plane.create_registration_token({"docker", "opencode"}, NOW + timedelta(hours=1))
        runner, credential = self.control_plane.register_runner(token, "runner", {"docker"}, NOW)
        self.control_plane.heartbeat(runner.id, credential, NOW)
        return runner.id, credential

    def test_registration_token_is_one_time_scoped_and_credential_is_required(self) -> None:
        token = self.control_plane.create_registration_token({"docker"}, NOW + timedelta(hours=1))
        with self.assertRaises(RegistrationError):
            self.control_plane.register_runner(token, "runner", {"docker", "opencode"}, NOW)
        runner, credential = self.control_plane.register_runner(token, "runner", {"docker"}, NOW)
        with self.assertRaises(RegistrationError):
            self.control_plane.register_runner(token, "runner-2", {"docker"}, NOW)
        with self.assertRaises(AuthenticationError):
            self.control_plane.heartbeat(runner.id, "wrong", NOW)
        connected = self.control_plane.heartbeat(runner.id, credential, NOW)
        self.assertTrue(connected.available)

    def test_offer_locks_project_and_expiration_releases_runner_and_lock(self) -> None:
        runner_id, _ = self.register_connected_runner()
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 10, NOW, NOW, True))
        scheduled = self.control_plane.schedule(NOW, timedelta(seconds=30))
        self.assertIsNotNone(scheduled)
        assert scheduled is not None
        self.assertEqual(scheduled.assignment.runner.id, runner_id)
        self.assertIsNone(self.control_plane.schedule(NOW, timedelta(seconds=30)))
        self.assertEqual(self.control_plane.expire_offers(NOW + timedelta(seconds=30)), (scheduled.offer.job_id,))
        workflows, runners, locks = self.control_plane.snapshot()
        self.assertEqual(workflows[0].status, WorkflowStatus.CANCELLED)
        self.assertIsNone(runners[0].active_job_id)
        self.assertEqual(locks, ())

    def test_rejected_offer_releases_the_runner_and_project_lock(self) -> None:
        runner_id, _ = self.register_connected_runner()
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 10, NOW, NOW, True))
        scheduled = self.control_plane.schedule(NOW, timedelta(minutes=1))
        assert scheduled is not None
        with self.assertRaises(OfferError):
            self.control_plane.reject_offer(scheduled.offer.job_id, "other-runner", NOW)
        self.control_plane.reject_offer(scheduled.offer.job_id, runner_id, NOW)
        workflows, runners, locks = self.control_plane.snapshot()
        self.assertEqual(workflows[0].status, WorkflowStatus.CANCELLED)
        self.assertIsNone(runners[0].active_job_id)
        self.assertEqual(locks, ())

    def test_only_assigned_runner_can_accept_and_current_lease_events_are_fenced(self) -> None:
        runner_id, _ = self.register_connected_runner()
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 10, NOW, NOW, True))
        scheduled = self.control_plane.schedule(NOW, timedelta(minutes=1))
        assert scheduled is not None
        with self.assertRaises(OfferError):
            self.control_plane.accept_offer(scheduled.offer.job_id, "other-runner", NOW)
        lease = self.control_plane.accept_offer(scheduled.offer.job_id, runner_id, NOW)
        updated = self.control_plane.accept_event(
            ExecutionEvent(lease.job_id, runner_id, lease.generation, 1), NOW
        )
        self.assertEqual(updated.last_event_sequence, 1)
        workflows, _, locks = self.control_plane.snapshot()
        self.assertEqual(workflows[0].status, WorkflowStatus.PREPARING)
        self.assertEqual(locks, (scheduled.workflow.id,))

    def test_only_assigned_runner_can_renew_an_accepted_lease(self) -> None:
        runner_id, _ = self.register_connected_runner()
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 10, NOW, NOW, True))
        scheduled = self.control_plane.schedule(NOW, timedelta(seconds=30))
        assert scheduled is not None
        lease = self.control_plane.accept_offer(scheduled.offer.job_id, runner_id, NOW)
        renewed = self.control_plane.renew_lease(
            lease.job_id,
            runner_id,
            lease.generation,
            NOW + timedelta(seconds=60),
            NOW,
        )
        self.assertEqual(renewed.expires_at, NOW + timedelta(seconds=60))
        with self.assertRaises(StaleLeaseError):
            self.control_plane.renew_lease(
                lease.job_id,
                "other-runner",
                lease.generation,
                NOW + timedelta(seconds=60),
                NOW,
            )

    def test_expired_accepted_lease_preserves_project_lock_for_recovery(self) -> None:
        runner_id, _ = self.register_connected_runner()
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 10, NOW, NOW, True))
        scheduled = self.control_plane.schedule(NOW, timedelta(seconds=30))
        assert scheduled is not None
        lease = self.control_plane.accept_offer(scheduled.offer.job_id, runner_id, NOW)
        self.assertEqual(self.control_plane.expire_leases(NOW + timedelta(seconds=30)), (lease.job_id,))
        workflows, runners, locks = self.control_plane.snapshot()
        self.assertEqual(workflows[0].status, WorkflowStatus.RECOVERING)
        self.assertIsNone(runners[0].active_job_id)
        self.assertEqual(locks, (scheduled.workflow.id,))
        with self.assertRaises(StaleLeaseError):
            self.control_plane.accept_event(ExecutionEvent(lease.job_id, runner_id, lease.generation, 1), NOW + timedelta(seconds=30))


if __name__ == "__main__":
    unittest.main()
