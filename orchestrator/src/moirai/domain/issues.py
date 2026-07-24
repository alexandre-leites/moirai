from __future__ import annotations

from dataclasses import dataclass
from datetime import datetime
from typing import Iterable

from .models import Issue
from .scheduling import parse_priority


@dataclass(frozen=True)
class LabelPolicy:
    ready: str = "agent:ready"
    running: str = "agent:running"
    blocked: str = "agent:blocked"
    delivered: str = "agent:delivered"
    human_approval: str = "agent:human-approval"
    priority_prefix: str = "agent-priority:"
    default_priority: int = 0


@dataclass(frozen=True)
class ExternalIssue:
    external_id: str
    title: str
    body: str
    state: str
    labels: tuple[str, ...]
    created_at: datetime
    updated_at: datetime


@dataclass(frozen=True)
class SynchronizedIssue:
    issue: Issue
    human_approval_required: bool
    invalid_priority_labels: tuple[str, ...]
    multiple_priority_labels: bool


def is_eligible(labels: Iterable[str], state: str, policy: LabelPolicy) -> bool:
    label_set = frozenset(labels)
    return (
        state.lower() == "open"
        and policy.ready in label_set
        and policy.running not in label_set
        and policy.blocked not in label_set
        and policy.delivered not in label_set
    )


def synchronize_issue(
    issue_id: str,
    project_id: str,
    external: ExternalIssue,
    queued_at: datetime,
    policy: LabelPolicy,
) -> SynchronizedIssue:
    priority, invalid = parse_priority(external.labels, policy.priority_prefix, policy.default_priority)
    parsed_count = sum(
        1
        for label in external.labels
        if label.startswith(policy.priority_prefix)
        and _is_integer(label[len(policy.priority_prefix) :])
    )
    return SynchronizedIssue(
        issue=Issue(
            id=issue_id,
            project_id=project_id,
            external_id=external.external_id,
            priority=priority,
            created_at=external.created_at,
            queued_at=queued_at,
            eligible=is_eligible(external.labels, external.state, policy),
        ),
        human_approval_required=policy.human_approval in external.labels,
        invalid_priority_labels=invalid,
        multiple_priority_labels=parsed_count > 1,
    )


def reconcile_labels(current: Iterable[str], desired: Iterable[str]) -> tuple[tuple[str, ...], tuple[str, ...]]:
    current_set = frozenset(current)
    desired_set = frozenset(desired)
    return tuple(sorted(desired_set - current_set)), tuple(sorted(current_set - desired_set))


def _is_integer(value: str) -> bool:
    try:
        int(value)
    except ValueError:
        return False
    return True
