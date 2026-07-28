from __future__ import annotations

from dataclasses import dataclass, field
from datetime import datetime
from enum import StrEnum
from typing import Any


class WorkflowStatus(StrEnum):
    OFFERED = "offered"
    PREPARING = "preparing"
    PLANNING = "planning"
    IMPLEMENTING = "implementing"
    LOCAL_PIPELINE = "local_pipeline"
    REPAIRING = "repairing"
    AI_REVIEW = "ai_review"
    PUSHING = "pushing"
    PR_CREATED = "pr_created"
    WAITING_GITHUB_CHECKS = "waiting_github_checks"
    WAITING_HUMAN = "waiting_human"
    MERGING = "merging"
    RECOVERING = "recovering"
    COMPLETED = "completed"
    BLOCKED = "blocked"
    FAILED = "failed"
    CANCELLED = "cancelled"

    @property
    def terminal(self) -> bool:
        return self in {self.COMPLETED, self.BLOCKED, self.FAILED, self.CANCELLED}


@dataclass(frozen=True)
class Project:
    id: str
    enabled: bool
    required_runner_labels: frozenset[str] = frozenset()
    repository_url: str | None = None


@dataclass(frozen=True)
class Issue:
    id: str
    project_id: str
    external_id: str
    priority: int
    created_at: datetime
    queued_at: datetime
    eligible: bool


@dataclass(frozen=True)
class Runner:
    id: str
    labels: frozenset[str]
    connected: bool
    enabled: bool
    draining: bool
    healthy: bool
    active_job_id: str | None = None
    capacity: int = 1

    @property
    def available(self) -> bool:
        return (
            self.connected
            and self.enabled
            and not self.draining
            and self.healthy
            and self.active_job_id is None
        )


@dataclass(frozen=True)
class Workflow:
    id: str
    project_id: str
    issue_id: str
    status: WorkflowStatus


@dataclass(frozen=True)
class JobLease:
    job_id: str
    runner_id: str
    generation: int
    expires_at: datetime
    last_event_sequence: int = 0


@dataclass(frozen=True)
class ExecutionEvent:
    job_id: str
    runner_id: str
    lease_generation: int
    event_sequence: int
    event_type: str = ""
    execution_id: str = ""
    payload: dict[str, Any] = field(default_factory=dict)
