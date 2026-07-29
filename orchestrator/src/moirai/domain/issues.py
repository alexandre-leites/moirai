from __future__ import annotations

from collections.abc import Iterable
from dataclasses import dataclass
from datetime import datetime

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
    # Every label the platform owns starts with this prefix. Reconciliation may
    # only delete labels inside it; everything else on the issue (triage labels
    # and `agent-priority:N`, which is user input read by the scheduler) belongs
    # to humans and must survive every sync pass.
    managed_prefix: str = "agent:"

    def __post_init__(self) -> None:
        if not self.managed_prefix:
            raise ValueError("managed label prefix must not be empty")
        for label in (self.ready, self.running, self.blocked, self.delivered, self.human_approval):
            if not label.startswith(self.managed_prefix):
                raise ValueError(
                    f"state label {label!r} is outside the managed namespace {self.managed_prefix!r}"
                )
        if self.priority_prefix.startswith(self.managed_prefix):
            raise ValueError(
                "priority labels are user input and must stay outside the managed namespace"
            )


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


def reconcile_labels(
    current: Iterable[str], desired: Iterable[str], *, managed_prefix: str
) -> tuple[tuple[str, ...], tuple[str, ...]]:
    """Diff the agent-managed label namespace against the issue's labels.

    Removals are restricted to labels starting with ``managed_prefix`` so a
    reconciliation pass never deletes triage labels, user priority labels, or
    anything else a human applied to the issue.
    """
    if not managed_prefix:
        raise ValueError("managed label prefix must not be empty")
    current_set = frozenset(current)
    desired_set = frozenset(desired)
    managed_current = frozenset(label for label in current_set if label.startswith(managed_prefix))
    return tuple(sorted(desired_set - current_set)), tuple(sorted(managed_current - desired_set))


def _is_integer(value: str) -> bool:
    try:
        int(value)
    except ValueError:
        return False
    return True
