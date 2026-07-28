import unittest
from typing import cast

from moirai.workflows.task_packets import (
    PipelineCommand,
    RepositorySource,
    TaskExecutionRequest,
    build_task_packet,
    planner_task_execution,
    task_execution,
)


class TaskPacketTests(unittest.TestCase):
    def test_planner_packet_preserves_current_restricted_contract(self) -> None:
        packet = build_task_packet(
            planner_task_execution(
                job_id="job-123456789",
                project_id="project-1",
                issue_external_id="42",
                issue_title="Implement scheduling",
                issue_body="Acceptance criteria",
                repository_mode="managed_clone",
                repository_url="https://example.test/repository.git",
                local_repository_path=None,
                default_branch="main",
            )
        )
        self.assertEqual(packet["protocolVersion"], "1.0")
        self.assertEqual(packet["executionId"], "job-123456789-plan")
        self.assertEqual(packet["role"], "planner")
        self.assertEqual(
            packet["repository"],
            {
                "projectId": "project-1",
                "mode": "managed_clone",
                "defaultBranch": "main",
                "branch": "agent/42/job-1234",
                "url": "https://example.test/repository.git",
            },
        )
        self.assertEqual(packet["constraints"], {"mayModifyFiles": False, "mayPush": False, "mayMerge": False})

    def test_role_packet_uses_role_specific_permissions_and_execution_identity(self) -> None:
        packet = build_task_packet(
            task_execution(
                job_id="job-1",
                execution_id="request-1-implement",
                role="developer",
                project_id="project-1",
                issue_external_id="42",
                issue_title="Title",
                issue_body="Body",
                repository_mode="managed_clone",
                repository_url="https://example.test/repository.git",
                local_repository_path=None,
                default_branch="main",
            )
        )
        self.assertEqual(packet["executionId"], "request-1-implement")
        self.assertEqual(packet["role"], "developer")
        self.assertEqual(packet["constraints"], {"mayModifyFiles": True, "mayPush": True, "mayMerge": False})

    def test_pipeline_packet_is_read_only_and_carries_configured_commands(self) -> None:
        packet = build_task_packet(
            TaskExecutionRequest(
                job_id="job-1", execution_id="request-1-pipeline", role="pipeline",
                objective="Run local pipeline", issue_external_id="42", issue_title="Title", issue_body="Body",
                repository=RepositorySource(project_id="project-1", mode="managed_clone", default_branch="main", branch="agent/42/job-1", url="https://example.test/repository.git"),
                timeout_seconds=600, may_modify_files=False, may_push=False, may_merge=False,
                pipeline=(PipelineCommand("make test", 300),),
            )
        )
        self.assertEqual(packet["role"], "pipeline")
        self.assertEqual(packet["pipeline"], [{"command": "make test", "timeoutSeconds": 300}])
        self.assertEqual(packet["constraints"], {"mayModifyFiles": False, "mayPush": False, "mayMerge": False})

    def test_factory_rejects_invalid_repository_source(self) -> None:
        with self.assertRaisesRegex(ValueError, "repository URL"):
            build_task_packet(
                planner_task_execution(
                    job_id="job-1",
                    project_id="project-1",
                    issue_external_id="42",
                    issue_title="Title",
                    issue_body="Body",
                    repository_mode="managed_clone",
                    repository_url=None,
                    local_repository_path=None,
                    default_branch="main",
                )
            )
        with self.assertRaisesRegex(ValueError, "repository mode"):
            planner_task_execution(
                job_id="job-1",
                project_id="project-1",
                issue_external_id="42",
                issue_title="Title",
                issue_body="Body",
                repository_mode="unknown",
                repository_url=None,
                local_repository_path=None,
                default_branch="main",
            )

    def test_factory_represents_non_planner_roles_without_persistence_branches(self) -> None:
        packet = build_task_packet(
            TaskExecutionRequest(
                job_id="job-1",
                execution_id="job-1-implement",
                role="developer",
                objective="Implement issue",
                issue_external_id="42",
                issue_title="Title",
                issue_body="Body",
                repository=RepositorySource(
                    project_id="project-1",
                    mode="existing_path",
                    default_branch="main",
                    branch="agent/42/job-1",
                    local_path="/repositories/project-1",
                ),
                timeout_seconds=600,
                may_modify_files=True,
                may_push=False,
                may_merge=False,
            )
        )
        self.assertEqual(packet["role"], "developer")
        repository = cast(dict[str, object], packet["repository"])
        constraints = cast(dict[str, object], packet["constraints"])
        self.assertEqual(repository["localPath"], "/repositories/project-1")
        self.assertEqual(constraints["mayModifyFiles"], True)


if __name__ == "__main__":
    unittest.main()
