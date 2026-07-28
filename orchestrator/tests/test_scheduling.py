import unittest
from datetime import UTC, datetime, timedelta

from moirai.domain import (
    Issue,
    Project,
    Runner,
    Workflow,
    WorkflowStatus,
    parse_priority,
    select_assignment,
)

NOW = datetime(2026, 1, 1, tzinfo=UTC)


def issue(identifier: str, project_id: str, priority: int, minutes: int) -> Issue:
    timestamp = NOW + timedelta(minutes=minutes)
    return Issue(identifier, project_id, identifier, priority, timestamp, timestamp, True)


class SchedulingTests(unittest.TestCase):
    def test_priority_uses_highest_valid_value_and_tracks_invalid_labels(self) -> None:
        priority, invalid = parse_priority(["agent-priority:10", "agent-priority:100", "agent-priority:x"], "agent-priority:")
        self.assertEqual(priority, 100)
        self.assertEqual(invalid, ("agent-priority:x",))

    def test_global_selection_prefers_priority_then_oldest_issue(self) -> None:
        projects = [Project("a", True), Project("b", True)]
        runner = Runner("runner", frozenset(), True, True, False, True)
        result = select_assignment(projects, [issue("low", "a", 1, 0), issue("high", "b", 2, 10)], [], [runner])
        self.assertIsNotNone(result)
        self.assertEqual(result.issue.id, "high")

    def test_active_project_lock_skips_only_that_project(self) -> None:
        projects = [Project("a", True), Project("b", True)]
        runner = Runner("runner", frozenset(), True, True, False, True)
        workflows = [Workflow("w", "a", "high", WorkflowStatus.IMPLEMENTING)]
        result = select_assignment(projects, [issue("high", "a", 100, 0), issue("next", "b", 1, 0)], workflows, [runner])
        self.assertIsNotNone(result)
        self.assertEqual(result.issue.id, "next")

    def test_runner_must_have_every_required_label(self) -> None:
        projects = [Project("a", True, frozenset({"docker", "opencode"}))]
        incompatible = Runner("a", frozenset({"docker"}), True, True, False, True)
        compatible = Runner("b", frozenset({"docker", "opencode"}), True, True, False, True)
        result = select_assignment(projects, [issue("i", "a", 0, 0)], [], [incompatible, compatible])
        self.assertIsNotNone(result)
        self.assertEqual(result.runner.id, "b")
