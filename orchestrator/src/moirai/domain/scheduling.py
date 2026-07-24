from __future__ import annotations

from dataclasses import dataclass
from typing import Iterable

from .models import Issue, Project, Runner, Workflow


@dataclass(frozen=True)
class Assignment:
    issue: Issue
    runner: Runner


def parse_priority(labels: Iterable[str], prefix: str, default: int = 0) -> tuple[int, tuple[str, ...]]:
    values: list[int] = []
    invalid: list[str] = []
    for label in labels:
        if not label.startswith(prefix):
            continue
        suffix = label[len(prefix) :]
        try:
            values.append(int(suffix))
        except ValueError:
            invalid.append(label)
    return (max(values) if values else default, tuple(invalid))


def eligible_issues(
    projects: Iterable[Project], issues: Iterable[Issue], workflows: Iterable[Workflow]
) -> list[Issue]:
    enabled = {project.id for project in projects if project.enabled}
    locked = {workflow.project_id for workflow in workflows if not workflow.status.terminal}
    candidates = [
        issue
        for issue in issues
        if issue.eligible and issue.project_id in enabled and issue.project_id not in locked
    ]
    return sorted(
        candidates,
        key=lambda issue: (
            -issue.priority,
            issue.created_at,
            issue.queued_at,
            issue.project_id,
            issue.external_id,
        ),
    )


def select_assignment(
    projects: Iterable[Project], issues: Iterable[Issue], workflows: Iterable[Workflow], runners: Iterable[Runner]
) -> Assignment | None:
    project_map = {project.id: project for project in projects}
    available = [runner for runner in runners if runner.available]
    for issue in eligible_issues(project_map.values(), issues, workflows):
        project = project_map[issue.project_id]
        for runner in sorted(available, key=lambda item: item.id):
            if project.required_runner_labels.issubset(runner.labels):
                return Assignment(issue=issue, runner=runner)
    return None
