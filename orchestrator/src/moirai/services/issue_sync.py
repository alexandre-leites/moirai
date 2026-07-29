from __future__ import annotations

import asyncio
import inspect
import logging
from collections.abc import Callable, Iterable
from datetime import UTC, datetime, timedelta
from typing import Any

from moirai.domain.issues import LabelPolicy, reconcile_labels, synchronize_issue
from moirai.domain.models import Project
from moirai.issue_trackers.github_cli import CommandRunner, GitHubCliIssueTracker, GitHubRepository

_LOGGER = logging.getLogger(__name__)


async def _await(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


class IssueSyncError(RuntimeError):
    pass


class LabelReconciliationError(IssueSyncError):
    """A failure while writing `agent:*` labels back to the issue tracker.

    Distinct from every other sync failure because of what it does *not* prove
    (issue #92, step 5): the tracker was reachable enough to list issues, and
    the labels it failed to write are a status mirror, not an input to any
    workflow. It must therefore never open the provider circuit, which gates
    scheduling for every project on that provider.
    """


def github_issue_tracker_for_project(
    project: Project, command_runner: CommandRunner | None = None
) -> GitHubCliIssueTracker:
    if project.repository_url is None:
        raise IssueSyncError(f"project {project.id} has no GitHub repository URL")
    return GitHubCliIssueTracker(GitHubRepository.from_remote_url(project.repository_url), command_runner)


class IssueSync:
    def __init__(
        self,
        control_plane: Any,
        issue_tracker_factory: Callable[[Project], Any],
        label_policy: LabelPolicy | None = None,
        sync_timeout_seconds: float = 60.0,
    ) -> None:
        if sync_timeout_seconds <= 0:
            raise ValueError("issue sync timeout must be positive")
        self._control_plane = control_plane
        self._issue_tracker_factory = issue_tracker_factory
        self._label_policy = label_policy or LabelPolicy()
        self._sync_timeout_seconds = sync_timeout_seconds
        self._failure_counts: dict[str, int] = {}
        self._retry_after: dict[str, datetime] = {}

    async def restore_retry_state(self, now: datetime) -> None:
        load_state = getattr(self._control_plane, "issue_sync_retry_state", None)
        if load_state is None:
            return
        for state in await _await(load_state(now)):
            project_id = state.get("project_id")
            failures = state.get("consecutive_failures")
            retry_after = state.get("next_retry_at")
            if (
                isinstance(project_id, str)
                and isinstance(failures, int)
                and failures > 0
                and isinstance(retry_after, datetime)
                and retry_after > now
            ):
                self._failure_counts[project_id] = failures
                self._retry_after[project_id] = retry_after

    async def sync_project(self, project: Project, now: datetime) -> int:
        tracker = self._issue_tracker_factory(project)
        try:
            external_issues = await asyncio.wait_for(
                _await(tracker.list_open_issues()), timeout=self._sync_timeout_seconds
            )
        except TimeoutError as error:
            raise IssueSyncError(f"issue tracker timed out for project {project.id}") from error
        except Exception as error:
            raise IssueSyncError(f"issue tracker failed for project {project.id}") from error

        seen_external_ids: list[str] = []
        for external in external_issues:
            synchronized = synchronize_issue(
                issue_id="",
                project_id=project.id,
                external=external,
                queued_at=now,
                policy=self._label_policy,
            )
            try:
                await _await(
                    self._control_plane.upsert_issue(
                        project_id=project.id,
                        external_id=external.external_id,
                        title=external.title,
                        body=external.body,
                        state=external.state,
                        labels=list(external.labels),
                        priority=synchronized.issue.priority,
                        eligible=synchronized.issue.eligible,
                        human_approval_required=synchronized.human_approval_required,
                        external_created_at=external.created_at,
                        external_updated_at=external.updated_at,
                        now=now,
                    )
                )
            except Exception as error:
                raise IssueSyncError(f"issue persistence failed for project {project.id}") from error
            seen_external_ids.append(external.external_id)

        mark_missing = getattr(self._control_plane, "mark_missing_issues_ineligible", None)
        if mark_missing is not None:
            try:
                await _await(mark_missing(project.id, seen_external_ids, now))
            except Exception as error:
                raise IssueSyncError(f"issue reconciliation failed for project {project.id}") from error
        return len(seen_external_ids)

    async def sync_all_projects(self, now: datetime) -> dict[str, int | str]:
        """Syncs every enabled project and reaches one verdict on the provider.

        The provider circuit is global -- it gates scheduling for every project
        on that provider -- so it is decided once, from the whole pass, and only
        on evidence about the provider itself (issue #92):

        - any project synced: the provider answered, so the circuit is cleared;
        - every project attempted failed: one failure is recorded for the pass;
        - nothing was attempted (all backing off): no verdict, nothing written.

        Deciding it per project was incoherent in both directions. One
        project's success erased the failure another had just recorded in the
        same pass, and one project's permanent fault -- a deleted repository, a
        bad URL -- looked exactly like a provider outage and halted scheduling
        for every other project. A per-project fault is already handled by that
        project's own `app.issue_sync_state` backoff.
        """
        projects = await _await(self._control_plane.list_enabled_projects())
        results: dict[str, int | str] = {}
        attempted = False
        answered = False
        provider_error: str | None = None
        for project in projects:
            retry_after = self._retry_after.get(project.id)
            if retry_after is not None and retry_after > now:
                results[project.id] = "issue sync is backing off"
                continue
            attempted = True
            try:
                count = await self.sync_project(project, now)
                answered = True
                await self.reconcile_project_labels(project)
                self._failure_counts.pop(project.id, None)
                self._retry_after.pop(project.id, None)
                clear_failure = getattr(self._control_plane, "clear_issue_sync_failure", None)
                if clear_failure is not None:
                    await _await(clear_failure(project.id, now))
                results[project.id] = count
            except LabelReconciliationError as error:
                # The tracker answered the read this pass depends on, so the
                # provider counts as reachable; only this project backs off.
                answered = True
                await self._record_sync_failure(project.id, error, now)
                results[project.id] = str(error)
            except IssueSyncError as error:
                if provider_error is None:
                    provider_error = str(error)
                await self._record_sync_failure(project.id, error, now)
                results[project.id] = str(error)
        if answered:
            clear_provider = getattr(self._control_plane, "clear_provider_failure", None)
            if clear_provider is not None:
                await _await(clear_provider("github", now))
        elif attempted and provider_error is not None:
            record_provider = getattr(self._control_plane, "record_provider_failure", None)
            if record_provider is not None:
                await _await(record_provider("github", provider_error, now))
        return results

    async def _record_sync_failure(self, project_id: str, error: Exception, now: datetime) -> None:
        failures = self._failure_counts.get(project_id, 0) + 1
        retry_at = now + _retry_delay(failures)
        self._failure_counts[project_id] = failures
        self._retry_after[project_id] = retry_at
        record_failure = getattr(self._control_plane, "record_issue_sync_failure", None)
        if record_failure is not None:
            await _await(record_failure(project_id, failures, retry_at, str(error), now))

    async def reconcile_project_labels(self, project: Project) -> None:
        tracker = self._issue_tracker_factory(project)
        workflow_runs = await _await(
            self._control_plane.list_latest_workflow_runs_for_project(project.id)
        )
        for workflow in _latest_run_per_issue(workflow_runs):
            current_labels = await _await(self._control_plane.get_issue_labels(workflow["issue_id"]))
            desired_labels = _desired_labels_for_workflow(workflow, self._label_policy)
            to_add, to_remove = reconcile_labels(
                current_labels, desired_labels, managed_prefix=self._label_policy.managed_prefix
            )
            if not to_add and not to_remove:
                continue
            try:
                if to_add:
                    await asyncio.wait_for(
                        _await(tracker.add_labels(workflow["external_id"], list(to_add))),
                        timeout=self._sync_timeout_seconds,
                    )
                if to_remove:
                    await asyncio.wait_for(
                        _await(tracker.remove_labels(workflow["external_id"], list(to_remove))),
                        timeout=self._sync_timeout_seconds,
                    )
            except TimeoutError as error:
                raise LabelReconciliationError(
                    f"label reconciliation timed out for project {project.id}"
                ) from error
            except Exception as error:
                raise LabelReconciliationError(
                    f"label reconciliation failed for project {project.id}"
                ) from error
            set_labels = getattr(self._control_plane, "set_issue_labels", None)
            if set_labels is not None:
                reconciled = (set(current_labels) - set(to_remove)) | set(to_add)
                await _await(set_labels(workflow["issue_id"], sorted(reconciled)))

    async def run(
        self,
        stop_event: asyncio.Event,
        now: Callable[[], datetime],
        interval: timedelta,
        on_run: Callable[[], None] | None = None,
    ) -> None:
        if interval <= timedelta():
            raise ValueError("issue sync interval must be positive")
        while not stop_event.is_set():
            try:
                await self.sync_all_projects(now())
                if on_run is not None:
                    on_run()
            except asyncio.CancelledError:
                raise
            except Exception:
                _LOGGER.exception("issue sync run failed; retrying after backoff")
            try:
                await asyncio.wait_for(stop_event.wait(), timeout=interval.total_seconds())
            except TimeoutError:
                pass


def _retry_delay(failures: int) -> timedelta:
    if failures < 1:
        raise ValueError("failure count must be positive")
    return timedelta(seconds=min(300, 5 * 2 ** min(failures - 1, 6)))


def _latest_run_per_issue(workflow_runs: Iterable[dict[str, Any]]) -> list[dict[str, Any]]:
    """Keep exactly one authoritative workflow run per issue, newest first.

    The control plane already returns one run per issue, but collapsing here as
    well keeps terminal-label convergence deterministic for any control-plane
    implementation that hands back several historical runs: without it, whichever
    row happens to be processed last decides the label, so `agent:blocked` from
    an old run can overwrite `agent:delivered` from the current one.
    """
    latest: dict[str, dict[str, Any]] = {}
    for workflow in workflow_runs:
        issue_id = str(workflow["issue_id"])
        incumbent = latest.get(issue_id)
        if incumbent is None or _run_recency(workflow) > _run_recency(incumbent):
            latest[issue_id] = workflow
    return [latest[issue_id] for issue_id in sorted(latest)]


def _run_recency(workflow: dict[str, Any]) -> tuple[datetime, str]:
    created_at = workflow.get("created_at")
    if not isinstance(created_at, datetime):
        created_at = datetime.min.replace(tzinfo=UTC)
    elif created_at.tzinfo is None:
        created_at = created_at.replace(tzinfo=UTC)
    return created_at, str(workflow.get("id", ""))


def _desired_labels_for_workflow(
    workflow: dict[str, Any], policy: LabelPolicy
) -> tuple[str, ...]:
    status = workflow.get("status", "")
    if status in {
        "offered", "preparing", "planning", "implementing", "local_pipeline", "repairing",
        "ai_review", "pushing", "pr_created", "waiting_github_checks", "merging", "recovering",
    }:
        return (policy.running,)
    if status == "waiting_human":
        return (policy.running, policy.human_approval)
    if status == "blocked":
        return (policy.blocked,)
    if status == "completed":
        return (policy.delivered,)
    return ()
