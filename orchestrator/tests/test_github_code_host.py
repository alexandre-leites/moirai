import asyncio
import json
import unittest
from collections.abc import Sequence

from moirai.code_hosts.github_cli import (
    ChecksResult,
    CheckStatus,
    GitHubCliCodeHost,
    PullRequestCheck,
    checks_pass,
    checks_result,
)
from moirai.issue_trackers.github_cli import GitHubCliError, GitHubRepository


class FakeRunner:
    def __init__(self, responses: list[tuple[int, str, str]]) -> None:
        self.responses = responses
        self.commands: list[tuple[str, ...]] = []

    async def run(self, command: Sequence[str], timeout_seconds: float) -> tuple[int, str, str]:
        self.commands.append(tuple(command))
        if not self.responses:
            raise AssertionError("unexpected command")
        return self.responses.pop(0)


class GitHubCliCodeHostTests(unittest.TestCase):
    def test_existing_pull_request_is_reused_without_create(self) -> None:
        runner = FakeRunner([(0, json.dumps([_pull_request()]), "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        pull_request = asyncio.run(
            host.create_or_find_pull_request("workflow-1", "agent/workflow-1", "main", "Fix scheduling", "7")
        )

        self.assertEqual(pull_request.external_id, "42")
        self.assertEqual(len(runner.commands), 1)
        self.assertIn("--head", runner.commands[0])

    def test_new_pull_request_has_marker_and_issue_closing_syntax(self) -> None:
        runner = FakeRunner([
            (0, "[]", ""),
            (0, "https://github.example/owner/repo/pull/42\n", ""),
            (0, json.dumps([_pull_request()]), ""),
        ])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        pull_request = asyncio.run(
            host.create_or_find_pull_request("workflow-1", "agent/workflow-1", "main", "Fix scheduling", "7")
        )

        self.assertEqual(pull_request.head_branch, "agent/workflow-1")
        create = runner.commands[1]
        self.assertEqual(create[:3], ("gh", "pr", "create"))
        body = create[create.index("--body") + 1]
        self.assertIn("<!-- loop-engineering-workflow:workflow-1 -->", body)
        self.assertIn("Closes #7", body)

    def test_portable_operations_update_pull_request_and_read_default_branch_head(self) -> None:
        runner = FakeRunner([
            (0, "", ""),
            (0, json.dumps({"reviews": [{"author": {"login": "reviewer"}, "state": "APPROVED", "body": "looks good"}]}), ""),
            (0, "", ""),
            (0, json.dumps({"default_branch": "main"}), ""),
            (0, json.dumps({"object": {"sha": "abc123"}}), ""),
        ])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)
        asyncio.run(host.update_pull_request("42", "Updated", "body"))
        self.assertEqual(asyncio.run(host.get_pull_request_reviews("42"))[0].author, "reviewer")
        asyncio.run(host.close_pull_request("42"))
        self.assertEqual(asyncio.run(host.get_default_branch_head()), "abc123")
        self.assertIn("edit", runner.commands[0])
        self.assertIn("close", runner.commands[2])

    def test_get_pull_request_reports_a_merge_with_its_commit_and_timestamp(self) -> None:
        """The workflow completes on this and nothing else, so the adapter has
        to carry the evidence and not just the word MERGED."""
        runner = FakeRunner([(0, json.dumps(_pull_request(
            state="MERGED",
            mergedAt="2026-01-02T03:04:05Z",
            mergeCommit={"oid": "def456"},
        )), "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        pull_request = asyncio.run(host.get_pull_request("42"))

        self.assertTrue(pull_request.merged)
        self.assertFalse(pull_request.closed)
        self.assertEqual(pull_request.state, "merged")
        self.assertEqual(pull_request.merged_at, "2026-01-02T03:04:05Z")
        self.assertEqual(pull_request.merge_commit, "def456")

    def test_get_pull_request_reports_closed_distinctly_from_merged(self) -> None:
        """A pull request someone closed is not a delivery, and the two must
        never collapse into one outcome."""
        runner = FakeRunner([(0, json.dumps(_pull_request(state="CLOSED")), "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        pull_request = asyncio.run(host.get_pull_request("42"))

        self.assertTrue(pull_request.closed)
        self.assertFalse(pull_request.merged)
        self.assertIsNone(pull_request.merged_at)
        self.assertIsNone(pull_request.merge_commit)

    def test_an_open_pull_request_carries_no_merge_evidence(self) -> None:
        """`gh` answers an unmerged pull request with nulls on some subcommands
        and Go's zero time on others; neither is a merge."""
        runner = FakeRunner([
            (0, json.dumps(_pull_request(mergedAt=None, mergeCommit=None)), ""),
            (0, json.dumps(_pull_request(mergedAt="0001-01-01T00:00:00Z", mergeCommit={"oid": ""})), ""),
        ])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        for _ in range(2):
            pull_request = asyncio.run(host.get_pull_request("42"))
            self.assertFalse(pull_request.merged)
            self.assertIsNone(pull_request.merged_at)
            self.assertIsNone(pull_request.merge_commit)

    def test_every_pull_request_read_asks_for_the_merge_fields(self) -> None:
        runner = FakeRunner([
            (0, json.dumps(_pull_request()), ""),
            (0, json.dumps([_pull_request()]), ""),
        ])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        asyncio.run(host.get_pull_request("42"))
        asyncio.run(host.find_pull_request("agent/workflow-1"))

        for command in runner.commands:
            fields = command[command.index("--json") + 1]
            self.assertIn("mergedAt", fields)
            self.assertIn("mergeCommit", fields)

    def test_required_checks_keep_distinct_statuses(self) -> None:
        runner = FakeRunner([(0, json.dumps([
            {"name": "unit", "bucket": "pass", "link": "https://checks/1", "state": "SUCCESS"},
            {"name": "lint", "bucket": "pending", "link": None, "state": "QUEUED"},
            {"name": "build", "bucket": "fail", "link": None, "state": "FAILURE"},
            {"name": "docs", "bucket": "skipping", "link": None, "state": "SKIPPED"},
            {"name": "deploy", "bucket": "canceling", "link": None, "state": "CANCELLED"},
        ]), "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        checks = asyncio.run(host.required_checks("42"))

        self.assertEqual(
            [check.status for check in checks],
            [
                CheckStatus.PASSING,
                CheckStatus.PENDING,
                CheckStatus.FAILING,
                CheckStatus.SKIPPED,
                CheckStatus.CANCELLED,
            ],
        )

    def test_merge_refuses_non_passing_checks_without_invoking_merge(self) -> None:
        runner = FakeRunner([(0, json.dumps([
            {"name": "unit", "bucket": "pass", "link": None, "state": "SUCCESS"},
            {"name": "lint", "bucket": "pending", "link": None, "state": "QUEUED"},
        ]), "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        with self.assertRaisesRegex(GitHubCliError, "refusing to merge"):
            asyncio.run(host.merge_pull_request("42", "squash"))

        self.assertEqual(len(runner.commands), 1)

    def test_merge_uses_requested_method_only_after_passing_checks(self) -> None:
        runner = FakeRunner([
            (0, json.dumps([{"name": "unit", "bucket": "pass", "link": None, "state": "SUCCESS"}]), ""),
            (0, "", ""),
        ])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        asyncio.run(host.merge_pull_request("42", "squash"))

        self.assertEqual(runner.commands[1][-1], "--squash")
        self.assertNotIn("--admin", runner.commands[1])

    def test_merge_refuses_a_pull_request_with_no_reported_checks(self) -> None:
        runner = FakeRunner([(0, "[]", "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        with self.assertRaisesRegex(GitHubCliError, "refusing to merge"):
            asyncio.run(host.merge_pull_request("42", "squash"))

    def test_unknown_check_state_is_treated_as_pending_not_fatal(self) -> None:
        runner = FakeRunner([(0, json.dumps([
            {"name": "mystery", "bucket": "some_new_bucket", "link": None, "state": "SOME_NEW_BUCKET"},
        ]), "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        checks = asyncio.run(host.required_checks("42"))

        self.assertEqual(checks[0].status, CheckStatus.PENDING)

    def test_required_checks_parses_is_required_field(self) -> None:
        runner = FakeRunner([(0, json.dumps([
            {"name": "unit", "bucket": "skipping", "state": "SKIPPED", "isRequired": True},
            {"name": "lint", "bucket": "skipping", "state": "SKIPPED", "isRequired": False},
        ]), "")])
        host = GitHubCliCodeHost(GitHubRepository("owner", "repo"), runner)

        checks = asyncio.run(host.required_checks("42"))

        self.assertTrue(checks[0].required)
        self.assertFalse(checks[1].required)


class ChecksPassTests(unittest.TestCase):
    def test_empty_check_list_does_not_pass(self) -> None:
        self.assertFalse(checks_pass([]))

    def test_pending_checks_have_a_distinct_result(self) -> None:
        checks = [PullRequestCheck("a", CheckStatus.PENDING)]
        self.assertEqual(checks_result(checks), ChecksResult.PENDING)

    def test_all_passing_checks_pass(self) -> None:
        checks = [PullRequestCheck("a", CheckStatus.PASSING), PullRequestCheck("b", CheckStatus.PASSING)]
        self.assertTrue(checks_pass(checks))

    def test_skipped_non_required_check_passes(self) -> None:
        checks = [
            PullRequestCheck("a", CheckStatus.PASSING),
            PullRequestCheck("b", CheckStatus.SKIPPED, required=False),
        ]
        self.assertTrue(checks_pass(checks))

    def test_skipped_required_check_does_not_pass(self) -> None:
        checks = [
            PullRequestCheck("a", CheckStatus.PASSING),
            PullRequestCheck("b", CheckStatus.SKIPPED, required=True),
        ]
        self.assertFalse(checks_pass(checks))

    def test_any_failing_check_does_not_pass(self) -> None:
        checks = [PullRequestCheck("a", CheckStatus.PASSING), PullRequestCheck("b", CheckStatus.FAILING)]
        self.assertFalse(checks_pass(checks))

    def test_any_pending_check_does_not_pass(self) -> None:
        checks = [PullRequestCheck("a", CheckStatus.PASSING), PullRequestCheck("b", CheckStatus.PENDING)]
        self.assertFalse(checks_pass(checks))


def _pull_request(**overrides: object) -> dict[str, object]:
    return {
        "number": 42,
        "url": "https://github.example/owner/repo/pull/42",
        "state": "OPEN",
        "headRefName": "agent/workflow-1",
        "headRefOid": "abc123",
        "mergedAt": None,
        "mergeCommit": None,
        **overrides,
    }


if __name__ == "__main__":
    unittest.main()
