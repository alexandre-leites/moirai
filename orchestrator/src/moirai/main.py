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
from uuid import uuid4

from .config import OrchestratorConfig, read_optional_secret
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


# How often the workflow maintenance loop runs, and how long a workflow run
# must have been untouched before that loop treats it as stalled rather than
# as a transition that is simply still in flight. The window is deliberately
# several intervals wide: a normal accept_event -> graph invocation completes
# in milliseconds, so anything older than this really has lost its driver.
_WORKFLOW_MAINTENANCE_INTERVAL = timedelta(seconds=30)
_WORKFLOW_STALL_AFTER = timedelta(minutes=2)
# Recovering a run can call out to the graph runtime (and through it to
# GitHub), so one tick takes a bounded bite rather than serialising an
# unbounded backlog behind the loop's own interval.
_WORKFLOW_STALL_BATCH = 20
_GITHUB_CHECK_POLL_BATCH = 20

_DEFAULT_SEED_PROJECT_NAME = "demo"
_DEFAULT_SEED_REPOSITORY_URL = "https://github.com/example/demo.git"
# Matches the 15-minute TTL the API mints for interactively-created runner
# registration tokens (moirai.grpc.control_plane.ControlPlaneService).
_SEED_TOKEN_TTL = timedelta(minutes=15)


async def _bootstrap_initial_setup(pool: Any) -> None:
    """Seeds the initial admin user, the seed project, and the runner registration token.

    The three steps are deliberately independent: each decides on its own whether it
    still has work to do, and none is gated on another having run. Bootstrap is the
    code whose job is initial recovery, so it must be able to recover itself — a first
    start interrupted halfway (a crash, or a secret that was not configured yet)
    completes on the next start instead of leaving the database permanently
    half-seeded with no way back other than manual SQL.
    """
    await _bootstrap_admin_user(pool)
    await _bootstrap_seed_project(pool)
    await _bootstrap_registration_token(pool)


async def _bootstrap_admin_user(pool: Any) -> None:
    user_count = await pool.fetchval("SELECT COUNT(*) FROM app.users")
    if user_count:
        return
    password = read_optional_secret(os.environ, "LOOP_INITIAL_ADMIN_PASSWORD")
    if not password:
        _LOGGER.warning("LOOP_INITIAL_ADMIN_PASSWORD unset — skipping admin bootstrap")
        return
    from moirai.persistence.authentication import AsyncpgAuthentication

    username = os.environ.get("LOOP_INITIAL_ADMIN_USERNAME", "admin")
    auth = AsyncpgAuthentication(pool)
    try:
        await auth.create_user(username, password, role="admin", now=datetime.now(UTC))
    except ValueError:
        # create_user reports a unique-username violation as ValueError, which is what
        # a second instance racing this same empty database gets. Re-read rather than
        # assume which ValueError it was: the step's goal is that an admin exists.
        if await pool.fetchval("SELECT COUNT(*) FROM app.users"):
            _LOGGER.info("initial admin user already exists", extra={"username": username})
            return
        raise
    _LOGGER.info("created initial admin user", extra={"username": username})


async def _bootstrap_seed_project(pool: Any) -> None:
    seed_project = os.environ.get("LOOP_SEED_PROJECT_NAME", _DEFAULT_SEED_PROJECT_NAME).strip()
    if not seed_project:
        _LOGGER.info("LOOP_SEED_PROJECT_NAME is empty — skipping seed project bootstrap")
        return
    existing = await pool.fetchval("SELECT COUNT(*) FROM app.projects WHERE name = $1", seed_project)
    if existing:
        return
    seed_repo = os.environ.get("LOOP_SEED_PROJECT_REPOSITORY_URL")
    now = datetime.now(UTC)
    # ON CONFLICT, rather than the SELECT above alone, is what makes the insert
    # idempotent: two orchestrator instances starting together both pass the check.
    await pool.execute(
        """
        INSERT INTO app.projects
            (id, name, enabled, repository_mode, repository_url, default_branch, configuration, created_at, updated_at)
        VALUES ($1, $2, true, 'managed_clone', $3, 'main', $4::jsonb, $5, $5)
        ON CONFLICT (name) DO NOTHING
        """,
        uuid4(), seed_project,
        seed_repo or _DEFAULT_SEED_REPOSITORY_URL,
        json.dumps({"required_runner_labels": ["linux"]}),
        now,
    )
    _LOGGER.info("created seed project", extra={"project": seed_project})


async def _bootstrap_registration_token(pool: Any) -> None:
    # RUNNER_REGISTRATION_TOKEN is the single source of truth in .env / compose.yaml: it
    # is what the runner container presents to register (LOOP_RUNNER_REGISTRATION_TOKEN),
    # so the orchestrator must seed a token hash for that same value, not a separate,
    # unrelated variable that happens to default to the same literal.
    seed_token_value = read_optional_secret(os.environ, "RUNNER_REGISTRATION_TOKEN")
    if not seed_token_value:
        _LOGGER.warning("RUNNER_REGISTRATION_TOKEN unset — skipping runner registration token bootstrap")
        return
    token_hash = hashlib.sha256(seed_token_value.encode()).hexdigest()
    # Counts rows with this hash whatever their state, including already used or
    # expired ones: registration tokens are single-use (persistence.control_plane's
    # register_runner stamps used_at), so re-seeding a consumed hash would silently
    # hand a spent secret a second registration.
    existing_token = await pool.fetchval(
        "SELECT COUNT(*) FROM app.runner_registration_tokens WHERE token_hash = $1", token_hash
    )
    if existing_token:
        return
    seed_labels = os.environ.get("LOOP_SEED_TOKEN_LABELS", "linux")
    labels = [label.strip() for label in seed_labels.split(",") if label.strip()]
    now = datetime.now(UTC)
    await pool.execute(
        """
        INSERT INTO app.runner_registration_tokens
            (id, token_hash, allowed_labels, created_at, expires_at)
        VALUES ($1, $2, $3::jsonb, $4, $5)
        ON CONFLICT (token_hash) DO NOTHING
        """,
        uuid4(), token_hash,
        json.dumps(labels),
        now, now + _SEED_TOKEN_TTL,
    )
    _LOGGER.info("created seed registration token", extra={"labels": labels})


async def _seed_issue_if_needed(pool: Any) -> None:
    seed_title = os.environ.get("LOOP_SEED_ISSUE_TITLE")
    if not seed_title:
        return
    project_id_from_db = await pool.fetchval(
        "SELECT id FROM app.projects WHERE name = $1",
        os.environ.get("LOOP_SEED_PROJECT_NAME", _DEFAULT_SEED_PROJECT_NAME).strip(),
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
    now = datetime.now(UTC)
    issue_id = uuid4()
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
    issue_sync: Any | None = None,
) -> RunnerControlService:
    from moirai.grpc.control_plane import ControlPlaneService
    from moirai.grpc.runner_control import RunnerControlService
    from proto import control_plane_pb2_grpc, runner_control_pb2_grpc

    runner_service = RunnerControlService(control_plane, now=now, workflow_runtime=workflow_runtime)
    control_plane_pb2_grpc.add_ControlPlaneServicer_to_server(
        ControlPlaneService(
            control_plane,
            now=now,
            workflow_runtime=workflow_runtime,
            runner_control=runner_service,
            issue_sync=issue_sync,
        ),
        server,
    )
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
        from moirai.issue_trackers.github_cli import (
            GitHubCliUnavailableError,
            SubprocessCommandRunner,
            verify_gh_ready,
        )
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

        issue_sync = IssueSync(
            control_plane, lambda project: github_issue_tracker_for_project(project, github_runner)
        )
        await issue_sync.restore_retry_state(datetime.now(UTC))

        runner_service = register_services(
            server, control_plane, workflow_runtime=workflow_runtime, issue_sync=issue_sync
        )

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
                    _WORKFLOW_MAINTENANCE_INTERVAL,
                    maintenance_leader,
                )
            )
            # Without this the only recovery loop in the process could die
            # silently, leaving the orchestrator reporting healthy while every
            # stalled run stayed stalled.
            workflow_maintenance_task.add_done_callback(
                lambda task: _log_unexpected_completion("workflow_maintenance", health, shutdown, task)
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
    """Repairs workflow runs that lost their way between the database and the
    graph runtime.

    Three arms, in dependency order:

    1. Drain the workflow_transition_outbox (at-least-once delivery for
       transitions committed but never invoked, e.g. after a crash).
    2. Close execution requests that can never be executed or reported on, so
       the runs holding them stop looking busy.
    3. Repair runs whose status says an execution should be in flight while
       nothing is: re-queue the execution that was lost, or hand a run whose
       execution did report back to the graph at the status that was committed
       (see persistence/control_plane.py's recover_stalled_workflow_run for
       which of the two applies and why the distinction matters).

    Arm 2 has to run before arm 3: a leaked `dispatched` request is exactly
    what makes arm 3's detector unable to see the run (issue #94).

    Arm 3 can call out to the graph runtime, which reaches GitHub, so one run's
    failure must not cost the rest of the batch their turn.
    """
    try:
        while not stop_event.is_set():
            stalled: tuple[str, ...] = ()
            waiting_for_checks: tuple[str, ...] = ()
            # The leadership probe is inside the guard on purpose: AsyncpgLeader
            # re-raises whatever the database did, and an unhandled exception
            # here now ends the whole process through the task's done-callback.
            # A failover blip must cost this loop one tick, not the orchestrator.
            try:
                current = now()
                if await leader.is_leader():
                    await control_plane.drain_pending_transitions(on_transition, current)
                    orphaned = await control_plane.close_orphaned_execution_requests(
                        current, _WORKFLOW_STALL_AFTER
                    )
                    if orphaned:
                        _LOGGER.info(
                            "closed orphaned execution requests", extra={"requests": orphaned}
                        )
                    waiting_for_checks = await control_plane.find_workflow_runs_waiting_for_checks(
                        _GITHUB_CHECK_POLL_BATCH
                    )
                    stalled = await control_plane.find_stalled_workflow_runs(
                        current, _WORKFLOW_STALL_AFTER, _WORKFLOW_STALL_BATCH
                    )
            except Exception:
                _LOGGER.exception("workflow maintenance loop iteration failed")
            for workflow_run_id in waiting_for_checks:
                try:
                    await on_transition(workflow_run_id, "waiting_github_checks", {"poll_github_checks": True})
                except Exception:
                    _LOGGER.exception(
                        "GitHub check polling failed",
                        extra={"workflow_run_id": workflow_run_id},
                    )
            for workflow_run_id in stalled:
                try:
                    recovered = await control_plane.recover_stalled_workflow_run(
                        workflow_run_id, on_transition, current
                    )
                except Exception:
                    _LOGGER.exception(
                        "stalled workflow run recovery failed",
                        extra={"workflow_run_id": workflow_run_id},
                    )
                    continue
                if recovered:
                    _LOGGER.info(
                        "recovered stalled workflow run",
                        extra={"workflow_run_id": workflow_run_id},
                    )
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
