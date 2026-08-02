"""Provider credentials: the path from a stored key to the agent's environment.

Issue #230 -- there was no way to run against a paid or subscription model
provider, because a credential could not be declared, stored, requested, or
delivered. These are the orchestrator-side halves of that path.
"""

from __future__ import annotations

import unittest
from datetime import UTC, datetime, timedelta

from moirai.config import ConfigurationError, OrchestratorConfig
from moirai.domain.control_plane import InMemoryControlPlane
from moirai.domain.credentials import (
    agent_credential_kind,
    agent_environment_name,
    credential_kind_for_environment,
    normalize_credential_file_path,
    parse_agent_credential_refs,
    validate_credential_kind,
)
from moirai.domain.leases import StaleLeaseError
from moirai.domain.models import Issue, Project
from moirai.workflows.task_packets import (
    EnvironmentRef,
    agent_environment_refs,
    build_task_packet,
    planner_task_execution,
    task_execution,
)

NOW = datetime(2026, 1, 1, tzinfo=UTC)


class CredentialKindTests(unittest.TestCase):
    def test_a_provider_name_becomes_a_storable_kind(self) -> None:
        self.assertEqual(agent_credential_kind("OPENROUTER_API_KEY"), "agent:OPENROUTER_API_KEY")
        self.assertEqual(agent_environment_name("agent:OPENROUTER_API_KEY"), "OPENROUTER_API_KEY")
        validate_credential_kind("agent:OPENROUTER_API_KEY")

    def test_the_two_git_kinds_are_unchanged(self) -> None:
        self.assertEqual(credential_kind_for_environment("GITHUB_TOKEN"), "github_token")
        self.assertEqual(credential_kind_for_environment("GIT_SSH_KEY"), "ssh_private_key")

    def test_any_environment_name_can_back_a_reference(self) -> None:
        """The gate that made a deployment-wide provider key impossible.

        Before this, a name outside the two git kinds was rejected outright, so
        the control plane could never answer "I have nothing under that name,
        use your own environment" -- and the runner never got to fall back.
        """
        self.assertEqual(
            credential_kind_for_environment("ANTHROPIC_API_KEY"), "agent:ANTHROPIC_API_KEY"
        )

    def test_a_name_that_would_repoint_the_execution_is_refused(self) -> None:
        for name in ("HOME", "PATH", "TMPDIR", "LD_PRELOAD", "lowercase", "1BAD"):
            with self.subTest(name=name):
                self.assertIsNone(credential_kind_for_environment(name))
                with self.assertRaises(ValueError):
                    agent_credential_kind(name)

    def test_a_git_name_cannot_be_claimed_as_an_agent_credential(self) -> None:
        """One environment name, one row: two would make precedence unanswerable."""
        for name in ("GITHUB_TOKEN", "GIT_SSH_KEY"):
            with self.subTest(name=name):
                with self.assertRaises(ValueError):
                    agent_credential_kind(name)

    def test_a_file_destination_must_stay_inside_the_agent_home(self) -> None:
        kind = "agent:OPENCODE_AUTH"
        self.assertEqual(
            normalize_credential_file_path(kind, ".local/share/opencode/auth.json"),
            ".local/share/opencode/auth.json",
        )
        for path in ("/etc/passwd", "../escape", "a/../../b", "~/x", "a//b", "with space", "a\\b"):
            with self.subTest(path=path):
                with self.assertRaises(ValueError):
                    normalize_credential_file_path(kind, path)

    def test_a_git_credential_cannot_claim_a_file_destination(self) -> None:
        """ssh_private_key is already a file, and its location is the runner's."""
        with self.assertRaises(ValueError):
            normalize_credential_file_path("ssh_private_key", "keys/id_ed25519")


class DeploymentDeclarationTests(unittest.TestCase):
    def test_a_declaration_list_is_parsed_into_names_and_destinations(self) -> None:
        self.assertEqual(
            parse_agent_credential_refs(
                "OPENROUTER_API_KEY, OPENCODE_AUTH=.local/share/opencode/auth.json"
            ),
            (("OPENROUTER_API_KEY", ""), ("OPENCODE_AUTH", ".local/share/opencode/auth.json")),
        )

    def test_nothing_declared_requests_nothing(self) -> None:
        self.assertEqual(parse_agent_credential_refs(""), ())

    def test_a_typo_stops_the_orchestrator_rather_than_producing_empty_packets(self) -> None:
        environment = {
            "LOOP_DATABASE_URL": "postgresql://localhost/moirai",
            "LOOP_AGENT_CREDENTIAL_REFS": "not-an-env-name",
        }
        with self.assertRaises(ConfigurationError) as raised:
            OrchestratorConfig.from_environment(environment)
        self.assertIn("LOOP_AGENT_CREDENTIAL_REFS", str(raised.exception))

    def test_a_valid_declaration_reaches_the_configuration(self) -> None:
        config = OrchestratorConfig.from_environment(
            {
                "LOOP_DATABASE_URL": "postgresql://localhost/moirai",
                "LOOP_AGENT_CREDENTIAL_REFS": "OPENROUTER_API_KEY",
            }
        )
        self.assertEqual(config.agent_credential_refs, (("OPENROUTER_API_KEY", ""),))


class AgentEnvironmentRefTests(unittest.TestCase):
    def test_a_project_declaration_overrides_the_deployment_one(self) -> None:
        refs = agent_environment_refs(
            [
                ("OPENROUTER_API_KEY", ""),
                ("OPENCODE_AUTH", ""),
                ("OPENCODE_AUTH", ".local/share/opencode/auth.json"),
            ]
        )
        self.assertEqual(
            refs,
            (
                EnvironmentRef("OPENROUTER_API_KEY", "agent:OPENROUTER_API_KEY", ""),
                EnvironmentRef(
                    "OPENCODE_AUTH", "agent:OPENCODE_AUTH", ".local/share/opencode/auth.json"
                ),
            ),
        )

    def test_a_provider_key_is_requested_by_every_role_including_read_only_ones(self) -> None:
        """A planner without its provider key is as stuck as a developer.

        Repository credentials follow what the role may do; a model credential
        does not -- every role runs an agent.
        """
        packet = build_task_packet(
            planner_task_execution(
                job_id="job-123456789",
                project_id="project-1",
                issue_external_id="42",
                issue_title="Title",
                issue_body="Body",
                repository_mode="existing_path",
                repository_url=None,
                local_repository_path="/repositories/project-1",
                default_branch="main",
                agent_credential_refs=agent_environment_refs([("OPENROUTER_API_KEY", "")]),
            )
        )
        self.assertEqual(
            packet["environmentRefs"],
            [{"name": "OPENROUTER_API_KEY", "secretRef": "agent:OPENROUTER_API_KEY", "path": ""}],
        )

    def test_a_provider_key_joins_the_repository_credential_rather_than_replacing_it(self) -> None:
        packet = build_task_packet(
            task_execution(
                job_id="job-123456789",
                execution_id="job-123456789-dev",
                role="developer",
                project_id="project-1",
                issue_external_id="42",
                issue_title="Title",
                issue_body="Body",
                repository_mode="managed_clone",
                repository_url="https://example.test/repository.git",
                local_repository_path=None,
                default_branch="main",
                agent_credential_refs=agent_environment_refs(
                    [("OPENCODE_AUTH", ".local/share/opencode/auth.json")]
                ),
            )
        )
        self.assertEqual(
            packet["environmentRefs"],
            [
                {"name": "GITHUB_TOKEN", "secretRef": "github_token", "path": ""},
                {
                    "name": "OPENCODE_AUTH",
                    "secretRef": "agent:OPENCODE_AUTH",
                    "path": ".local/share/opencode/auth.json",
                },
            ],
        )

    def test_no_value_ever_travels_in_the_packet(self) -> None:
        packet = build_task_packet(
            planner_task_execution(
                job_id="job-123456789",
                project_id="project-1",
                issue_external_id="42",
                issue_title="Title",
                issue_body="Body",
                repository_mode="existing_path",
                repository_url=None,
                local_repository_path="/repositories/project-1",
                default_branch="main",
                agent_credential_refs=agent_environment_refs([("OPENROUTER_API_KEY", "")]),
            )
        )
        self.assertNotIn("sk-or-", repr(packet))
        for reference in packet["environmentRefs"]:  # type: ignore[union-attr]
            self.assertEqual(set(reference), {"name", "secretRef", "path"})

    def test_a_destination_outside_the_agent_home_fails_the_packet(self) -> None:
        with self.assertRaises(ValueError):
            build_task_packet(
                planner_task_execution(
                    job_id="job-123456789",
                    project_id="project-1",
                    issue_external_id="42",
                    issue_title="Title",
                    issue_body="Body",
                    repository_mode="existing_path",
                    repository_url=None,
                    local_repository_path="/repositories/project-1",
                    default_branch="main",
                    agent_credential_refs=(
                        EnvironmentRef("OPENCODE_AUTH", "agent:OPENCODE_AUTH", "../../etc/passwd"),
                    ),
                )
            )


class RotationTests(unittest.TestCase):
    """The durable half of a subscription credential.

    The harness refreshes its own token inside the execution; what decides
    whether the *next* execution starts from a live one is whether the runner
    could put it somewhere shared.
    """

    def setUp(self) -> None:
        self.control_plane = InMemoryControlPlane()
        self.control_plane.add_project(Project("project-a", True, frozenset({"linux"})))
        self.control_plane.add_issue(Issue("issue-a", "project-a", "1", 1, NOW, NOW, True))
        token = self.control_plane.create_registration_token({"linux"}, NOW + timedelta(hours=1))
        runner, self.credential = self.control_plane.register_runner(token, "runner-1", ["linux"], NOW)
        self.runner_id = runner.id
        self.control_plane.heartbeat(self.runner_id, self.credential, NOW)
        scheduled = self.control_plane.schedule(NOW, timedelta(minutes=5))
        assert scheduled is not None
        self.job_id = scheduled.offer.job_id
        lease = self.control_plane.accept_offer(self.job_id, self.runner_id, NOW)
        self.generation = lease.generation

    def test_a_rotated_token_replaces_the_stored_one(self) -> None:
        self.control_plane.set_project_credential(
            "project-a", "agent:OPENCODE_AUTH", '{"access":"old"}', ".config/auth.json"
        )

        stored = self.control_plane.store_job_secret(
            self.runner_id, self.job_id, self.generation, "OPENCODE_AUTH", '{"access":"new"}', NOW
        )

        self.assertTrue(stored)
        self.assertEqual(
            self.control_plane.resolve_job_secret(
                self.runner_id, self.job_id, self.generation, "OPENCODE_AUTH", NOW
            ),
            ('{"access":"new"}', "file"),
        )

    def test_a_runner_cannot_introduce_a_credential_nobody_configured(self) -> None:
        stored = self.control_plane.store_job_secret(
            self.runner_id, self.job_id, self.generation, "OPENROUTER_API_KEY", "sk-or-injected", NOW
        )

        self.assertFalse(stored)
        self.assertIsNone(
            self.control_plane.resolve_job_secret(
                self.runner_id, self.job_id, self.generation, "OPENROUTER_API_KEY", NOW
            )
        )

    def test_a_stale_lease_cannot_rewrite_the_projects_credential(self) -> None:
        self.control_plane.set_project_credential(
            "project-a", "agent:OPENCODE_AUTH", "original", ".config/auth.json"
        )

        with self.assertRaises(StaleLeaseError):
            self.control_plane.store_job_secret(
                self.runner_id, self.job_id, self.generation + 1, "OPENCODE_AUTH", "stolen", NOW
            )

    def test_the_git_credentials_are_not_the_runners_to_rewrite(self) -> None:
        for name in ("GITHUB_TOKEN", "GIT_SSH_KEY"):
            with self.subTest(name=name):
                with self.assertRaises(ValueError):
                    self.control_plane.store_job_secret(
                        self.runner_id, self.job_id, self.generation, name, "value", NOW
                    )

    def test_delivery_follows_the_stored_row_not_the_name(self) -> None:
        """The same key is a variable for one deployment and a file for another."""
        self.control_plane.set_project_credential("project-a", "agent:PROVIDER_KEY", "sk-value")

        self.assertEqual(
            self.control_plane.resolve_job_secret(
                self.runner_id, self.job_id, self.generation, "PROVIDER_KEY", NOW
            ),
            ("sk-value", "environment"),
        )


if __name__ == "__main__":
    unittest.main()
