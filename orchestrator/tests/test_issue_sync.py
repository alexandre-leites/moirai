import asyncio
import unittest
from datetime import UTC, datetime, timedelta
from typing import Any

from moirai.domain.issues import ExternalIssue
from moirai.domain.models import Project
from moirai.services.issue_sync import IssueSync, IssueSyncError

NOW = datetime(2026, 1, 1, tzinfo=UTC)


def _external_issue(
    external_id: str = "42",
    title: str = "Fix bug",
    priority_label: str | None = "agent-priority:10",
    eligible: bool = True,
) -> ExternalIssue:
    labels: list[str] = ["agent:ready"]
    if priority_label:
        labels.append(priority_label)
    return ExternalIssue(
        external_id=external_id,
        title=title,
        body="Body",
        state="open",
        labels=tuple(labels) if eligible else (),
        created_at=NOW - timedelta(days=1),
        updated_at=NOW,
    )


class _FakeTracker:
    def __init__(self, issues: list[ExternalIssue], fail: bool = False) -> None:
        self._issues = issues
        self._fail = fail
        self.added: list[tuple[str, list[str]]] = []
        self.removed: list[tuple[str, list[str]]] = []

    async def list_open_issues(self) -> list[ExternalIssue]:
        if self._fail:
            raise RuntimeError("tracker unavailable")
        return self._issues

    async def add_labels(self, external_id: str, labels: list[str]) -> None:
        self.added.append((external_id, labels))

    async def remove_labels(self, external_id: str, labels: list[str]) -> None:
        self.removed.append((external_id, labels))


class _FakeControlPlane:
    def __init__(self) -> None:
        self.upserted: list[dict[str, Any]] = []
        self.projects: list[Project] = [
            Project("project-1", True, frozenset({"linux"})),
        ]
        self.active_workflows: list[dict[str, Any]] = []
        self.issue_labels: dict[str, list[str]] = {}
        self.sync_failures: list[tuple[str, int, datetime, str, datetime]] = []
        self.sync_clears: list[tuple[str, datetime]] = []
        self.retry_state: list[dict[str, object]] = []

    async def upsert_issue(self, **kwargs: Any) -> None:
        self.upserted.append(kwargs)

    async def list_enabled_projects(self) -> list[Project]:
        return [p for p in self.projects if p.enabled]

    async def list_active_workflows_for_project(self, project_id: str) -> list[dict[str, Any]]:
        return [w for w in self.active_workflows if w["project_id"] == project_id]

    async def get_issue_labels(self, issue_id: str) -> list[str]:
        return self.issue_labels.get(issue_id, [])

    async def set_issue_labels(self, issue_id: str, labels: list[str]) -> None:
        self.issue_labels[issue_id] = labels

    async def mark_missing_issues_ineligible(
        self, project_id: str, external_ids: list[str], now: datetime
    ) -> None:
        self.marked_missing = (project_id, external_ids, now)

    async def record_issue_sync_failure(
        self, project_id: str, failures: int, retry_at: datetime, error: str, now: datetime
    ) -> None:
        self.sync_failures.append((project_id, failures, retry_at, error, now))

    async def clear_issue_sync_failure(self, project_id: str, now: datetime) -> None:
        self.sync_clears.append((project_id, now))

    async def issue_sync_retry_state(self, now: datetime) -> list[dict[str, object]]:
        del now
        return self.retry_state


class IssueSyncTests(unittest.IsolatedAsyncioTestCase):
    def _sync(
        self,
        tracker_issues: list[ExternalIssue] | None = None,
        tracker_fail: bool = False,
    ) -> tuple[IssueSync, _FakeControlPlane, _FakeTracker]:
        control_plane = _FakeControlPlane()
        tracker = _FakeTracker(tracker_issues or [_external_issue()], fail=tracker_fail)
        sync = IssueSync(
            control_plane=control_plane,
            issue_tracker_factory=lambda project: tracker,
        )
        return sync, control_plane, tracker

    async def test_restore_retry_state_skips_a_project_until_the_persisted_deadline(self) -> None:
        sync, control_plane, _ = self._sync()
        control_plane.retry_state = [
            {
                "project_id": "project-1",
                "consecutive_failures": 3,
                "next_retry_at": NOW + timedelta(seconds=20),
            }
        ]
        await sync.restore_retry_state(NOW)
        self.assertEqual(await sync.sync_all_projects(NOW), {"project-1": "issue sync is backing off"})

    async def test_sync_project_upserts_each_external_issue(self) -> None:
        sync, control_plane, _ = self._sync(
            [_external_issue("1"), _external_issue("2")]
        )
        project = Project("project-1", True)
        count = await sync.sync_project(project, NOW)
        self.assertEqual(count, 2)
        self.assertEqual(len(control_plane.upserted), 2)
        self.assertEqual(control_plane.upserted[0]["external_id"], "1")
        self.assertEqual(control_plane.upserted[1]["external_id"], "2")

    async def test_sync_project_maps_priority_and_eligibility(self) -> None:
        sync, control_plane, _ = self._sync([_external_issue("42", priority_label="agent-priority:15")])
        project = Project("project-1", True)
        await sync.sync_project(project, NOW)
        self.assertEqual(control_plane.upserted[0]["priority"], 15)
        self.assertTrue(control_plane.upserted[0]["eligible"])
        self.assertEqual(control_plane.marked_missing, ("project-1", ["42"], NOW))

    async def test_label_reconciliation_updates_persisted_labels_after_provider_success(self) -> None:
        sync, control_plane, tracker = self._sync()
        control_plane.active_workflows = [
            {
                "project_id": "project-1",
                "issue_id": "issue-1",
                "external_id": "42",
                "status": "waiting_human",
            }
        ]
        control_plane.issue_labels["issue-1"] = ["agent:ready"]
        await sync.reconcile_project_labels(Project("project-1", True))
        self.assertEqual(tracker.added, [("42", ["agent:human-approval", "agent:running"])])
        self.assertEqual(tracker.removed, [("42", ["agent:ready"])])
        self.assertEqual(control_plane.issue_labels["issue-1"], ["agent:human-approval", "agent:running"])

    async def test_label_reconciliation_is_idempotent_after_persistence(self) -> None:
        sync, control_plane, tracker = self._sync()
        control_plane.active_workflows = [
            {
                "project_id": "project-1",
                "issue_id": "issue-1",
                "external_id": "42",
                "status": "blocked",
            }
        ]
        control_plane.issue_labels["issue-1"] = ["agent:blocked"]
        await sync.reconcile_project_labels(Project("project-1", True))
        self.assertEqual(tracker.added, [])
        self.assertEqual(tracker.removed, [])

    async def test_terminal_workflows_receive_blocked_and_delivered_labels(self) -> None:
        sync, control_plane, tracker = self._sync()
        control_plane.active_workflows = [
            {"project_id": "project-1", "issue_id": "issue-blocked", "external_id": "42", "status": "blocked"},
            {"project_id": "project-1", "issue_id": "issue-delivered", "external_id": "43", "status": "completed"},
        ]
        control_plane.issue_labels = {
            "issue-blocked": ["agent:running"],
            "issue-delivered": ["agent:running"],
        }
        await sync.reconcile_project_labels(Project("project-1", True))
        self.assertEqual(control_plane.issue_labels["issue-blocked"], ["agent:blocked"])
        self.assertEqual(control_plane.issue_labels["issue-delivered"], ["agent:delivered"])
        self.assertEqual(
            tracker.added,
            [("42", ["agent:blocked"]), ("43", ["agent:delivered"])],
        )

    async def test_sync_project_raises_on_tracker_failure(self) -> None:
        sync, _, _ = self._sync(tracker_fail=True)
        project = Project("project-1", True)
        with self.assertRaises(IssueSyncError):
            await sync.sync_project(project, NOW)

    async def test_sync_all_projects_returns_counts_per_project(self) -> None:
        sync, _control_plane, _ = self._sync([_external_issue("1"), _external_issue("2")])
        results = await sync.sync_all_projects(NOW)
        self.assertEqual(results, {"project-1": 2})

    async def test_sync_all_projects_records_error_string_on_tracker_failure(self) -> None:
        sync, _control_plane, _ = self._sync(tracker_fail=True)
        results = await sync.sync_all_projects(NOW)
        self.assertIn("project-1", results)
        self.assertIsInstance(results["project-1"], str)

    async def test_sync_all_projects_backs_off_after_failure_and_resets_after_success(self) -> None:
        sync, control_plane, tracker = self._sync(tracker_fail=True)
        first = await sync.sync_all_projects(NOW)
        self.assertIn("project-1", first)
        self.assertEqual(await sync.sync_all_projects(NOW + timedelta(seconds=4)), {"project-1": "issue sync is backing off"})
        tracker._fail = False
        self.assertEqual(await sync.sync_all_projects(NOW + timedelta(seconds=5)), {"project-1": 1})
        self.assertEqual(control_plane.sync_failures[0][1], 1)
        self.assertEqual(control_plane.sync_clears, [("project-1", NOW + timedelta(seconds=5))])
        tracker._fail = True
        next_result = await sync.sync_all_projects(NOW + timedelta(seconds=6))
        self.assertIn("project-1", next_result)

    async def test_run_stops_when_stop_event_is_set(self) -> None:
        sync, control_plane, _ = self._sync([])
        stop_event = asyncio.Event()
        stop_event.set()
        await sync.run(stop_event, lambda: NOW, timedelta(seconds=1))
        self.assertEqual(control_plane.upserted, [])

    async def test_run_rejects_non_positive_interval(self) -> None:
        sync, _, _ = self._sync()
        with self.assertRaises(ValueError):
            await sync.run(asyncio.Event(), lambda: NOW, timedelta())

    async def test_run_survives_list_enabled_projects_raising_and_keeps_looping(self) -> None:
        sync, control_plane, _ = self._sync()
        stop_event = asyncio.Event()
        calls = 0
        original_list_enabled_projects = control_plane.list_enabled_projects

        async def flaky_list_enabled_projects() -> list[Project]:
            nonlocal calls
            calls += 1
            if calls == 1:
                raise RuntimeError("database connection lost")
            stop_event.set()
            return await original_list_enabled_projects()

        control_plane.list_enabled_projects = flaky_list_enabled_projects  # type: ignore[method-assign]
        await sync.run(stop_event, lambda: NOW, timedelta(milliseconds=1))
        self.assertEqual(calls, 2)

    async def test_run_invokes_on_run_after_each_successful_iteration(self) -> None:
        sync, _, _ = self._sync()
        stop_event = asyncio.Event()
        runs = 0

        def on_run() -> None:
            nonlocal runs
            runs += 1
            stop_event.set()

        await sync.run(stop_event, lambda: NOW, timedelta(milliseconds=1), on_run=on_run)
        self.assertEqual(runs, 1)


if __name__ == "__main__":
    unittest.main()
