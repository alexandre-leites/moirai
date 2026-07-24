import unittest

from moirai.workflows.policy import (
    GateState,
    RetryBudget,
    WorkflowRoute,
    route_after_checks,
    route_after_human_response,
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


if __name__ == "__main__":
    unittest.main()
