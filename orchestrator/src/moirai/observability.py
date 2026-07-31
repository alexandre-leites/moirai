from __future__ import annotations

import json
import logging
from datetime import UTC, datetime
from typing import Any

import grpc
from prometheus_client import CollectorRegistry, Gauge, start_http_server

_STANDARD_RECORD_FIELDS = frozenset(logging.makeLogRecord({}).__dict__)


class JSONFormatter(logging.Formatter):
    def format(self, record: logging.LogRecord) -> str:
        payload: dict[str, Any] = {
            "timestamp": datetime.fromtimestamp(record.created, UTC).isoformat(),
            "level": record.levelname,
            "logger": record.name,
            "message": record.getMessage(),
        }
        for key, value in record.__dict__.items():
            if key not in _STANDARD_RECORD_FIELDS and not key.startswith("_"):
                payload[key] = value
        if record.exc_info:
            payload["exception"] = self.formatException(record.exc_info)
        return json.dumps(payload, default=str, separators=(",", ":"))


def configure_logging() -> None:
    handler = logging.StreamHandler()
    handler.setFormatter(JSONFormatter())
    logging.basicConfig(level=logging.INFO, handlers=[handler], force=True)


class CorrelationLoggingInterceptor(grpc.aio.ServerInterceptor):
    async def intercept_service(self, continuation: Any, handler_call_details: Any) -> Any:
        metadata = dict(handler_call_details.invocation_metadata or ())
        request_id = metadata.get("x-request-id")
        logging.getLogger("moirai.grpc").info(
            "grpc request", extra={"request_id": request_id, "method": handler_call_details.method}
        )
        return await continuation(handler_call_details)


class Metrics:
    def __init__(self) -> None:
        registry = CollectorRegistry()
        self._queue_depth = Gauge("moirai_queue_depth", "Eligible issue queue depth", registry=registry)
        self._active_workflows = Gauge("moirai_active_workflow_count", "Active workflow count", registry=registry)
        self._runner_heartbeat_age = Gauge(
            "moirai_runner_heartbeat_age_seconds", "Age of the oldest runner heartbeat", registry=registry
        )
        self._scheduled_job_count = Gauge(
            "moirai_scheduled_job_count", "Scheduled job count", registry=registry
        )
        self._registry = registry

    def start(self, bind: str) -> None:
        host, port = _split_bind(bind)
        start_http_server(port, addr=host, registry=self._registry)

    def update(self, *, queue_depth: float = 0, active_workflows: float = 0, runner_heartbeat_age: float = 0, scheduled_job_count: float = 0) -> None:
        self._queue_depth.set(queue_depth)
        self._active_workflows.set(active_workflows)
        self._runner_heartbeat_age.set(runner_heartbeat_age)
        self._scheduled_job_count.set(scheduled_job_count)

    def update_snapshot(self, snapshot: dict[str, float]) -> None:
        self.update(
            queue_depth=snapshot["queue_depth"],
            active_workflows=snapshot["active_workflows"],
            runner_heartbeat_age=snapshot["runner_heartbeat_age"],
            scheduled_job_count=snapshot["scheduled_job_count"],
        )


def _split_bind(bind: str) -> tuple[str, int]:
    host, port = bind.rsplit(":", 1)
    return host, int(port)
