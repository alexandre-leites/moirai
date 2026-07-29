import unittest

from moirai.workflows.issue_graph import (
    route_checks,
    route_human,
    route_pipeline,
    route_plan,
    route_review,
    suspend_after_dispatch,
)
from moirai.workflows.policy import RetryBudget


class IssueGraphRouteTests(unittest.TestCase):
    def test_plan_route_uses_shared_budget_policy(self) -> None:
        budget = RetryBudget(planning_attempts=2, total_agent_executions=3)
        self.assertEqual(route_plan({"planning_attempts": 1}, budget), "plan")
        self.assertEqual(route_plan({"plan_valid": True}, budget), "implement")
        self.assertEqual(route_plan({"planning_attempts": 2}, budget), "blocked")
        self.assertEqual(route_plan({"plan_valid": True, "total_agent_executions": 3}, budget), "blocked")

    def test_pipeline_and_review_routes_use_shared_budget_policy(self) -> None:
        budget = RetryBudget(pipeline_repair_attempts=2, review_cycles=2, total_agent_executions=3)
        self.assertEqual(route_pipeline({"pipeline_passed": True}, budget), "review")
        self.assertEqual(route_pipeline({"pipeline_repair_attempts": 1}, budget), "repair")
        self.assertEqual(route_pipeline({"pipeline_repair_attempts": 2}, budget), "blocked")
        self.assertEqual(route_review({"review_approved": True}, budget), "push")
        self.assertEqual(route_review({"review_cycles": 1}, budget), "repair")
        self.assertEqual(route_review({"review_cycles": 2}, budget), "blocked")

    def test_checks_route_requires_prior_gates_then_human_or_merge(self) -> None:
        budget = RetryBudget(ci_repair_attempts=2, total_agent_executions=3)
        self.assertEqual(route_checks({}, budget), "blocked")
        self.assertEqual(route_checks({"pipeline_passed": True, "review_approved": True}, budget), "ci_repair")
        self.assertEqual(
            route_checks({"pipeline_passed": True, "review_approved": True, "checks_passed": True}, budget),
            "merge",
        )
        self.assertEqual(
            route_checks({"pipeline_passed": True, "review_approved": True, "checks_passed": True, "human_approval_required": True}, budget),
            "wait_for_human",
        )

    def test_failing_checks_route_to_ci_repair_on_the_ci_budget_alone(self) -> None:
        """The failing-checks edge is the only entry to the `ci_repair` node,
        and it is bounded by `ci_repair_attempts` -- not by the local pipeline's
        repair budget, which a CI failure must never touch."""
        budget = RetryBudget(pipeline_repair_attempts=3, ci_repair_attempts=2, total_agent_executions=10)
        delivered = {"pipeline_passed": True, "review_approved": True}
        self.assertEqual(route_checks({**delivered, "ci_repair_attempts": 1}, budget), "ci_repair")
        self.assertEqual(route_checks({**delivered, "ci_repair_attempts": 2}, budget), "blocked")
        self.assertEqual(route_checks({**delivered, "pipeline_repair_attempts": 3}, budget), "ci_repair")

    def test_dispatching_edge_ends_the_invocation_while_an_execution_is_pending(self) -> None:
        route = suspend_after_dispatch(lambda state: "pipeline")
        self.assertEqual(route({"status": "implementing"}), "pipeline")
        self.assertNotEqual(route({"status": "implementing", "awaiting_execution": True}), "pipeline")

    def test_dispatching_edge_sends_an_exhausted_budget_straight_to_blocked(self) -> None:
        """A node that reported blocked instead of dispatching must not fall
        through an unconditional edge into another dispatching node."""
        route = suspend_after_dispatch(lambda state: "pipeline")
        self.assertEqual(route({"status": "blocked", "blocking_reason": "budget"}), "blocked")

    def test_human_route_routes_based_on_human_response(self) -> None:
        self.assertEqual(route_human({"human_approved": True}), "merge")
        self.assertEqual(route_human({"human_changes_requested": True}), "repair")
        self.assertEqual(route_human({}), "blocked")
        self.assertEqual(
            route_human({"human_approved": False, "human_changes_requested": False}),
            "blocked",
        )


if __name__ == "__main__":
    unittest.main()
