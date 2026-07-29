import unittest
from dataclasses import replace

from moirai.workflows.policy import (
    GateState,
    RetryBudget,
    WorkflowRoute,
    route_after_checks,
    route_after_human_response,
    route_after_merge,
    route_after_pipeline,
    route_after_plan,
    route_after_review,
)


class WorkflowPolicyTests(unittest.TestCase):
    def test_plan_retries_then_blocks_and_respects_total_agent_budget(self) -> None:
        budget = RetryBudget(planning_attempts=2, total_agent_executions=3)
        self.assertEqual(route_after_plan(GateState(planning_attempts=1), budget), WorkflowRoute.PLAN)
        self.assertEqual(route_after_plan(GateState(planning_attempts=2), budget), WorkflowRoute.BLOCKED)
        self.assertEqual(
            route_after_plan(GateState(plan_valid=True, total_agent_executions=3), budget), WorkflowRoute.BLOCKED
        )

    def test_pipeline_and_review_failures_route_to_bounded_repairs(self) -> None:
        budget = RetryBudget(pipeline_repair_attempts=2, review_cycles=2)
        self.assertEqual(route_after_pipeline(GateState(pipeline_repair_attempts=1), budget), WorkflowRoute.REPAIR)
        self.assertEqual(route_after_pipeline(GateState(pipeline_repair_attempts=2), budget), WorkflowRoute.BLOCKED)
        self.assertEqual(route_after_review(GateState(review_cycles=1), budget), WorkflowRoute.REPAIR)
        self.assertEqual(route_after_review(GateState(review_cycles=2), budget), WorkflowRoute.BLOCKED)

    def test_pipeline_review_respects_total_agent_budget(self) -> None:
        budget = RetryBudget(total_agent_executions=1)
        state = GateState(pipeline_passed=True, total_agent_executions=1)
        self.assertEqual(route_after_pipeline(state, budget), WorkflowRoute.BLOCKED)

    def test_checks_require_pipeline_and_review_before_human_or_merge(self) -> None:
        budget = RetryBudget()
        incomplete = GateState(checks_passed=True, pipeline_passed=True)
        self.assertEqual(route_after_checks(incomplete, budget), WorkflowRoute.BLOCKED)
        pending = GateState(pipeline_passed=True, review_approved=True, checks_pending=True)
        self.assertEqual(route_after_checks(pending, budget), WorkflowRoute.WAIT_FOR_CHECKS)
        waiting = GateState(
            checks_passed=True,
            pipeline_passed=True,
            review_approved=True,
            human_approval_required=True,
        )
        self.assertEqual(route_after_checks(waiting, budget), WorkflowRoute.WAIT_FOR_HUMAN)
        approved = GateState(
            checks_passed=True,
            pipeline_passed=True,
            review_approved=True,
            human_approval_required=True,
            human_approved=True,
        )
        self.assertEqual(route_after_checks(approved, budget), WorkflowRoute.MERGE)
        self.assertEqual(route_after_human_response(False, True), WorkflowRoute.REPAIR)
        self.assertEqual(route_after_human_response(False, False), WorkflowRoute.BLOCKED)

    def test_failing_checks_route_to_the_ci_repair_node_and_stop_at_its_own_budget(self) -> None:
        """Failing checks must reach a route that spends `ci_repair_attempts`,
        and that counter alone must decide when the CI loop stops."""
        budget = RetryBudget(ci_repair_attempts=2)
        delivered = GateState(pipeline_passed=True, review_approved=True)
        self.assertEqual(route_after_checks(delivered, budget), WorkflowRoute.CI_REPAIR)
        self.assertEqual(
            route_after_checks(replace(delivered, ci_repair_attempts=1), budget),
            WorkflowRoute.CI_REPAIR,
        )
        self.assertEqual(
            route_after_checks(replace(delivered, ci_repair_attempts=2), budget),
            WorkflowRoute.BLOCKED,
        )

    def test_ci_and_pipeline_repair_budgets_are_independent(self) -> None:
        """Neither failure source may block on -- or be blocked by -- the
        other's counter."""
        budget = RetryBudget(pipeline_repair_attempts=2, ci_repair_attempts=2)
        drained_pipeline = GateState(
            pipeline_passed=True, review_approved=True, pipeline_repair_attempts=2
        )
        self.assertEqual(route_after_checks(drained_pipeline, budget), WorkflowRoute.CI_REPAIR)
        self.assertEqual(
            route_after_pipeline(GateState(ci_repair_attempts=2, pipeline_repair_attempts=1), budget),
            WorkflowRoute.REPAIR,
        )


    def test_completion_is_reachable_only_from_a_verified_merge(self) -> None:
        """Completion closes the issue and labels it delivered, so the one gate
        that opens it is a merge the code host confirmed. Every other shape of
        gate state -- including the passing checks and human approval that got
        the run to the merge node -- keeps waiting."""
        self.assertEqual(route_after_merge(GateState(pull_request_merged=True)), WorkflowRoute.COMPLETE)
        self.assertEqual(route_after_merge(GateState()), WorkflowRoute.BLOCKED)
        approved = GateState(
            pipeline_passed=True, review_approved=True, checks_passed=True, human_approved=True
        )
        self.assertEqual(route_after_merge(approved), WorkflowRoute.BLOCKED)


if __name__ == "__main__":
    unittest.main()
