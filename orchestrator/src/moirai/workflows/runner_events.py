from __future__ import annotations

from dataclasses import dataclass, field
from typing import Any, Literal

from .schema_validation import SchemaNotFoundError, load_schema, validate

# The runner event vocabulary is a shared contract (runner
# control.validEventType, the ExecutionEvent proto, app.jobs.status). An
# agent-reported block is deliberately *not* a new type: it is a `failed`
# event refined by the `blocked` marker in its payload. See the decision
# recorded in PROGRESS.md for issue #97.
VALID_EVENT_TYPES = frozenset({"started", "progress", "log", "completed", "failed", "cancelled"})
TERMINAL_EVENT_TYPES = frozenset({"completed", "failed", "cancelled"})
MAX_PAYLOAD_FIELDS = 32
# app.workflow_runs.blocking_reason is TEXT but is truncated to 1024 characters
# by the circuit writer; composing a longer reason here would only be cut later.
MAX_BLOCKING_REASON_CHARS = 1024


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
    # The agent's own account of a non-delivering outcome. `blocked` marks the
    # run as "the agent stopped and said why" rather than "the process died";
    # `summary_text` and `remaining_work` carry the explanation, and survive the
    # runner's reduced-payload retry that drops `result`.
    blocked: bool = False
    summary_text: str | None = None
    remaining_work: list[str] = field(default_factory=list)

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
    summary_text: str | None = None
    remaining_work: list[str] = []

    raw_blocked = payload.get("blocked")
    if raw_blocked is not None and not isinstance(raw_blocked, bool):
        raise RunnerEventError("runner event blocked must be a boolean")
    raw_summary = payload.get("summary")
    if raw_summary is not None:
        if not isinstance(raw_summary, str):
            raise RunnerEventError("runner event summary must be a string")
        summary_text = raw_summary
    raw_remaining = payload.get("remainingWork")
    if raw_remaining is not None:
        if not isinstance(raw_remaining, list) or any(not isinstance(w, str) for w in raw_remaining):
            raise RunnerEventError("runner event remainingWork must be a list of strings")
        remaining_work = list(raw_remaining)

    if terminal:
        raw_exit = payload.get("exitCode")
        if raw_exit is not None:
            if not isinstance(raw_exit, int):
                raise RunnerEventError("runner event exitCode must be an integer")
            exit_code = int(raw_exit)
        # The result document is the agent's structured account of what it did.
        # It is read for every terminal outcome, not only a success: a run that
        # stopped without delivering is exactly when the orchestrator most needs
        # the agent's own reasoning (issue #97).
        raw_result = payload.get("result")
        if raw_result is not None:
            if not isinstance(raw_result, dict):
                raise RunnerEventError("runner event result must be an object")
            result = raw_result

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
        result=result,
        # A block refines a failure. A `completed` or `cancelled` event carrying
        # the marker contradicts the outcome the runner reported, and the
        # outcome wins.
        blocked=bool(raw_blocked) and event_type == "failed",
        summary_text=summary_text,
        remaining_work=remaining_work,
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
    "verifier": "verify",
    "repairer": "repair",
}
_SUFFIX_TO_ROLE = {suffix: role for role, suffix in ROLE_TO_SUFFIX.items()}

# Execution types keyed by the same suffixes as ROLE_TO_SUFFIX, plus the
# "pipeline" execution which has no associated workflow-execution-request role.
_SUFFIX_TO_EXECUTION_TYPE = {
    "plan": "run_planner",
    "implement": "run_developer",
    "review": "run_reviewer",
    "verify": "run_verifier",
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
    try:
        schema = load_schema(schema_name)
    except SchemaNotFoundError:
        return None
    if validate(result, schema):
        return None
    value = result.get(field)
    return value if isinstance(value, str) else None


def _agent_block_transition(
    summary: RunnerEventSummary,
    role: str | None,
) -> WorkflowTransition:
    """Routes an agent-reported block to the terminal `blocked` status with the
    agent's own reason recorded, and clears the gate the reporting role owns so
    a resumed graph can never read a stale approval."""
    reason = _agent_blocking_reason(summary, role)
    state_updates: dict[str, object] = {
        "status": "blocked",
        "blocking_reason": reason,
        "last_delivery_outcome": "agent_reported_blocked",
        "last_gate_verdict": reason,
        "remaining_work": list(summary.remaining_work),
    }
    if role == "planner":
        state_updates["plan_valid"] = False
    elif role == "reviewer":
        state_updates["review_approved"] = False
    return WorkflowTransition(new_status="blocked", state_updates=state_updates)


def _agent_blocking_reason(summary: RunnerEventSummary, role: str | None) -> str:
    """Renders the block as one durable line. `remaining_work` is folded in
    rather than stored separately: `blocking_reason` is the only queryable
    column a blocked run has, and a reason without the outstanding work is the
    degraded signal this issue exists to remove."""
    explanation = (summary.summary_text or "").strip() or "no reason was supplied"
    reason = f"{role or 'agent'} reported blocked: {explanation}"
    outstanding = [item.strip() for item in summary.remaining_work if item.strip()]
    if outstanding:
        reason = f"{reason} | remaining work: " + "; ".join(outstanding)
    return reason[:MAX_BLOCKING_REASON_CHARS]


def _human_question(summary: RunnerEventSummary, field: str, fallback: str) -> str:
    result = summary.result or {}
    values = result.get(field)
    questions = [value.strip() for value in values if isinstance(value, str) and value.strip()] if isinstance(values, list) else []
    question = "\n".join(questions) or str(result.get("summary") or "").strip() or fallback
    return question[:MAX_BLOCKING_REASON_CHARS]


def workflow_transition_for_terminal_event(
    summary: RunnerEventSummary,
    current_status: str,
    role: str | None = None,
) -> WorkflowTransition | None:
    transition = _terminal_event_transition(summary, current_status, role)
    if transition is None:
        return None
    # A terminal execution event is precisely the signal the suspended graph is
    # waiting for (see issue_graph.suspend_after_dispatch). Clearing the gate is
    # what lets the dispatching node's outgoing edge advance on resume instead
    # of routing straight back to END; without it the run would never progress.
    return WorkflowTransition(
        new_status=transition.new_status,
        state_updates={**transition.state_updates, "awaiting_execution": False},
    )


def _non_delivery_verdict(summary: RunnerEventSummary) -> str | None:
    reasons: list[str] = []
    if not _valid_agent_result(summary.result):
        reasons.append("agent result is invalid")
    if not summary.changed_files:
        reasons.append("no changed files")
    remaining = [item.strip() for item in summary.remaining_work if item.strip()]
    if remaining:
        reasons.append("remaining work: " + "; ".join(remaining))
    return None if not reasons else "returned without evidence: " + "; ".join(reasons)


def _valid_agent_result(result: dict[str, Any] | None) -> bool:
    if result is None:
        return False
    try:
        return not validate(result, load_schema("agent-result"))
    except (SchemaNotFoundError, ValueError):
        return False


def _returned_without_evidence_transition(
    summary: RunnerEventSummary, current_status: str
) -> WorkflowTransition:
    verdict = _non_delivery_verdict(summary)
    assert verdict is not None
    return WorkflowTransition(
        new_status=current_status,
        state_updates={
            "status": current_status,
            "continuation_requested": True,
            "last_delivery_outcome": "returned_without_evidence",
            "last_gate_verdict": verdict,
            "remaining_work": list(summary.remaining_work),
        },
    )


def _terminal_event_transition(
    summary: RunnerEventSummary,
    current_status: str,
    role: str | None,
) -> WorkflowTransition | None:
    if not summary.terminal:
        return None

    resolved_role = role if role is not None else execution_role_from_id(summary.execution_id)

    # The one and only producer of `pipeline_passed`. The local pipeline is the
    # workflow's deterministic completion gate, so the gate may only be written
    # by the execution that actually ran those commands: never inferred from
    # another role's exit code (an agent process exiting 0 is evidence that the
    # process ended, not that the change builds or passes its tests).
    if resolved_role == "pipeline":
        return WorkflowTransition(
            new_status="local_pipeline",
            state_updates={"status": "local_pipeline", "pipeline_passed": summary.succeeded},
        )

    # An agent that reported `blocked` stopped deliberately and said why. That
    # is a materially different outcome from a crash: "blocked, here is the
    # reason, here is what remains" is the difference between an informed
    # escalation and a blind retry, so it is routed before the generic failure
    # arm rather than being folded into it (issue #97).
    if summary.blocked:
        return _agent_block_transition(summary, resolved_role)

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
            question = _human_question(summary, "questions", "planner requires human input")
            return WorkflowTransition(
                new_status="waiting_human",
                state_updates={
                    "status": "waiting_human", "plan_valid": False, "human_question": question,
                    "human_resume_phase": "planning", "blocking_reason": question,
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
        if current_status == "implementing" and _non_delivery_verdict(summary) is not None:
            return _returned_without_evidence_transition(summary, current_status)
        if current_status == "implementing":
            # Hand the run to the local_pipeline phase without touching
            # `pipeline_passed`: the developer's exit code is not evidence that
            # the deterministic checks pass. Leaving the gate to the pipeline
            # execution is what makes the `pipeline` node dispatch a real run
            # instead of short-circuiting into AI review.
            return WorkflowTransition(
                new_status="local_pipeline",
                state_updates={"status": "local_pipeline", "last_delivery_outcome": "delivered"},
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
            question = _human_question(summary, "findings", "reviewer requires human input")
            return WorkflowTransition(
                new_status="waiting_human",
                state_updates={
                    "status": "waiting_human", "review_approved": False, "human_question": question,
                    "human_resume_phase": "ai_review", "blocking_reason": question,
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

    if resolved_role == "verifier":
        verdict = _schema_field(summary.result, "verifier-result", "verdict")
        if verdict == "passed":
            return WorkflowTransition(
                new_status="ai_review",
                state_updates={"status": "ai_review", "verification_passed": True},
            )
        if verdict == "human_required":
            question = _human_question(summary, "summary", "verifier requires human input")
            return WorkflowTransition(
                new_status="waiting_human",
                state_updates={
                    "status": "waiting_human", "verification_passed": False, "human_question": question,
                    "human_resume_phase": "ai_review", "blocking_reason": question,
                },
            )
        return WorkflowTransition(
            new_status="recovering",
            state_updates={"status": "recovering", "verification_passed": False},
        )

    if resolved_role == "repairer":
        if current_status == "repairing" and _non_delivery_verdict(summary) is not None:
            return _returned_without_evidence_transition(summary, current_status)
        # Like the developer branch: back to local_pipeline with the gate
        # untouched, so the repaired tree is re-validated by a real pipeline
        # execution before the workflow can advance.
        return WorkflowTransition(
            new_status="local_pipeline",
            state_updates={"status": "local_pipeline", "last_delivery_outcome": "delivered"},
        )

    return None
