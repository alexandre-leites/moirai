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
        add, remove = reconcile_labels(
            ("agent:ready", "agent:stale"),
            ("agent:running", "agent:ready"),
            managed_prefix="agent:",
        )
        self.assertEqual(add, ("agent:running",))
        self.assertEqual(remove, ("agent:stale",))

    def test_label_reconciliation_rejects_an_empty_managed_prefix(self) -> None:
        with self.assertRaises(ValueError):
            reconcile_labels(("bug",), ("agent:running",), managed_prefix="")

    def test_label_reconciliation_only_removes_labels_inside_the_managed_namespace(self) -> None:
        policy = LabelPolicy()
        add, remove = reconcile_labels(
            ("agent:ready", "agent-priority:5", "bug", "enhancement", "needs-design"),
            (policy.running,),
            managed_prefix=policy.managed_prefix,
        )
        self.assertEqual(add, ("agent:running",))
        self.assertEqual(remove, ("agent:ready",))

    def test_label_reconciliation_never_removes_the_user_priority_label(self) -> None:
        policy = LabelPolicy()
        _, remove = reconcile_labels(
            (f"{policy.priority_prefix}100",),
            (policy.delivered,),
            managed_prefix=policy.managed_prefix,
        )
        self.assertEqual(remove, ())

    def test_label_policy_rejects_a_priority_prefix_inside_the_managed_namespace(self) -> None:
        with self.assertRaises(ValueError):
            LabelPolicy(priority_prefix="agent:priority:")

    def test_label_policy_rejects_state_labels_outside_the_managed_namespace(self) -> None:
        with self.assertRaises(ValueError):
            LabelPolicy(blocked="blocked")

    def test_label_policy_rejects_an_empty_managed_prefix(self) -> None:
        with self.assertRaises(ValueError):
            LabelPolicy(managed_prefix="")


if __name__ == "__main__":
    unittest.main()
