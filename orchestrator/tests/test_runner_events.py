import unittest
from typing import Any

from moirai.workflows.runner_events import (
    RunnerEventError,
    RunnerEventSummary,
    execution_role_from_id,
    validate_runner_event,
    workflow_transition_for_terminal_event,
)


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
    ) -> RunnerEventSummary:
        return RunnerEventSummary(
            event_type=event_type,
            execution_id=execution_id,
            exit_code=exit_code,
            changed_files=[],
            commands_run=[],
            terminal=event_type in {"completed", "failed", "cancelled"},
            result=result,
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

    def test_completed_developer_transitions_to_local_pipeline(self) -> None:
        summary = self._summary("completed", "job-1-implement")
        transition = workflow_transition_for_terminal_event(summary, "implementing", role="developer")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")

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

    def test_completed_reviewer_with_human_required_verdict_transitions_to_blocked(self) -> None:
        summary = self._summary("completed", "job-1-review", result=self._review_result("human_required"))
        transition = workflow_transition_for_terminal_event(summary, "ai_review", role="reviewer")
        assert transition is not None
        self.assertEqual(transition.new_status, "blocked")
        self.assertFalse(transition.state_updates.get("review_approved"))

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
        summary = self._summary("completed", "job-1-repair")
        transition = workflow_transition_for_terminal_event(summary, "repairing", role="repairer")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")

    def test_completed_unknown_role_returns_none(self) -> None:
        summary = self._summary("completed", "job-1-push")
        self.assertIsNone(workflow_transition_for_terminal_event(summary, "pushing", role=None))

    def test_role_falls_back_to_suffix_derivation_when_not_supplied(self) -> None:
        """Callers with no authoritative role (e.g. the in-memory control
        plane used only in tests) fall back to suffix parsing."""
        summary = self._summary("completed", "job-1-repair")
        transition = workflow_transition_for_terminal_event(summary, "repairing")
        assert transition is not None
        self.assertEqual(transition.new_status, "local_pipeline")


if __name__ == "__main__":
    unittest.main()
