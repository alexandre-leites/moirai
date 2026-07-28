import asyncio
import json
import unittest
from collections.abc import Sequence

from moirai.issue_trackers.github_cli import GitHubCliError, GitHubCliIssueTracker, GitHubRepository


class FakeRunner:
    def __init__(self, code: int, stdout: str, stderr: str = "") -> None:
        self.code = code
        self.stdout = stdout
        self.stderr = stderr
        self.commands: list[tuple[str, ...]] = []

    async def run(self, command: Sequence[str], timeout_seconds: float) -> tuple[int, str, str]:
        self.commands.append(tuple(command))
        return self.code, self.stdout, self.stderr


class GitHubCliIssueTrackerTests(unittest.TestCase):
    def test_repository_parses_supported_github_remote_urls(self) -> None:
        for value in ("https://github.com/owner/repo.git", "git@github.com:owner/repo.git"):
            self.assertEqual(GitHubRepository.from_remote_url(value).slug, "owner/repo")
        with self.assertRaisesRegex(ValueError, "GitHub remote URL"):
            GitHubRepository.from_remote_url("https://example.test/owner/repo.git")

    def test_lists_json_issues_without_parsing_tables(self) -> None:
        runner = FakeRunner(
            0,
            json.dumps(
                [{
                    "number": 7,
                    "title": "Fix scheduling",
                    "body": None,
                    "state": "OPEN",
                    "labels": [{"name": "agent:ready"}],
                    "createdAt": "2026-01-01T00:00:00Z",
                    "updatedAt": "2026-01-02T00:00:00Z",
                }]
            ),
        )
        tracker = GitHubCliIssueTracker(GitHubRepository("owner", "repo"), runner)
        issues = asyncio.run(tracker.list_open_issues())
        self.assertEqual(issues[0].external_id, "7")
        self.assertEqual(issues[0].labels, ("agent:ready",))
        self.assertIn("--json", runner.commands[0])

    def test_label_updates_are_argument_safe_and_noop_for_empty_input(self) -> None:
        runner = FakeRunner(0, "")
        tracker = GitHubCliIssueTracker(GitHubRepository("owner", "repo"), runner)
        asyncio.run(tracker.add_labels("7", ["agent:running", "agent:review"]))
        asyncio.run(tracker.remove_labels("7", []))
        self.assertEqual(len(runner.commands), 1)
        self.assertEqual(runner.commands[0][-4:], ("--add-label", "agent:running", "--add-label", "agent:review"))

    def test_get_issue_and_idempotent_comment_use_structured_cli_calls(self) -> None:
        issue = {
            "number": 7, "title": "Fix scheduling", "body": "body", "state": "OPEN",
            "labels": [], "createdAt": "2026-01-01T00:00:00Z", "updatedAt": "2026-01-02T00:00:00Z",
        }
        runner = FakeRunner(0, json.dumps(issue))
        tracker = GitHubCliIssueTracker(GitHubRepository("owner", "repo"), runner)
        self.assertEqual(asyncio.run(tracker.get_issue("7")).external_id, "7")
        runner.stdout = json.dumps({"comments": []})
        asyncio.run(tracker.add_comment("7", "Investigating", "workflow-1"))
        self.assertIn("comment", runner.commands[-1])
        self.assertIn("moirai-comment:workflow-1", runner.commands[-1][-1])

    def test_nonzero_status_redacts_github_tokens(self) -> None:
        runner = FakeRunner(1, "", "authentication failed ghp_abcdefghijklmnopqrstuvwxyz123456")
        tracker = GitHubCliIssueTracker(GitHubRepository("owner", "repo"), runner)
        with self.assertRaisesRegex(GitHubCliError, "REDACTED") as raised:
            asyncio.run(tracker.close_issue("7"))
        self.assertNotIn("ghp_", str(raised.exception))


if __name__ == "__main__":
    unittest.main()
