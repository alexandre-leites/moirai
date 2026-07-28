from __future__ import annotations

import asyncio
import hashlib
import json
import logging
import os
import signal
from collections.abc import Callable
from datetime import UTC, datetime, timedelta
from pathlib import Path
from typing import TYPE_CHECKING, Any

from .config import OrchestratorConfig
from .observability import CorrelationLoggingInterceptor, Metrics, configure_logging

configure_logging()
_LOGGER = logging.getLogger(__name__)
from .health import HealthState

if TYPE_CHECKING:
    from moirai.grpc.runner_control import RunnerControlService


class CheckpointerUnavailableError(RuntimeError):
    pass


_ALLOW_NO_CHECKPOINTER_ENV = "LOOP_ALLOW_NO_CHECKPOINTER"


def _allow_no_checkpointer() -> bool:
    return os.environ.get(_ALLOW_NO_CHECKPOINTER_ENV, "").strip().lower() in {"1", "true", "yes"}


async def _build_checkpointer(database_url: str) -> Any | None:
    """Builds the LangGraph checkpointer, or returns None if explicitly permitted.

    Durable checkpointing (crash recovery, human-approval interrupts) depends on
    this succeeding. A missing dependency or a database it cannot reach is
    therefore fatal unless the caller has opted into LOOP_ALLOW_NO_CHECKPOINTER,
    which exists for tests and other reduced-capability environments only.
    """
    allow_missing = _allow_no_checkpointer()
    try:
        from langgraph.checkpoint.postgres.aio import AsyncPostgresSaver
        from psycopg import AsyncConnection
        from psycopg.rows import DictRow, dict_row
        from psycopg_pool import AsyncConnectionPool
    except ModuleNotFoundError:
        _LOGGER.exception("checkpointer dependencies are not installed")
        if allow_missing:
            return None
        raise
    pg_url = database_url.replace("+asyncpg", "")
    # AsyncPostgresSaver reads rows as mappings, so the pool has to hand out
    # dict-row connections; the psycopg default is tuple rows.
    pool = AsyncConnectionPool(
        pg_url,
        connection_class=AsyncConnection[DictRow],
        min_size=1,
        max_size=5,
        open=False,
        kwargs={"autocommit": True, "row_factory": dict_row},
    )
    try:
        await pool.open()
        checkpointer = AsyncPostgresSaver(pool)
        await checkpointer.setup()
    except Exception as exc:
        _LOGGER.exception("checkpointer database connection failed")
        await pool.close()
        if allow_missing:
            return None
        raise CheckpointerUnavailableError("checkpointer could not connect to the database") from exc
    return checkpointer


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
    # RUNNER_REGISTRATION_TOKEN is the single source of truth in .env / compose.yaml: it
    # is what the runner container presents to register (LOOP_RUNNER_REGISTRATION_TOKEN),
    # so the orchestrator must seed a token hash for that same value, not a separate,
    # unrelated variable that happens to default to the same literal.
    seed_token_value = read_optional_secret(os.environ, "RUNNER_REGISTRATION_TOKEN")
    if not seed_token_value:
        _LOGGER.warning("RUNNER_REGISTRATION_TOKEN unset — skipping runner registration token bootstrap")
        return
    token_hash = hashlib.sha256(seed_token_value.encode()).hexdigest()
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
            # Matches the 15-minute TTL the API mints for interactively-created runner
            # registration tokens (moirai.grpc.control_plane.ControlPlaneService).
            now, now + timedelta(minutes=15),
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


async def _run_retention_reaper(
    control_plane: Any,
    stop_event: asyncio.Event,
    now: Callable[[], datetime],
    interval: timedelta,
) -> None:
    if interval <= timedelta():
        raise ValueError("retention reaper interval must be positive")
    while not stop_event.is_set():
        try:
            removed = await control_plane.reap_expired_data(now())
            if any(removed.values()):
                _LOGGER.info("reaped expired data", extra=removed)
        except Exception as exc:  # noqa: BLE001 - a transient reap failure must not kill the loop
            _LOGGER.warning("retention reaper failed: %s", str(exc))
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=interval.total_seconds())
        except TimeoutError:
            pass


def register_services(
    server: Any,
    control_plane: Any,
    now: Callable[[], Any] | None = None,
    workflow_runtime: Any | None = None,
) -> RunnerControlService:
    from moirai.grpc.control_plane import ControlPlaneService
    from moirai.grpc.runner_control import RunnerControlService
    from proto import control_plane_pb2_grpc, runner_control_pb2_grpc

    control_plane_pb2_grpc.add_ControlPlaneServicer_to_server(
        ControlPlaneService(control_plane, now=now, workflow_runtime=workflow_runtime), server
    )
    runner_service = RunnerControlService(control_plane, now=now, workflow_runtime=workflow_runtime)
    runner_control_pb2_grpc.add_RunnerControlServicer_to_server(runner_service, server)
    return runner_service


async def _db_health_loop(
    pool: Any, health: HealthState, stop_event: asyncio.Event, interval: timedelta = timedelta(seconds=15)
) -> None:
    while not stop_event.is_set():
        try:
            await pool.fetchval("SELECT 1")
            health.mark_db_check(True)
        except asyncio.CancelledError:
            raise
        except Exception:
            _LOGGER.exception("database health check failed")
            health.mark_db_check(False)
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=interval.total_seconds())
        except TimeoutError:
            pass


def _log_unexpected_completion(name: str, health: HealthState, shutdown: asyncio.Event, task: asyncio.Task[None]) -> None:
    if task.cancelled():
        return
    error = task.exception()
    if error is None:
        if shutdown.is_set():
            return  # a normal, requested shutdown — the loop exited because it was told to
        _LOGGER.error("%s task ended unexpectedly without an exception", name)
    else:
        _LOGGER.error("%s task ended unexpectedly", name, exc_info=error)
    health.mark_loop_dead(name)
    shutdown.set()


def _bind_grpc_endpoint(server: Any, config: OrchestratorConfig) -> int:
    if config.grpc_tls_cert_file is None:
        return server.add_insecure_port(config.grpc_bind)
    import grpc

    certificate_chain = Path(config.grpc_tls_cert_file).read_bytes()
    private_key = Path(config.grpc_tls_key_file or "").read_bytes()
    client_ca = None if config.grpc_tls_client_ca_file is None else Path(config.grpc_tls_client_ca_file).read_bytes()
    credentials = grpc.ssl_server_credentials(
        [(private_key, certificate_chain)], root_certificates=client_ca, require_client_auth=client_ca is not None
    )
    return server.add_secure_port(config.grpc_bind, credentials)


async def _metrics_loop(control_plane: Any, metrics: Metrics, stop_event: asyncio.Event) -> None:
    while not stop_event.is_set():
        try:
            metrics.update_snapshot(await control_plane.metrics_snapshot(datetime.now(UTC)))
        except asyncio.CancelledError:
            raise
        except Exception:
            _LOGGER.exception("metrics collection failed")
        try:
            await asyncio.wait_for(stop_event.wait(), timeout=15)
        except TimeoutError:
            pass


async def serve(
    config: OrchestratorConfig | None = None,
    stop_event: asyncio.Event | None = None,
    control_plane_factory: Callable[[str], Any] | None = None,
) -> None:
    import grpc

    from moirai.persistence.control_plane import AsyncpgControlPlane

    active_config = config or OrchestratorConfig.from_environment()
    metrics = Metrics()
    metrics.start(active_config.metrics_bind)
    metrics.update()
    shutdown = stop_event or asyncio.Event()
    factory = control_plane_factory or AsyncpgControlPlane.connect
    control_plane = await factory(active_config.database_url)
    health = HealthState()
    server = grpc.aio.server(interceptors=(CorrelationLoggingInterceptor(),))
    workflow_runtime: Any | None = None

    scheduler_task: asyncio.Task[None] | None = None
    issue_sync_task: asyncio.Task[None] | None = None
    db_health_task: asyncio.Task[None] | None = None
    reaper_task: asyncio.Task[None] | None = None
    workflow_maintenance_task: asyncio.Task[None] | None = None
    metrics_task: asyncio.Task[None] | None = None

    supports_durability = isinstance(control_plane, AsyncpgControlPlane)
    if supports_durability:
        pool = control_plane.pool
        metrics_task = asyncio.create_task(_metrics_loop(control_plane, metrics, shutdown))

        from moirai.persistence.migrations import MigrationRunner
        migrations = await MigrationRunner(pool).run()
        if migrations:
            _LOGGER.info("applied migrations", extra={"migrations": migrations})

        await _bootstrap_initial_setup(pool)
        await _seed_issue_if_needed(pool)
        from moirai.issue_trackers.github_cli import GitHubCliUnavailableError, SubprocessCommandRunner, verify_gh_ready
        from moirai.scheduler import AsyncpgLeader, Scheduler
        from moirai.services.issue_sync import IssueSync, github_issue_tracker_for_project
        from moirai.workflows.code_host_factory import ProjectCodeHostFactory
        from moirai.workflows.runtime import build_persisted_runtime

        github_runner = SubprocessCommandRunner(active_config.github_token)
        if active_config.github_token is not None:
            try:
                await verify_gh_ready(github_runner)
            except GitHubCliUnavailableError:
                _LOGGER.warning("GitHub CLI is not authenticated; issue sync and code host operations will fail")
        code_hosts = ProjectCodeHostFactory(pool, command_runner=github_runner)
        checkpointer = await _build_checkpointer(active_config.database_url)
        health.mark_checkpointer(checkpointer is not None)
        workflow_runtime = build_persisted_runtime(
            pool,
            checkpointer=checkpointer,
            code_host_factory=code_hosts.code_host,
            issue_tracker_factory=code_hosts.issue_tracker,
        )

        runner_service = register_services(server, control_plane, workflow_runtime=workflow_runtime)

        scheduler = Scheduler(
            control_plane,
            runner_service.deliver_offer,
            control_plane.build_task_packet,
            timedelta(seconds=600),
        )
        leader = AsyncpgLeader(pool, 712345)
        scheduler_task = asyncio.create_task(
            scheduler.run_with_leader(
                shutdown,
                lambda: datetime.now(UTC),
                timedelta(seconds=1),
                leader,
                on_tick=health.mark_scheduler_tick,
            )
        )
        scheduler_task.add_done_callback(
            lambda task: _log_unexpected_completion("scheduler", health, shutdown, task)
        )

        issue_sync = IssueSync(
            control_plane, lambda project: github_issue_tracker_for_project(project, github_runner)
        )
        await issue_sync.restore_retry_state(datetime.now(UTC))
        issue_sync_task = asyncio.create_task(
            issue_sync.run(
                shutdown, lambda: datetime.now(UTC), timedelta(minutes=1), on_run=health.mark_issue_sync_run
            )
        )
        issue_sync_task.add_done_callback(
            lambda task: _log_unexpected_completion("issue_sync", health, shutdown, task)
        )

        db_health_task = asyncio.create_task(_db_health_loop(pool, health, shutdown))
        reaper_task = asyncio.create_task(
            _run_retention_reaper(control_plane, shutdown, lambda: datetime.now(UTC), timedelta(hours=1))
        )
        if workflow_runtime is not None:
            maintenance_leader = AsyncpgLeader(control_plane._pool, 712346)
            workflow_maintenance_task = asyncio.create_task(
                _run_workflow_maintenance_loop(
                    control_plane,
                    runner_service._advance_workflow,
                    shutdown,
                    lambda: datetime.now(UTC),
                    timedelta(seconds=30),
                    maintenance_leader,
                )
            )
    else:
        _LOGGER.warning(
            "control plane implementation does not support durability features "
            "(migrations, workflow checkpointing, scheduling, issue sync) — running in reduced capacity"
        )
        register_services(server, control_plane)
    port = _bind_grpc_endpoint(server, active_config)
    if port == 0:
        shutdown.set()
        if scheduler_task is not None:
            await scheduler_task
        if issue_sync_task is not None:
            await issue_sync_task
        if db_health_task is not None:
            await db_health_task
        if reaper_task is not None:
            await reaper_task
        if workflow_maintenance_task is not None:
            await workflow_maintenance_task
        if metrics_task is not None:
            await metrics_task
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
        if issue_sync_task is not None:
            await issue_sync_task
        if db_health_task is not None:
            await db_health_task
        if reaper_task is not None:
            await reaper_task
        if workflow_maintenance_task is not None:
            await workflow_maintenance_task
        if metrics_task is not None:
            await metrics_task
        await server.stop(grace=5)
        await control_plane.close()


async def _run_workflow_maintenance_loop(
    control_plane: Any,
    on_transition: Callable[[str, str, dict[str, object]], Any],
    stop_event: asyncio.Event,
    now: Callable[[], datetime],
    interval: timedelta,
    leader: Any,
) -> None:
    """Drains the workflow_transition_outbox (at-least-once delivery for
    transitions committed but never invoked, e.g. after a crash) and
    recovers workflow runs stalled with in-flight status but no queued or
    active work (see persistence/control_plane.py's accept_event)."""
    try:
        while not stop_event.is_set():
            if await leader.is_leader():
                current = now()
                try:
                    await control_plane.drain_pending_transitions(on_transition, current)
                    stalled = await control_plane.find_stalled_workflow_runs(current, timedelta(seconds=30))
                    for workflow_run_id in stalled:
                        await control_plane.recover_stalled_workflow_run(workflow_run_id, on_transition)
                except Exception:
                    _LOGGER.exception("workflow maintenance loop iteration failed")
            try:
                await asyncio.wait_for(stop_event.wait(), timeout=interval.total_seconds())
            except TimeoutError:
                pass
    finally:
        await leader.close()


def _install_signal_handlers(stop_event: asyncio.Event) -> None:
    loop = asyncio.get_running_loop()
    for received_signal in (signal.SIGINT, signal.SIGTERM):
        try:
            loop.add_signal_handler(received_signal, stop_event.set)
        except (NotImplementedError, RuntimeError):
            pass


if __name__ == "__main__":
    asyncio.run(serve())
