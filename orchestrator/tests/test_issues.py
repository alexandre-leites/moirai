import unittest
from datetime import UTC, datetime, timedelta

from moirai.domain import ExternalIssue, LabelPolicy, reconcile_labels, synchronize_issue

NOW = datetime(2026, 1, 1, tzinfo=UTC)


class IssueSynchronizationTests(unittest.TestCase):
    def external(self, labels: tuple[str, ...], state: str = "OPEN") -> ExternalIssue:
        return ExternalIssue("42", "Title", "Body", state, labels, NOW, NOW)

    def test_synchronization_derives_priority_eligibility_and_human_gate(self) -> None:
        synchronized = synchronize_issue(
            "issue-id",
            "project-id",
            self.external(("agent:ready", "agent-priority:10", "agent-priority:100", "agent:human-approval")),
            NOW + timedelta(minutes=1),
            LabelPolicy(),
        )
        self.assertEqual(synchronized.issue.priority, 100)
        self.assertTrue(synchronized.issue.eligible)
        self.assertTrue(synchronized.human_approval_required)
        self.assertTrue(synchronized.multiple_priority_labels)

    def test_blocking_state_labels_and_closed_issues_are_ineligible(self) -> None:
        cases = (
            (("agent:ready", "agent:running"), "OPEN"),
            (("agent:ready", "agent:blocked"), "OPEN"),
            (("agent:ready", "agent:delivered"), "OPEN"),
            (("agent:ready",), "CLOSED"),
        )
        for labels, state in cases:
            synchronized = synchronize_issue(
                "issue-id", "project-id", self.external(labels, state), NOW, LabelPolicy()
            )
            self.assertFalse(synchronized.issue.eligible)

    def test_invalid_priority_labels_are_retained_as_warnings(self) -> None:
        synchronized = synchronize_issue(
            "issue-id", "project-id", self.external(("agent:ready", "agent-priority:not-a-number")), NOW, LabelPolicy()
        )
        self.assertEqual(synchronized.issue.priority, 0)
        self.assertEqual(synchronized.invalid_priority_labels, ("agent-priority:not-a-number",))

    def test_label_reconciliation_is_idempotent_and_sorted(self) -> None:
        add, remove = reconcile_labels(("ready", "stale"), ("running", "ready"))
        self.assertEqual(add, ("running",))
        self.assertEqual(remove, ("stale",))


if __name__ == "__main__":
    unittest.main()
