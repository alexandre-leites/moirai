import unittest
from typing import Any, ClassVar
from unittest.mock import patch

from moirai.workflows.runner_events import (
    MAX_PAYLOAD_FIELDS,
    RunnerEventError,
    RunnerEventSummary,
    execution_role_from_id,
    validate_runner_event,
    workflow_transition_for_terminal_event,
)
from moirai.workflows.schema_validation import SchemaNotFoundError


class ValidateRunnerEventTests(unittest.TestCase):
    def test_started_event_is_non_terminal(self) -> None:
        summary = validate_runner_event("started", "job-1-plan", {"status": "running"})
        self.assertEqual(summary.event_type, "started")
        self.assertFalse(summary.terminal)
        self.assertFalse(summary.succeeded)
        self.assertFalse(summary.failed)
        self.assertFalse(summary.cancelled)
        self.assertIsNone(summary.exit_code)

    def test_completed_event_extracts_fields(self) -> None:
        summary = validate_runner_event(
            "completed",
            "job-1-plan",
            {"status": "completed", "exitCode": 0, "changedFiles": ["a.py"], "commandsRun": ["make test"]},
        )
        self.assertTrue(summary.terminal)
        self.assertTrue(summary.succeeded)
        self.assertEqual(summary.exit_code, 0)
        self.assertEqual(summary.changed_files, ["a.py"])
        self.assertEqual(summary.commands_run, ["make test"])

    def test_failed_event_extracts_exit_code(self) -> None:
        summary = validate_runner_event("failed", "job-1-implement", {"status": "failed", "exitCode": 1})
        self.assertTrue(summary.terminal)
        self.assertTrue(summary.failed)
        self.assertEqual(summary.exit_code, 1)
        self.assertEqual(summary.changed_files, [])

    def test_cancelled_event_is_terminal(self) -> None:
        summary = validate_runner_event("cancelled", "job-1-plan", {"status": "cancelled", "exitCode": -1})
        self.assertTrue(summary.terminal)
        self.assertTrue(summary.cancelled)
        self.assertEqual(summary.exit_code, -1)

    def test_progress_and_log_events_are_non_terminal(self) -> None:
        for event_type in ("progress", "log"):
            summary = validate_runner_event(event_type, "job-1-plan", {})
            self.assertFalse(summary.terminal)

    def test_rejects_invalid_event_type(self) -> None:
        with self.assertRaisesRegex(RunnerEventError, "event type is invalid"):
            validate_runner_event("unknown", "job-1-plan", {})

    def test_rejects_empty_execution_id(self) -> None:
        with self.assertRaisesRegex(RunnerEventError, "execution ID is required"):
            validate_runner_event("started", "", {})
        with self.assertRaisesRegex(RunnerEventError, "execution ID is required"):
            validate_runner_event("started", "   ", {})

    def test_rejects_non_dict_payload(self) -> None:
        with self.assertRaisesRegex(RunnerEventError, "payload must be an object"):
            validate_runner_event("started", "job-1-plan", [])  # type: ignore[arg-type]

    def test_rejects_oversized_payload(self) -> None:
        payload: dict[str, Any] = {f"key{i}": i for i in range(33)}
        with self.assertRaisesRegex(RunnerEventError, "too many fields"):
            validate_runner_event("started", "job-1-plan", payload)

    def test_rejects_non_integer_exit_code(self) -> None:
        with self.assertRaisesRegex(RunnerEventError, "exitCode must be an integer"):
            validate_runner_event("completed", "job-1-plan", {"exitCode": "zero"})

    def test_rejects_non_string_list_changed_files(self) -> None:
        with self.assertRaisesRegex(RunnerEventError, "changedFiles must be a list"):
            validate_runner_event("completed", "job-1-plan", {"changedFiles": "a.py"})
        with self.assertRaisesRegex(RunnerEventError, "changedFiles must be a list"):
            validate_runner_event("completed", "job-1-plan", {"changedFiles": [1, 2]})

    def test_rejects_non_string_list_commands_run(self) -> None:
        with self.assertRaisesRegex(RunnerEventError, "commandsRun must be a list"):
            validate_runner_event("completed", "job-1-plan", {"commandsRun": 42})

    def test_optional_fields_default_to_empty(self) -> None:
        summary = validate_runner_event("completed", "job-1-plan", {"exitCode": 0})
        self.assertEqual(summary.changed_files, [])
        self.assertEqual(summary.commands_run, [])
        self.assertFalse(summary.blocked)
        self.assertIsNone(summary.summary_text)
        self.assertEqual(summary.remaining_work, [])

    def test_agent_reported_block_is_parsed_from_a_failed_event(self) -> None:
        """The runner reports an agent-reported block as a `failed` event
        refined by a `blocked` marker; its own account of the block has to
        survive validation intact."""
        summary = validate_runner_event(
            "failed",
            "job-1-plan",
            {
                "status": "blocked",
                "blocked": True,
                "exitCode": 0,
                "summary": "the deployment credential is missing",
                "remainingWork": ["obtain DEPLOY_KEY", "re-run the migration"],
                "result": {"status": "blocked", "summary": "the deployment credential is missing"},
            },
        )
        self.assertTrue(summary.terminal)
        self.assertTrue(summary.blocked)
        self.assertEqual(summary.summary_text, "the deployment credential is missing")
        self.assertEqual(summary.remaining_work, ["obtain DEPLOY_KEY", "re-run the migration"])
        assert summary.result is not None
        self.assertEqual(summary.result["status"], "blocked")

    def test_result_document_is_parsed_for_every_terminal_event(self) -> None:
        for event_type in ("failed", "cancelled"):
            summary = validate_runner_event(
                event_type, "job-1-review", {"result": {"verdict": "invalid"}}
            )
            assert summary.result is not None
            self.assertEqual(summary.result["verdict"], "invalid")

    def test_non_terminal_event_carries_no_result_document(self) -> None:
        summary = validate_runner_event("progress", "job-1-plan", {"result": {"status": "blocked"}})
        self.assertIsNone(summary.result)

    def test_rejects_malformed_block_fields(self) -> None:
        with self.assertRaisesRegex(RunnerEventError, "blocked must be a boolean"):
            validate_runner_event("failed", "job-1-plan", {"blocked": "yes"})
        with self.assertRaisesRegex(RunnerEventError, "summary must be a string"):
            validate_runner_event("failed", "job-1-plan", {"summary": ["blocked"]})
        with self.assertRaisesRegex(RunnerEventError, "remainingWork must be a list"):
            validate_runner_event("failed", "job-1-plan", {"remainingWork": "one thing"})
        with self.assertRaisesRegex(RunnerEventError, "remainingWork must be a list"):
            validate_runner_event("failed", "job-1-plan", {"remainingWork": [1]})
        with self.assertRaisesRegex(RunnerEventError, "result must be an object"):
            validate_runner_event("failed", "job-1-plan", {"result": "blocked"})

    def test_block_marker_is_only_honoured_on_a_failed_event(self) -> None:
        """A completed or cancelled execution contradicts a block: the outcome
        the runner reported wins over a marker in the payload."""
        for event_type in ("completed", "cancelled"):
            summary = validate_runner_event(event_type, "job-1-plan", {"blocked": True})
            self.assertFalse(summary.blocked)


class ExecutionRoleFromIdTests(unittest.TestCase):
    def test_recognized_suffixes_return_correct_role(self) -> None:
        self.assertEqual(execution_role_from_id("job-abc-plan"), "planner")
        self.assertEqual(execution_role_from_id("job-abc-implement"), "developer")
        self.assertEqual(execution_role_from_id("job-abc-review"), "reviewer")
        self.assertEqual(execution_role_from_id("job-abc-repair"), "repairer")

    def test_unrecognized_suffix_returns_none(self) -> None:
        self.assertIsNone(execution_role_from_id("job-abc-push"))
        self.assertIsNone(execution_role_from_id("job-abc"))
        self.assertIsNone(execution_role_from_id(""))


class WorkflowTransitionTests(unittest.TestCase):
    def _summary(
        self,
        event_type: str,
        execution_id: str = "job-1-plan",
        exit_code: int | None = 0,
        result: dict[str, Any] | None = None,
        blocked: bool = False,
        summary_text: str | None = None,
        remaining_work: list[str] | None = None,
        changed_files: list[str] | None = None,
    ) -> RunnerEventSummary:
        return RunnerEventSummary(
            event_type=event_type,
            execution_id=execution_id,
            exit_code=exit_code,
            changed_files=list(changed_files or []),
            commands_run=[],
            terminal=event_type in {"completed", "failed", "cancelled"},
            result=result,
            blocked=blocked,
            summary_text=summary_text,
            remaining_work=list(remaining_work or []),
        )

    def test_non_terminal_event_returns_none(self) -> None:
        summary = self._summary("started")
        self.assertIsNone(workflow_transition_for_terminal_event(summary, "preparing"))

    def test_cancelled_event_transitions_to_cancelled(self) -> None:
        summary = self._summary("cancelled")
        transition = workflow_transition_for_terminal_event(summary, "planning")
        assert transition is not None
        self.assertEqual(transition.new_status, "cancelled")

    def test_failed_event_transitions_to_recovering(self) -> None:
        summary = self._summary("failed", exit_code=1)
        transition = workflow_transition_for_terminal_event(summary, "planning")
        assert transition is not None
        self.assertEqual(transition.new_status, "recovering")

    def _blocked(self, execution_id: str = "job-1-plan") -> RunnerEventSummary:
        return self._summary(
            "failed",
            execution_id,
            exit_code=0,
            blocked=True,
            summary_text="the deployment credential is missing",
            remaining_work=["obtain DEPLOY_KEY", "re-run the migration"],
            result={"status": "blocked", "summary": "the deployment credential is missing"},
        )

    def test_agent_reported_block_transitions_to_blocked_with_its_own_reason(self) -> None:
        """An agent-reported block is not an anonymous failure: the agent said
        why it stopped and what remains, and both have to reach the workflow."""
        for execution_id, role in (
            ("job-1-plan", "planner"),
            ("job-1-implement", "developer"),
            ("job-1-review", "reviewer"),
            ("job-1-repair", "repairer"),
        ):
            transition = workflow_transition_for_terminal_event(
                self._blocked(execution_id), "planning", role=role
            )
            assert transition is not None, role
            self.assertEqual(transition.new_status, "blocked", role)
            reason = str(transition.state_updates["blocking_reason"])
            self.assertIn("the deployment credential is missing", reason, role)
            self.assertIn("obtain DEPLOY_KEY", reason, role)
            self.assertIn("re-run the migration", reason, role)
            self.assertIs(transition.state_updates["awaiting_execution"], False, role)

    def test_agent_reported_block_clears_the_role_gate_it_owns(self) -> None:
        planner = workflow_transition_for_terminal_event(
            self._blocked("job-1-plan"), "planning", role="planner"
        )
        assert planner is not None
        self.assertIs(planner.state_updates["plan_valid"], False)
        reviewer = workflow_transition_for_terminal_event(
            self._blocked("job-1-review"), "ai_review", role="reviewer"
        )
        assert reviewer is not None
        self.assertIs(reviewer.state_updates["review_approved"], False)

    def test_agent_reported_block_without_a_reason_still_names_the_role(self) -> None:
        summary = self._summary("failed", "job-1-implement", exit_code=0, blocked=True)
        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertEqual(transition.new_status, "blocked")
        self.assertIn("developer", str(transition.state_updates["blocking_reason"]))

    def test_blocking_reason_is_bounded(self) -> None:
        summary = self._summary(
            "failed",
            "job-1-implement",
            exit_code=0,
            blocked=True,
            summary_text="x" * 4000,
            remaining_work=["y" * 4000],
        )
        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertLessEqual(len(str(transition.state_updates["blocking_reason"])), 1024)

    def test_pipeline_role_keeps_its_gate_even_if_a_block_is_marked(self) -> None:
        """`pipeline_passed` has exactly one producer; a marker in the payload
        must not divert the deterministic gate's own execution."""
        summary = self._summary("failed", "job-1-pipeline", exit_code=1, blocked=True)
        transition = workflow_transition_for_terminal_event(summary, "local_pipeline", role="pipeline")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")
        self.assertIs(transition.state_updates["pipeline_passed"], False)

    def test_unmarked_failure_still_recovers_rather_than_blocking(self) -> None:
        summary = self._summary("failed", "job-1-implement", exit_code=1)
        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertEqual(transition.new_status, "recovering")

    def _planner_result(self, status: str) -> dict[str, Any]:
        return {
            "status": status,
            "summary": "s",
            "assumptions": [],
            "questions": [],
            "risk": "low",
            "acceptanceCriteria": [],
            "steps": [],
        }

    def _review_result(self, verdict: str) -> dict[str, Any]:
        return {"verdict": verdict, "acceptanceCriteria": [], "findings": []}

    def test_terminal_events_clear_the_awaiting_execution_gate(self) -> None:
        """The suspended graph only advances when the gate the dispatching node
        set is cleared; every terminal transition has to carry that clear."""
        for summary, status, role in (
            (self._summary("completed", "job-1-plan", result=self._planner_result("ready")), "planning", "planner"),
            (self._summary("completed", "job-1-implement"), "implementing", "developer"),
            (self._summary("failed", "job-1-pipeline", exit_code=1), "local_pipeline", "pipeline"),
            (self._summary("failed", "job-1-repair", exit_code=1), "repairing", "repairer"),
            (self._summary("cancelled", "job-1-plan"), "planning", "planner"),
        ):
            transition = workflow_transition_for_terminal_event(summary, status, role=role)
            assert transition is not None
            self.assertIs(transition.state_updates["awaiting_execution"], False)

    def test_completed_planner_with_ready_verdict_transitions_to_implementing(self) -> None:
        summary = self._summary("completed", "job-1-plan", result=self._planner_result("ready"))
        transition = workflow_transition_for_terminal_event(summary, "planning", role="planner")
        assert transition is not None
        self.assertEqual(transition.new_status, "implementing")
        self.assertTrue(transition.state_updates.get("plan_valid"))

    def test_completed_planner_without_result_does_not_approve_the_plan(self) -> None:
        summary = self._summary("completed", "job-1-plan")
        transition = workflow_transition_for_terminal_event(summary, "planning", role="planner")
        assert transition is not None
        self.assertEqual(transition.new_status, "planning")
        self.assertFalse(transition.state_updates.get("plan_valid"))

    def test_completed_planner_with_blocked_verdict_transitions_to_blocked(self) -> None:
        summary = self._summary("completed", "job-1-plan", result=self._planner_result("blocked"))
        transition = workflow_transition_for_terminal_event(summary, "planning", role="planner")
        assert transition is not None
        self.assertEqual(transition.new_status, "blocked")
        self.assertFalse(transition.state_updates.get("plan_valid"))

    def _agent_result(self, changed_files: list[str], remaining_work: list[str] | None = None) -> dict[str, Any]:
        return {
            "protocolVersion": "1.0",
            "executionId": "job-1-implement",
            "status": "completed",
            "summary": "implemented the requested change",
            "changedFiles": changed_files,
            "commandsRun": [],
            "remainingWork": remaining_work or [],
            "knownLimitations": [],
        }

    def test_completed_developer_with_delivery_evidence_transitions_to_local_pipeline(self) -> None:
        summary = self._summary(
            "completed", "job-1-implement", result=self._agent_result(["a.py"]), changed_files=["a.py"]
        )
        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")
        self.assertEqual(transition.state_updates["last_delivery_outcome"], "delivered")

    def test_completed_developer_without_delivery_evidence_requests_continuation(self) -> None:
        summary = self._summary("completed", "job-1-implement", result=self._agent_result([]))
        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertEqual(transition.new_status, "implementing")
        self.assertTrue(transition.state_updates["continuation_requested"])
        self.assertEqual(transition.state_updates["last_delivery_outcome"], "returned_without_evidence")
        self.assertIn("no changed files", str(transition.state_updates["last_gate_verdict"]))

    def test_completed_developer_with_remaining_work_requests_continuation(self) -> None:
        summary = self._summary(
            "completed", "job-1-implement", result=self._agent_result(["a.py"], ["finish tests"]),
            changed_files=["a.py"], remaining_work=["finish tests"],
        )
        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertEqual(transition.new_status, "implementing")
        self.assertEqual(transition.state_updates["remaining_work"], ["finish tests"])

    def test_developer_exit_code_never_decides_the_pipeline_gate(self) -> None:
        """A clean developer exit is evidence the agent process ended, not that
        the deterministic checks pass. Either exit code must hand the run to the
        local_pipeline phase with the gate untouched, so the pipeline node
        dispatches a real execution instead of short-circuiting into review."""
        for exit_code in (0, 1):
            summary = self._summary(
                "completed", "job-1-implement", exit_code=exit_code,
                result=self._agent_result(["a.py"]), changed_files=["a.py"],
            )
            transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
            assert transition is not None
            self.assertEqual(transition.new_status, "local_pipeline")
            self.assertNotIn("pipeline_passed", transition.state_updates)

    def test_only_the_pipeline_role_writes_the_pipeline_gate(self) -> None:
        """`pipeline_passed` has exactly one producer. Any other role writing it
        would reintroduce a gate that can be satisfied without the deterministic
        checks ever running."""
        cases = [
            (self._summary("completed", "job-1-plan", result=self._planner_result("ready")), "planning", "planner"),
            (self._summary("completed", "job-1-plan"), "planning", "planner"),
            (self._summary("completed", "job-1-implement"), "implementing", "developer"),
            (self._summary("completed", "job-1-implement"), "pushing", "developer"),
            (self._summary("completed", "job-1-review", result=self._review_result("approved")), "ai_review", "reviewer"),
            (self._summary("completed", "job-1-review"), "ai_review", "reviewer"),
            (self._summary("completed", "job-1-repair"), "repairing", "repairer"),
            (self._summary("failed", "job-1-implement", exit_code=1), "implementing", "developer"),
            (self._summary("cancelled", "job-1-implement"), "implementing", "developer"),
        ]
        for summary, status, role in cases:
            transition = workflow_transition_for_terminal_event(summary, status, role=role)
            assert transition is not None, (role, status)
            self.assertNotIn("pipeline_passed", transition.state_updates, (role, status))
        pipeline = workflow_transition_for_terminal_event(
            self._summary("completed", "job-1-pipeline"), "local_pipeline", role="pipeline"
        )
        assert pipeline is not None
        self.assertIn("pipeline_passed", pipeline.state_updates)

    def test_completed_reviewer_with_approved_verdict_transitions_to_pushing(self) -> None:
        summary = self._summary("completed", "job-1-review", result=self._review_result("approved"))
        transition = workflow_transition_for_terminal_event(summary, "ai_review", role="reviewer")
        assert transition is not None
        self.assertEqual(transition.new_status, "pushing")
        self.assertTrue(transition.state_updates.get("review_approved"))

    def test_completed_reviewer_with_changes_requested_does_not_approve(self) -> None:
        summary = self._summary("completed", "job-1-review", result=self._review_result("changes_requested"))
        transition = workflow_transition_for_terminal_event(summary, "ai_review", role="reviewer")
        assert transition is not None
        self.assertNotEqual(transition.new_status, "pushing")
        self.assertFalse(transition.state_updates.get("review_approved"))

    def test_completed_reviewer_without_result_does_not_approve(self) -> None:
        """A completed process alone must never approve the review."""
        summary = self._summary("completed", "job-1-review")
        transition = workflow_transition_for_terminal_event(summary, "ai_review", role="reviewer")
        assert transition is not None
        self.assertFalse(transition.state_updates.get("review_approved"))

    def test_missing_result_schema_fails_closed_without_rejecting_the_event(self) -> None:
        summary = self._summary("completed", "job-1-review", result=self._review_result("approved"))
        with patch(
            "moirai.workflows.runner_events.load_schema",
            side_effect=SchemaNotFoundError("missing schema"),
        ):
            transition = workflow_transition_for_terminal_event(summary, "ai_review", role="reviewer")
        assert transition is not None
        self.assertEqual(transition.new_status, "ai_review")
        self.assertFalse(transition.state_updates["review_approved"])

    def test_human_required_parks_with_question_and_resume_phase(self) -> None:
        result = self._review_result("human_required")
        result["findings"] = ["Which compatibility target?"]
        transition = workflow_transition_for_terminal_event(
            self._summary("completed", "job-1-review", result=result), "ai_review", role="reviewer"
        )
        assert transition is not None
        self.assertEqual(transition.new_status, "waiting_human")
        self.assertEqual(transition.state_updates["human_question"], "Which compatibility target?")
        self.assertEqual(transition.state_updates["human_resume_phase"], "ai_review")

    def test_pipeline_result_controls_pipeline_gate_independently_of_developer(self) -> None:
        failed = self._summary("failed", "job-1-pipeline", exit_code=1)
        transition = workflow_transition_for_terminal_event(failed, "local_pipeline", role="pipeline")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")
        self.assertFalse(transition.state_updates["pipeline_passed"])
        passed = self._summary("completed", "job-1-pipeline", exit_code=0)
        transition = workflow_transition_for_terminal_event(passed, "local_pipeline", role="pipeline")
        assert transition is not None
        self.assertTrue(transition.state_updates["pipeline_passed"])

    def test_completed_repairer_transitions_to_local_pipeline(self) -> None:
        summary = self._summary(
            "completed", "job-1-repair", result=self._agent_result(["a.py"]), changed_files=["a.py"]
        )
        transition = workflow_transition_for_terminal_event(summary, "repairing", role="repairer")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")
        # The repaired tree is re-validated by a real pipeline execution: the
        # gate must not survive from the run that preceded the repair.
        self.assertNotIn("pipeline_passed", transition.state_updates)

    def test_completed_unknown_role_returns_none(self) -> None:
        summary = self._summary("completed", "job-1-push")
        self.assertIsNone(workflow_transition_for_terminal_event(summary, "pushing", role=None))

    def test_role_falls_back_to_suffix_derivation_when_not_supplied(self) -> None:
        """Callers with no authoritative role (e.g. the in-memory control
        plane used only in tests) fall back to suffix parsing."""
        summary = self._summary(
            "completed", "job-1-repair", result=self._agent_result(["a.py"]), changed_files=["a.py"]
        )
        transition = workflow_transition_for_terminal_event(summary, "repairing")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")


class AgentBlockEndToEndTests(unittest.TestCase):
    """Issue #97 acceptance: an agent-reported block reaches
    `workflow_transition_for_terminal_event` with its summary and
    `remainingWork` intact. The payload below is the wire shape the runner
    builds in `dispatch.terminalPayload` for a blocked outcome."""

    BLOCKED_PAYLOAD: ClassVar[dict[str, Any]] = {
        "status": "blocked",
        "blocked": True,
        "exitCode": 0,
        "changedFiles": ["migrations/003.sql"],
        "commandsRun": [],
        "finalRevision": "cafebabe",
        "committed": True,
        "pushed": False,
        "wipCommit": "cafebabe",
        "wipBranch": "wip/job-1-implement",
        "wipPushed": True,
        "summary": "the deployment credential is missing",
        "remainingWork": ["obtain DEPLOY_KEY", "re-run the migration"],
        "result": {
            "protocolVersion": "1.0",
            "executionId": "job-1-implement",
            "status": "blocked",
            "summary": "the deployment credential is missing",
            "remainingWork": ["obtain DEPLOY_KEY", "re-run the migration"],
        },
        "failureFingerprint": "execution:0123456789abcdef",
        "error": "the deployment credential is missing",
        "durationMs": 4200,
        "changedFileCount": 1,
        "commandCount": 0,
        "pipelineCommandCount": 0,
    }

    def test_blocked_payload_survives_the_whole_path(self) -> None:
        summary = validate_runner_event("failed", "job-1-implement", self.BLOCKED_PAYLOAD)
        self.assertTrue(summary.blocked)
        self.assertEqual(summary.summary_text, "the deployment credential is missing")
        self.assertEqual(summary.remaining_work, ["obtain DEPLOY_KEY", "re-run the migration"])
        assert summary.result is not None
        self.assertEqual(summary.result["status"], "blocked")

        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertEqual(transition.new_status, "blocked")
        reason = str(transition.state_updates["blocking_reason"])
        self.assertIn("developer reported blocked", reason)
        self.assertIn("the deployment credential is missing", reason)
        self.assertIn("obtain DEPLOY_KEY", reason)
        self.assertIn("re-run the migration", reason)

    def test_the_wire_payload_is_accepted_whole(self) -> None:
        """The runner attaches the block fields on top of an already-populated
        failure payload, so the widened payload must still clear the field limit
        that would otherwise reject the event outright."""
        self.assertLessEqual(len(self.BLOCKED_PAYLOAD), MAX_PAYLOAD_FIELDS)
        padded = dict(self.BLOCKED_PAYLOAD)
        padded.update({f"future{index}": index for index in range(MAX_PAYLOAD_FIELDS - len(padded) + 1)})
        with self.assertRaisesRegex(RunnerEventError, "too many fields"):
            validate_runner_event("failed", "job-1-implement", padded)


if __name__ == "__main__":
    unittest.main()
