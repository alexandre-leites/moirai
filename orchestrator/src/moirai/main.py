from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
import signal
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from typing import Any

logging.basicConfig(level=logging.INFO, format='%(message)s', force=True)
_LOGGER = logging.getLogger(__name__)

from .config import OrchestratorConfig


async def _build_checkpointer(database_url: str) -> Any | None:
    try:
        from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
        from psycopg_pool import AsyncConnectionPool

        pg_url = database_url.replace("+asyncpg", "")
        pool = AsyncConnectionPool(pg_url, min_size=1, max_size=5, open=False, kwargs={"autocommit": True})
        await pool.open()
        checkpointer = AsyncPostgresSaver(pool)
        await checkpointer.setup()
        return checkpointer
    except Exception as exc:
        _LOGGER.info("checkpointer unavailable: %s", str(exc))
        return None


async def _bootstrap_initial_setup(pool: Any) -> None:
    user_count = await pool.fetchval("SELECT COUNT(*) FROM app.users")
    if user_count and user_count > 0:
        return
    from moirai.config import read_optional_secret
    from moirai.persistence.authentication import AsyncpgAuthentication
    username = os.environ.get("LOOP_INITIAL_ADMIN_USERNAME", "admin")
    password = read_optional_secret(os.environ, "LOOP_INITIAL_ADMIN_PASSWORD")
    if not password:
        _LOGGER.warning("LOOP_INITIAL_ADMIN_PASSWORD unset — skipping admin bootstrap")
        return
    auth = AsyncpgAuthentication(pool)
    await auth.create_user(username, password, role="admin", now=datetime.now(UTC))
    _LOGGER.info("created initial admin user", extra={"username": username})
    seed_project = os.environ.get("LOOP_SEED_PROJECT_NAME", "demo")
    seed_repo = os.environ.get("LOOP_SEED_PROJECT_REPOSITORY_URL")
    existing = await pool.fetchval("SELECT COUNT(*) FROM app.projects WHERE name = $1", seed_project)
    if existing is None or existing == 0:
        from uuid import uuid4
        project_id = uuid4()
        import json
        now = datetime.now(UTC)
        await pool.execute(
            """
            INSERT INTO app.projects
                (id, name, enabled, repository_mode, repository_url, default_branch, configuration, created_at, updated_at)
            VALUES ($1, $2, true, 'managed_clone', $3, 'main', $4::jsonb, $5, $5)
            """,
            project_id, seed_project,
            seed_repo or "https://github.com/example/demo.git",
            json.dumps({"required_runner_labels": ["linux"]}),
            now,
        )
        _LOGGER.info("created seed project", extra={"project": seed_project})
    else:
        project_id = await pool.fetchval("SELECT id FROM app.projects WHERE name = $1", seed_project)
    seed_labels = os.environ.get("LOOP_SEED_TOKEN_LABELS", "linux")
    labels = [label.strip() for label in seed_labels.split(",") if label.strip()]
    token_hash = hashlib.sha256(
        os.environ.get("LOOP_SEED_TOKEN_VALUE", "dev-registration-token").encode()
    ).hexdigest()
    existing_token = await pool.fetchval(
        "SELECT COUNT(*) FROM app.runner_registration_tokens WHERE token_hash = $1", token_hash
    )
    if existing_token is None or existing_token == 0:
        token_id = uuid4()
        now = datetime.now(UTC)
        await pool.execute(
            """
            INSERT INTO app.runner_registration_tokens
                (id, token_hash, allowed_labels, created_at, expires_at)
            VALUES ($1, $2, $3::jsonb, $4, $5)
            """,
            token_id, token_hash,
            json.dumps(labels),
            now, now.replace(year=now.year + 10),
        )
        _LOGGER.info("created seed registration token", extra={"labels": labels})


async def _seed_issue_if_needed(pool: Any) -> None:
    seed_title = os.environ.get("LOOP_SEED_ISSUE_TITLE")
    if not seed_title:
        return
    project_id_from_db = await pool.fetchval(
        "SELECT id FROM app.projects WHERE name = $1",
        os.environ.get("LOOP_SEED_PROJECT_NAME", "demo"),
    )
    if project_id_from_db is None:
        _LOGGER.info("no seed project found — skipping seed issue")
        return
    existing_issue = await pool.fetchval(
        "SELECT COUNT(*) FROM app.issues WHERE project_id = $1 AND external_id = 'seed-1'",
        project_id_from_db,
    )
    if existing_issue is not None and existing_issue > 0:
        return
    from uuid import uuid4 as _uuid4
    now = datetime.now(UTC)
    issue_id = _uuid4()
    await pool.execute(
        """
        INSERT INTO app.issues
            (id, project_id, provider, external_id, display_number, title, body, url,
             state, labels, priority, eligible, human_approval_required,
             external_created_at, external_updated_at, last_synced_at)
        VALUES ($1, $2, 'seed', 'seed-1', '1', $3, $4, '',
                'open', '[]'::jsonb, 100, true, false, $5, $5, $5)
        """,
        issue_id,
        project_id_from_db,
        seed_title,
        os.environ.get("LOOP_SEED_ISSUE_BODY", ""),
        now,
    )
    _LOGGER.info("created seed issue", extra={"title": seed_title})


def register_services(
    server: Any,
    control_plane: Any,
    now: Callable[[], Any] | None = None,
    workflow_runtime: Any | None = None,
) -> Any:
    from moirai.grpc.control_plane import ControlPlaneService
    from moirai.grpc.runner_control import RunnerControlService
    from proto import control_plane_pb2_grpc, runner_control_pb2_grpc

    control_plane_pb2_grpc.add_ControlPlaneServicer_to_server(
        ControlPlaneService(control_plane, now=now), server
    )
    runner_service = RunnerControlService(control_plane, now=now, workflow_runtime=workflow_runtime)
    runner_control_pb2_grpc.add_RunnerControlServicer_to_server(runner_service, server)
    return runner_service


async def serve(
    config: OrchestratorConfig | None = None,
    stop_event: asyncio.Event | None = None,
    control_plane_factory: Callable[[str], Any] | None = None,
) -> None:
    import grpc

    from moirai.persistence.control_plane import AsyncpgControlPlane

    active_config = config or OrchestratorConfig.from_environment()
    shutdown = stop_event or asyncio.Event()
    factory = control_plane_factory or AsyncpgControlPlane.connect
    control_plane = await factory(active_config.database_url)
    if isinstance(control_plane, AsyncpgControlPlane):
        from moirai.persistence.migrations import MigrationRunner
        migrations = await MigrationRunner(control_plane._pool).run()
        if migrations:
            _LOGGER.info("applied migrations", extra={"migrations": migrations})
    if isinstance(control_plane, AsyncpgControlPlane):
        await _bootstrap_initial_setup(control_plane._pool)
        await _seed_issue_if_needed(control_plane._pool)
    server = grpc.aio.server()
    workflow_runtime: Any | None = None
    if isinstance(control_plane, AsyncpgControlPlane):
        from moirai.code_hosts import GitHubCliCodeHost
        from moirai.issue_trackers import GitHubCliIssueTracker, GitHubRepository
        from moirai.issue_trackers.github_cli import SubprocessCommandRunner, verify_gh_ready
        from moirai.workflows.runtime import build_persisted_runtime
        repos_by_id: dict[str, GitHubRepository] = {}
        async with control_plane._pool.acquire() as conn:
            rows = await conn.fetch("SELECT id, repository_url FROM app.projects WHERE enabled = true")
            for row in rows:
                try:
                    repos_by_id[str(row["id"])] = GitHubRepository.from_remote_url(str(row["repository_url"]))
                except ValueError:
                    pass
        gh_runner = SubprocessCommandRunner(github_token=active_config.github_token)
        code_host = GitHubCliCodeHost(next(iter(repos_by_id.values())), runner=gh_runner) if repos_by_id else None
        issue_tracker: Any = None
        if repos_by_id:
            repo = next(iter(repos_by_id.values()))
            issue_tracker = GitHubCliIssueTracker(repo, runner=gh_runner)
        if code_host is not None or issue_tracker is not None:
            await verify_gh_ready(gh_runner)
        checkpointer = await _build_checkpointer(active_config.database_url)
        workflow_runtime = build_persisted_runtime(
            control_plane._pool, checkpointer=checkpointer, code_host=code_host, issue_tracker=issue_tracker,
        )
    runner_service = register_services(server, control_plane, workflow_runtime=workflow_runtime)
    scheduler_task: asyncio.Task[None] | None = None
    issue_sync_task: asyncio.Task[None] | None = None
    if isinstance(control_plane, AsyncpgControlPlane):
        from moirai.scheduler import AsyncpgLeader, Scheduler
        from moirai.services.issue_sync import IssueSync, github_issue_tracker_for_project

        scheduler = Scheduler(
            control_plane,
            runner_service.deliver_offer,
            control_plane.build_task_packet,
            timedelta(seconds=600),
        )
        leader = AsyncpgLeader(control_plane._pool, 712345)
        scheduler_task = asyncio.create_task(
            scheduler.run_with_leader(
                shutdown,
                lambda: datetime.now(UTC),
                timedelta(seconds=1),
                leader,
            )
        )
        issue_sync = IssueSync(control_plane, github_issue_tracker_for_project)
        await issue_sync.restore_retry_state(datetime.now(UTC))
        issue_sync_task = asyncio.create_task(
            issue_sync.run(shutdown, lambda: datetime.now(UTC), timedelta(minutes=1))
        )
    port = server.add_insecure_port(active_config.grpc_bind)
    if port == 0:
        shutdown.set()
        if scheduler_task is not None:
            await scheduler_task
        if issue_sync_task is not None:
            await issue_sync_task
        await control_plane.close()
        raise RuntimeError("orchestrator gRPC endpoint could not bind")
    _install_signal_handlers(shutdown)
    try:
        await server.start()
        print(
            json.dumps(
                {"service": "orchestrator", "message": "started", "bind": active_config.grpc_bind},
                separators=(",", ":"),
            ),
            flush=True,
        )
        await shutdown.wait()
    finally:
        shutdown.set()
        if scheduler_task is not None:
            await scheduler_task
        await server.stop(grace=5)
        await control_plane.close()


def _install_signal_handlers(stop_event: asyncio.Event) -> None:
    loop = asyncio.get_running_loop()
    for received_signal in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(received_signal, stop_event.set)
        except (NotImplementedError, RuntimeError):
            pass


if __name__ == "__main__":
    asyncio.run(serve())
