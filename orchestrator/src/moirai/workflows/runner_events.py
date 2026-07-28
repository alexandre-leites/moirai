from __future__ import annotations

from dataclasses import dataclass
from typing import Any, Literal

from .schema_validation import load_schema, validate

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
    result: dict[str, Any] | None = None

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
    result: dict[str, Any] | None = None

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
        raw_result = payload.get("result")
        if raw_result is not None:
            if not isinstance(raw_result, dict):
                raise RunnerEventError("runner event result must be an object")
            result = raw_result

    return RunnerEventSummary(
        event_type=event_type,
        execution_id=execution_id,
        exit_code=exit_code,
        changed_files=changed_files,
        commands_run=commands_run,
        terminal=terminal,
        result=result,
    )


# Single source of truth for role<->execution-id-suffix mapping. `role` is a
# real column on app.workflow_execution_requests; every other place that needs
# a suffix or an execution type derives it from this mapping instead of
# maintaining its own copy.
ROLE_TO_SUFFIX = {
    "planner": "plan",
    "developer": "implement",
    "pipeline": "pipeline",
    "reviewer": "review",
    "repairer": "repair",
}
_SUFFIX_TO_ROLE = {suffix: role for role, suffix in ROLE_TO_SUFFIX.items()}

# Execution types keyed by the same suffixes as ROLE_TO_SUFFIX, plus the
# "pipeline" execution which has no associated workflow-execution-request role.
_SUFFIX_TO_EXECUTION_TYPE = {
    "plan": "run_planner",
    "implement": "run_developer",
    "review": "run_reviewer",
    "repair": "run_repair",
    "pipeline": "run_local_pipeline",
}


def role_to_suffix(role: str) -> str:
    try:
        return ROLE_TO_SUFFIX[role]
    except KeyError as error:
        raise ValueError("workflow execution request role is invalid") from error


def execution_role_from_id(execution_id: str) -> str | None:
    for suffix, role in _SUFFIX_TO_ROLE.items():
        if execution_id.endswith(f"-{suffix}"):

            return role
    return None


def execution_type_from_id(execution_id: str) -> str | None:
    for suffix, execution_type in _SUFFIX_TO_EXECUTION_TYPE.items():
        if execution_id.endswith(f"-{suffix}"):
            return execution_type
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


def _schema_field(result: dict[str, Any] | None, schema_name: str, field: str) -> str | None:
    """Returns result[field] only if result validates against the named
    schema; otherwise None (covers missing results, malformed JSON shapes,
    and values outside the schema's enum)."""
    if not isinstance(result, dict):
        return None
    schema = load_schema(schema_name)
    if validate(result, schema):
        return None
    value = result.get(field)
    return value if isinstance(value, str) else None


def workflow_transition_for_terminal_event(
    summary: RunnerEventSummary,
    current_status: str,
    role: str | None = None,
) -> WorkflowTransition | None:
    if not summary.terminal:
        return None

    resolved_role = role if role is not None else execution_role_from_id(summary.execution_id)

    if resolved_role == "pipeline" and summary.terminal:
        return WorkflowTransition(
            new_status="local_pipeline",
            state_updates={"status": "local_pipeline", "pipeline_passed": summary.succeeded},
        )

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

    if resolved_role == "planner":
        status = _schema_field(summary.result, "planner-result", "status")
        if status == "ready":
            return WorkflowTransition(
                new_status="implementing",
                state_updates={"status": "implementing", "plan_valid": True},
            )
        if status == "human_required":
            return WorkflowTransition(
                new_status="blocked",
                state_updates={
                    "status": "blocked",
                    "plan_valid": False,
                    "blocking_reason": "planner requires human input",
                },
            )
        if status == "blocked":
            reason = (summary.result or {}).get("summary")
            return WorkflowTransition(
                new_status="blocked",
                state_updates={
                    "status": "blocked",
                    "plan_valid": False,
                    "blocking_reason": reason if isinstance(reason, str) and reason else "planner reported blocked",
                },
            )
        # "invalid" verdict, or a result that failed schema validation / was
        # never sent: do not trust the plan. Stay in planning so the existing
        # plan -> plan retry edge (gated by planning_attempts) applies.
        return WorkflowTransition(
            new_status="planning",
            state_updates={"status": "planning", "plan_valid": False},
        )

    if resolved_role == "developer":
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

    if resolved_role == "reviewer":
        verdict = _schema_field(summary.result, "review-result", "verdict")
        if verdict == "approved":
            return WorkflowTransition(
                new_status="pushing",
                state_updates={"status": "pushing", "review_approved": True},
            )
        if verdict == "human_required":
            return WorkflowTransition(
                new_status="blocked",
                state_updates={
                    "status": "blocked",
                    "review_approved": False,
                    "blocking_reason": "reviewer requires human input",
                },
            )
        # "changes_requested", "invalid", or an unparseable/missing result:
        # never approve. Stay in ai_review so route_after_review (driven by
        # review_approved=False) sends the workflow through the standard
        # repair/re-review cycle instead of pushing.
        return WorkflowTransition(
            new_status="ai_review",
            state_updates={"status": "ai_review", "review_approved": False},
        )

    if resolved_role == "repairer":
        return WorkflowTransition(
            new_status="local_pipeline",
            state_updates={"status": "local_pipeline"},
        )

    return None
