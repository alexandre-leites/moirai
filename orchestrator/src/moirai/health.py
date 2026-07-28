from __future__ import annotations

import json
import logging
import os
import time
from dataclasses import dataclass, field
from pathlib import Path
from typing import Any

_LOGGER = logging.getLogger(__name__)

_STALE_AFTER_SECONDS = 45.0


def health_file_path() -> Path:
    return Path(os.environ.get("LOOP_HEALTH_FILE", "/tmp/loop-orchestrator-health.json"))


@dataclass
class HealthState:
    """Tracks the orchestrator's own view of its health and mirrors it to disk.

    The gRPC server exposes no HTTP surface of its own, so the Docker healthcheck
    (and anything else that needs a truthful signal without a database round trip)
    reads this file instead of merely proving the process is alive.
    """

    checkpointer_enabled: bool = False
    db_ok: bool = False
    db_checked_at: float | None = None
    scheduler_last_tick_at: float | None = None
    issue_sync_last_run_at: float | None = None
    dead_loops: set[str] = field(default_factory=set)
    _path: Path = field(default_factory=health_file_path)

    def mark_checkpointer(self, enabled: bool) -> None:
        self.checkpointer_enabled = enabled
        self._write()

    def mark_db_check(self, ok: bool) -> None:
        self.db_ok = ok
        self.db_checked_at = time.time()
        self._write()

    def mark_scheduler_tick(self) -> None:
        self.scheduler_last_tick_at = time.time()
        self._write()

    def mark_issue_sync_run(self) -> None:
        self.issue_sync_last_run_at = time.time()
        self._write()

    def mark_loop_dead(self, name: str) -> None:
        self.dead_loops.add(name)
        self._write()

    def is_healthy(self) -> bool:
        if self.dead_loops:
            return False
        if not self.db_ok:
            return False
        return self.db_checked_at is None or time.time() - self.db_checked_at <= _STALE_AFTER_SECONDS

    def snapshot(self) -> dict[str, Any]:
        now = time.time()
        return {
            "healthy": self.is_healthy(),
            "checkpointer_enabled": self.checkpointer_enabled,
            "db_ok": self.db_ok,
            "db_checked_seconds_ago": None if self.db_checked_at is None else now - self.db_checked_at,
            "scheduler_last_tick_seconds_ago": (
                None if self.scheduler_last_tick_at is None else now - self.scheduler_last_tick_at
            ),
            "issue_sync_last_run_seconds_ago": (
                None if self.issue_sync_last_run_at is None else now - self.issue_sync_last_run_at
            ),
            "dead_loops": sorted(self.dead_loops),
            "updated_at": now,
        }

    def _write(self) -> None:
        try:
            self._path.write_text(json.dumps(self.snapshot()))
        except OSError:
            _LOGGER.exception("failed to write orchestrator health state to %s", self._path)
