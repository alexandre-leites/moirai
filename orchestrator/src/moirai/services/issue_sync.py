from __future__ import annotations

import asyncio
import inspect
from collections.abc import Callable
from datetime import datetime, timedelta
from typing import Any

from moirai.domain.issues import LabelPolicy, reconcile_labels, synchronize_issue
from moirai.domain.models import Project
from moirai.issue_trackers.github_cli import GitHubCliIssueTracker, GitHubRepository


async def _await(value: Any) -> Any:
    if inspect.isawaitable(value):
        return await value
    return value


class IssueSyncError(RuntimeError):
    pass


def github_issue_tracker_for_project(project: Project) -> GitHubCliIssueTracker:
    if project.repository_url is None:
        raise IssueSyncError(f"project {project.id} has no GitHub repository URL")
    return GitHubCliIssueTracker(GitHubRepository.from_remote_url(project.repository_url))


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
        projects = await _await(self._control_plane.list_enabled_projects())
        results: dict[str, int | str] = {}
        for project in projects:
            retry_after = self._retry_after.get(project.id)
            if retry_after is not None and retry_after > now:
                results[project.id] = "issue sync is backing off"
                continue
            try:
                count = await self.sync_project(project, now)
                await self.reconcile_project_labels(project)
                self._failure_counts.pop(project.id, None)
                self._retry_after.pop(project.id, None)
                clear_failure = getattr(self._control_plane, "clear_issue_sync_failure", None)
                if clear_failure is not None:
                    await _await(clear_failure(project.id, now))
                results[project.id] = count
            except IssueSyncError as error:
                failures = self._failure_counts.get(project.id, 0) + 1
                retry_at = now + _retry_delay(failures)
                self._failure_counts[project.id] = failures
                self._retry_after[project.id] = retry_at
                record_failure = getattr(self._control_plane, "record_issue_sync_failure", None)
                if record_failure is not None:
                    await _await(record_failure(project.id, failures, retry_at, str(error), now))
                results[project.id] = str(error)
        return results

    async def reconcile_project_labels(self, project: Project) -> None:
        tracker = self._issue_tracker_factory(project)
        active_workflows = await _await(
            self._control_plane.list_active_workflows_for_project(project.id)
        )
        for workflow in active_workflows:
            current_labels = await _await(self._control_plane.get_issue_labels(workflow["issue_id"]))
            desired_labels = _desired_labels_for_workflow(workflow, self._label_policy)
            to_add, to_remove = reconcile_labels(current_labels, desired_labels)
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
                raise IssueSyncError(f"label reconciliation timed out for project {project.id}") from error
            except Exception as error:
                raise IssueSyncError(f"label reconciliation failed for project {project.id}") from error
            set_labels = getattr(self._control_plane, "set_issue_labels", None)
            if set_labels is not None:
                await _await(set_labels(workflow["issue_id"], sorted(set(desired_labels))))

    async def run(
        self,
        stop_event: asyncio.Event,
        now: Callable[[], datetime],
        interval: timedelta,
    ) -> None:
        if interval <= timedelta():
            raise ValueError("issue sync interval must be positive")
        while not stop_event.is_set():
            await self.sync_all_projects(now())
            try:
                await asyncio.wait_for(stop_event.wait(), timeout=interval.total_seconds())
            except TimeoutError:
                pass


def _retry_delay(failures: int) -> timedelta:
    if failures < 1:
        raise ValueError("failure count must be positive")
    return timedelta(seconds=min(300, 5 * 2 ** min(failures - 1, 6)))


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
