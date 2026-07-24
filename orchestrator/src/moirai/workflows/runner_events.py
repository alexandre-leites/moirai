from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Literal


VALID_EVENT_TYPES = frozenset({"started", "progress", "log", "completed", "failed", "cancelled"})
TERMINAL_EVENT_TYPES = frozenset({"completed", "failed", "cancelled"})
MAX_PAYLOAD_FIELDS = 32


class RunnerEventError(ValueError):
    pass


@dataclass(frozen=True)
class RunnerEventSummary:
    event_type: str
    execution_id: str
    exit_code: int | None
    changed_files: list[str]
    commands_run: list[str]
    terminal: bool

    @property
    def succeeded(self) -> bool:
        return self.event_type == "completed"

    @property
    def failed(self) -> bool:
        return self.event_type == "failed"

    @property
    def cancelled(self) -> bool:
        return self.event_type == "cancelled"


def validate_runner_event(
    event_type: str,
    execution_id: str,
    payload: dict[str, Any],
) -> RunnerEventSummary:
    if event_type not in VALID_EVENT_TYPES:
        raise RunnerEventError(f"runner event type is invalid: {event_type!r}")
    if not execution_id or not execution_id.strip():
        raise RunnerEventError("runner execution ID is required")
    if not isinstance(payload, dict):
        raise RunnerEventError("runner event payload must be an object")
    if len(payload) > MAX_PAYLOAD_FIELDS:
        raise RunnerEventError("runner event payload has too many fields")

    terminal = event_type in TERMINAL_EVENT_TYPES
    exit_code: int | None = None
    changed_files: list[str] = []
    commands_run: list[str] = []

    if terminal:
        raw_exit = payload.get("exitCode")
        if raw_exit is not None:
            if not isinstance(raw_exit, int):
                raise RunnerEventError("runner event exitCode must be an integer")
            exit_code = int(raw_exit)

    if event_type == "completed":
        raw_files = payload.get("changedFiles")
        if raw_files is not None:
            if not isinstance(raw_files, list) or any(not isinstance(f, str) for f in raw_files):
                raise RunnerEventError("runner event changedFiles must be a list of strings")
            changed_files = list(raw_files)
        raw_cmds = payload.get("commandsRun")
        if raw_cmds is not None:
            if not isinstance(raw_cmds, list) or any(not isinstance(c, str) for c in raw_cmds):
                raise RunnerEventError("runner event commandsRun must be a list of strings")
            commands_run = list(raw_cmds)

    return RunnerEventSummary(
        event_type=event_type,
        execution_id=execution_id,
        exit_code=exit_code,
        changed_files=changed_files,
        commands_run=commands_run,
        terminal=terminal,
    )


def execution_role_from_id(execution_id: str) -> str | None:
    suffix_to_role = {
        "-plan": "planner",
        "-implement": "developer",
        "-review": "reviewer",
        "-repair": "repairer",
    }
    for suffix, role in suffix_to_role.items():
        if execution_id.endswith(suffix):
            return role
    return None


WorkflowPhase = Literal[
    "planning",
    "implementing",
    "local_pipeline",
    "repairing",
    "ai_review",
    "pushing",
    "recovering",
    "failed",
    "cancelled",
]


@dataclass(frozen=True)
class WorkflowTransition:
    new_status: str
    state_updates: dict[str, object]


def workflow_transition_for_terminal_event(
    summary: RunnerEventSummary,
    current_status: str,
) -> WorkflowTransition | None:
    if not summary.terminal:
        return None

    role = execution_role_from_id(summary.execution_id)

    if summary.cancelled:
        return WorkflowTransition(
            new_status="cancelled",
            state_updates={"status": "cancelled"},
        )

    if summary.failed:
        return WorkflowTransition(
            new_status="recovering",
            state_updates={"status": "recovering"},
        )

    if not summary.succeeded:
        return None

    if role == "planner":
        return WorkflowTransition(
            new_status="implementing",
            state_updates={
                "status": "implementing",
                "plan_valid": True,
            },
        )

    if role == "developer":
        if current_status == "implementing":
            return WorkflowTransition(
                new_status="local_pipeline",
                state_updates={
                    "status": "local_pipeline",
                    "pipeline_passed": summary.exit_code == 0,
                },
            )
        if current_status == "pushing":
            return WorkflowTransition(
                new_status="pr_created",
                state_updates={"status": "pr_created"},
            )
        return None

    if role == "reviewer":
        return WorkflowTransition(
            new_status="pushing",
            state_updates={
                "status": "pushing",
                "review_approved": True,
            },
        )

    if role == "repairer":
        return WorkflowTransition(
            new_status="local_pipeline",
            state_updates={"status": "local_pipeline"},
        )

    return None
