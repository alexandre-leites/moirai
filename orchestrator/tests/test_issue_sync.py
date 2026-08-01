import asyncio
import unittest
from dataclasses import replace
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
    def __init__(
        self, issues: list[ExternalIssue], fail: bool = False, label_writes_fail: bool = False
    ) -> None:
        self._issues = issues
        self._fail = fail
        self.label_writes_fail = label_writes_fail
        self.added: list[tuple[str, list[str]]] = []
        self.removed: list[tuple[str, list[str]]] = []

    async def list_open_issues(self) -> list[ExternalIssue]:
        if self._fail:
            raise RuntimeError("tracker unavailable")
        return self._issues

    async def add_labels(self, external_id: str, labels: list[str]) -> None:
        if self.label_writes_fail:
            raise RuntimeError("label write forbidden")
        self.added.append((external_id, labels))
        self._mutate(external_id, lambda current: current | set(labels))

    async def remove_labels(self, external_id: str, labels: list[str]) -> None:
        if self.label_writes_fail:
            raise RuntimeError("label write forbidden")
        self.removed.append((external_id, labels))
        self._mutate(external_id, lambda current: current - set(labels))

    def _mutate(self, external_id: str, change: Any) -> None:
        for index, issue in enumerate(self._issues):
            if issue.external_id == external_id:
                self._issues[index] = replace(issue, labels=tuple(sorted(change(set(issue.labels)))))


def _workflow(
    status: str,
    external_id: str = "42",
    issue_id: str = "issue-42",
    run_id: str = "run-1",
    created_at: datetime = NOW,
) -> dict[str, Any]:
    return {
        "project_id": "project-1",
        "id": run_id,
        "issue_id": issue_id,
        "external_id": external_id,
        "status": status,
        "created_at": created_at,
    }


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
        self.provider_failures: list[tuple[str, str, datetime]] = []
        self.provider_clears: list[tuple[str, datetime]] = []
        self.retry_state: list[dict[str, object]] = []

    async def upsert_issue(self, **kwargs: Any) -> None:
        self.upserted.append(kwargs)
        self.issue_labels[f"issue-{kwargs['external_id']}"] = sorted(kwargs["labels"])

    async def list_enabled_projects(self) -> list[Project]:
        return [p for p in self.projects if p.enabled]

    async def list_latest_workflow_runs_for_project(self, project_id: str) -> list[dict[str, Any]]:
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

    async def record_provider_failure(self, provider: str, reason: str, now: datetime) -> None:
        self.provider_failures.append((provider, reason, now))

    async def clear_provider_failure(self, provider: str, now: datetime) -> None:
        self.provider_clears.append((provider, now))

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
        control_plane.active_workflows = [_workflow("waiting_human", issue_id="issue-1")]
        control_plane.issue_labels["issue-1"] = ["agent:ready"]
        await sync.reconcile_project_labels(Project("project-1", True))
        self.assertEqual(tracker.added, [("42", ["agent:human-approval", "agent:running"])])
        self.assertEqual(tracker.removed, [("42", ["agent:ready"])])
        self.assertEqual(control_plane.issue_labels["issue-1"], ["agent:human-approval", "agent:running"])

    async def test_label_reconciliation_is_idempotent_after_persistence(self) -> None:
        sync, control_plane, tracker = self._sync()
        control_plane.active_workflows = [_workflow("blocked", issue_id="issue-1")]
        control_plane.issue_labels["issue-1"] = ["agent:blocked"]
        await sync.reconcile_project_labels(Project("project-1", True))
        self.assertEqual(tracker.added, [])
        self.assertEqual(tracker.removed, [])

    async def test_terminal_workflows_receive_blocked_and_delivered_labels(self) -> None:
        sync, control_plane, tracker = self._sync()
        control_plane.active_workflows = [
            _workflow("blocked", external_id="42", issue_id="issue-blocked"),
            _workflow("completed", external_id="43", issue_id="issue-delivered", run_id="run-2"),
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

    async def test_label_reconciliation_never_removes_labels_outside_the_agent_namespace(self) -> None:
        sync, control_plane, tracker = self._sync()
        control_plane.active_workflows = [_workflow("implementing")]
        control_plane.issue_labels["issue-42"] = [
            "agent:ready",
            "agent-priority:5",
            "bug",
            "enhancement",
            "needs-design",
        ]
        await sync.reconcile_project_labels(Project("project-1", True))
        self.assertEqual(tracker.added, [("42", ["agent:running"])])
        self.assertEqual(tracker.removed, [("42", ["agent:ready"])])
        self.assertEqual(
            control_plane.issue_labels["issue-42"],
            ["agent-priority:5", "agent:running", "bug", "enhancement", "needs-design"],
        )

    async def test_label_reconciliation_converges_on_the_newest_run_in_any_order(self) -> None:
        older = _workflow("blocked", run_id="run-old", created_at=NOW - timedelta(hours=2))
        newer = _workflow("completed", run_id="run-new", created_at=NOW)
        for workflows in ([older, newer], [newer, older]):
            with self.subTest(order=[w["id"] for w in workflows]):
                sync, control_plane, tracker = self._sync()
                control_plane.active_workflows = list(workflows)
                control_plane.issue_labels["issue-42"] = ["agent:running"]
                await sync.reconcile_project_labels(Project("project-1", True))
                self.assertEqual(tracker.added, [("42", ["agent:delivered"])])
                self.assertEqual(tracker.removed, [("42", ["agent:running"])])
                self.assertEqual(control_plane.issue_labels["issue-42"], ["agent:delivered"])

    async def test_repeated_sync_cycles_preserve_user_priority_and_triage_labels(self) -> None:
        issue = ExternalIssue(
            external_id="42",
            title="Fix bug",
            body="Body",
            state="open",
            labels=("agent:ready", "agent-priority:10", "bug"),
            created_at=NOW - timedelta(days=1),
            updated_at=NOW,
        )
        sync, control_plane, tracker = self._sync([issue])
        control_plane.active_workflows = [_workflow("implementing")]

        for cycle in range(3):
            with self.subTest(cycle=cycle):
                await sync.sync_all_projects(NOW + timedelta(seconds=60 * cycle))
                self.assertEqual(control_plane.upserted[-1]["priority"], 10)
                self.assertIn("bug", tracker._issues[0].labels)
                self.assertIn("agent-priority:10", tracker._issues[0].labels)
        self.assertEqual(tracker.removed, [("42", ["agent:ready"])])
        self.assertEqual(tracker.added, [("42", ["agent:running"])])

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

    async def test_label_write_failures_never_open_the_provider_circuit(self) -> None:
        """Issue #92 step 5: the provider circuit gates *scheduling* for every
        project on that provider. A refused `agent:*` label write is a failure
        to mirror status onto an issue -- the read that scheduling depends on
        succeeded -- so it may back this project's sync off and nothing more."""
        sync, control_plane, tracker = self._sync()
        tracker.label_writes_fail = True
        control_plane.active_workflows = [_workflow("completed")]

        results = await sync.sync_all_projects(NOW)

        self.assertIn("label reconciliation failed", str(results["project-1"]))
        self.assertEqual(control_plane.provider_failures, [])
        self.assertEqual(control_plane.provider_clears, [("github", NOW)])
        self.assertEqual(control_plane.sync_failures[0][0], "project-1")

    def _two_project_sync(
        self, trackers: dict[str, _FakeTracker]
    ) -> tuple[IssueSync, _FakeControlPlane]:
        control_plane = _FakeControlPlane()
        control_plane.projects = [
            Project("project-1", True, frozenset({"linux"})),
            Project("project-2", True, frozenset({"linux"})),
        ]
        return (
            IssueSync(
                control_plane=control_plane,
                issue_tracker_factory=lambda project: trackers[project.id],
            ),
            control_plane,
        )

    async def test_one_broken_project_never_opens_the_shared_provider_circuit(self) -> None:
        """Issue #92: the provider circuit is global, so it must only be moved
        by evidence about the provider. One project failing while another syncs
        is a project fault -- a deleted repository, a bad URL -- and opening the
        circuit for it would stop scheduling every other project on GitHub.
        Deciding it per project was incoherent in both directions: the same pass
        recorded a failure and then cleared it, in whichever order the projects
        happened to be listed."""
        sync, control_plane = self._two_project_sync(
            {
                "project-1": _FakeTracker([_external_issue()], fail=True),
                "project-2": _FakeTracker([_external_issue()]),
            }
        )

        results = await sync.sync_all_projects(NOW)

        self.assertIsInstance(results["project-1"], str)
        self.assertEqual(results["project-2"], 1)
        self.assertEqual(control_plane.provider_failures, [])
        self.assertEqual(control_plane.provider_clears, [("github", NOW)])
        # The broken project is still backed off on its own.
        self.assertEqual([failure[0] for failure in control_plane.sync_failures], ["project-1"])

    async def test_a_full_outage_records_a_failure_on_every_pass(self) -> None:
        """The suppression above must not disarm the circuit during a real
        outage. Every project fails together, so every project's backoff is
        the same, and the first delays (5s, 10s, 20s) are all shorter than the
        one-minute sync interval `main.py` runs this on -- no project is
        skipped while the circuit is being opened. Three recorded failures is
        what `record_provider_failure` needs to move the state to `open`."""
        sync, control_plane = self._two_project_sync(
            {
                "project-1": _FakeTracker([_external_issue()], fail=True),
                "project-2": _FakeTracker([_external_issue()], fail=True),
            }
        )

        for minute in range(3):
            await sync.sync_all_projects(NOW + timedelta(minutes=minute))

        self.assertEqual(len(control_plane.provider_failures), 3)
        self.assertEqual(control_plane.provider_clears, [])

    async def test_a_pass_where_every_project_fails_records_one_provider_failure(self) -> None:
        """Every attempted project failing is the only evidence issue sync has
        that the provider itself is down -- and it is recorded once for the
        pass, not once per project, so a fleet of projects cannot open the
        circuit on the first outage tick."""
        sync, control_plane = self._two_project_sync(
            {
                "project-1": _FakeTracker([_external_issue()], fail=True),
                "project-2": _FakeTracker([_external_issue()], fail=True),
            }
        )

        await sync.sync_all_projects(NOW)

        self.assertEqual(len(control_plane.provider_failures), 1)
        self.assertEqual(control_plane.provider_failures[0][0], "github")
        self.assertIn("project-1", control_plane.provider_failures[0][1])
        self.assertEqual(control_plane.provider_clears, [])

    async def test_a_permanently_broken_project_never_accumulates_provider_failures(self) -> None:
        """The regression a per-pass verdict has to avoid: recording a provider
        failure per failing project, with nothing clearing it while a healthy
        project still syncs, opens the shared circuit after three passes and
        halts scheduling for the whole provider."""
        sync, control_plane = self._two_project_sync(
            {
                "project-1": _FakeTracker([_external_issue()], fail=True),
                "project-2": _FakeTracker([_external_issue()]),
            }
        )

        for minute in range(6):
            await sync.sync_all_projects(NOW + timedelta(minutes=minute))

        self.assertEqual(control_plane.provider_failures, [])
        self.assertEqual(len(control_plane.provider_clears), 6)

    async def test_a_backed_off_project_suppresses_the_provider_verdict(self) -> None:
        """The hole an "every *attempted* project failed" rule leaves open.

        A project that is backing off drops out of later passes, so the
        remaining projects become the only evidence. With one project stuck in
        a long backoff (here a token that cannot write `agent:*` labels, which
        must never touch the provider circuit) and one project that then breaks
        on its own, every pass in the gap would read as "every attempted
        project failed" and open the shared circuit while GitHub was healthy.
        Requiring the whole fleet closes it: a pass that skipped a project
        writes no verdict at all.
        """
        broken = _FakeTracker([_external_issue()])
        sync, control_plane = self._two_project_sync(
            {"project-1": _FakeTracker([_external_issue()], label_writes_fail=True), "project-2": broken}
        )
        control_plane.active_workflows = [_workflow("completed", issue_id="issue-42")]

        # project-1 fails its label write and backs off; project-2 is healthy.
        await sync.sync_all_projects(NOW)
        self.assertIn("project-1", [failure[0] for failure in control_plane.sync_failures])
        control_plane.provider_clears.clear()

        # project-2 now breaks too, while project-1 is still inside its backoff.
        broken._fail = True
        for second in range(1, 5):
            await sync.sync_all_projects(NOW + timedelta(seconds=second))

        self.assertEqual(control_plane.provider_failures, [])
        self.assertEqual(control_plane.provider_clears, [])

    async def test_a_clean_pass_clears_the_provider_circuit_once(self) -> None:
        sync, control_plane = self._two_project_sync(
            {
                "project-1": _FakeTracker([_external_issue()]),
                "project-2": _FakeTracker([_external_issue()]),
            }
        )

        await sync.sync_all_projects(NOW)

        self.assertEqual(control_plane.provider_clears, [("github", NOW)])

    async def test_a_pass_that_synced_nothing_leaves_the_provider_circuit_alone(self) -> None:
        """Backing off every project proves nothing about the provider, so an
        open circuit must not be cleared -- nor opened further -- by a pass that
        made no request."""
        sync, control_plane, _ = self._sync(tracker_fail=True)
        await sync.sync_all_projects(NOW)
        control_plane.provider_clears.clear()
        control_plane.provider_failures.clear()

        results = await sync.sync_all_projects(NOW + timedelta(seconds=1))

        self.assertEqual(results, {"project-1": "issue sync is backing off"})
        self.assertEqual(control_plane.provider_clears, [])
        self.assertEqual(control_plane.provider_failures, [])

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



class SyncFailureReportingTests(unittest.IsolatedAsyncioTestCase):
    """The reason a sync failed has to survive into the error operators read.

    `str(error)` here becomes `issue_sync_state.last_error`, which the console
    renders on its issue-sync card. It is the only place the reason exists. The
    case that prompted this was a private repository with no GitHub token
    configured: the CLI says exactly what is wrong and the wrapper dropped it,
    leaving "issue tracker failed for project <uuid>" and nowhere to go.
    """

    def _sync_with(self, tracker: object) -> IssueSync:
        return IssueSync(
            control_plane=_FakeControlPlane(),
            issue_tracker_factory=lambda project: tracker,
        )

    async def test_tracker_failure_carries_the_underlying_reason(self) -> None:
        class _Unauthenticated:
            async def list_open_issues(self) -> list[ExternalIssue]:
                raise RuntimeError(
                    "gh: To get started with GitHub CLI, please run: gh auth login"
                )

        sync = self._sync_with(_Unauthenticated())
        with self.assertRaises(IssueSyncError) as raised:
            await sync.sync_project(Project("project-1", True), NOW)

        message = str(raised.exception)
        self.assertIn("issue tracker failed", message)
        self.assertIn("gh auth login", message)

    async def test_a_cause_with_no_message_still_names_something(self) -> None:
        class _Silent:
            async def list_open_issues(self) -> list[ExternalIssue]:
                raise RuntimeError()

        sync = self._sync_with(_Silent())
        with self.assertRaises(IssueSyncError) as raised:
            await sync.sync_project(Project("project-1", True), NOW)
        # Never a message that trails off after the colon.
        self.assertTrue(str(raised.exception).endswith("RuntimeError"))
