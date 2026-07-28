from __future__ import annotations

import json
import tempfile
import unittest
from pathlib import Path

from moirai.health import HealthState


class HealthStateTests(unittest.TestCase):
    def setUp(self) -> None:
        self._tmpdir = tempfile.TemporaryDirectory()
        self.path = Path(self._tmpdir.name) / "health.json"

    def tearDown(self) -> None:
        self._tmpdir.cleanup()

    def _state(self) -> HealthState:
        return HealthState(_path=self.path)

    def test_starts_unhealthy_until_a_db_check_succeeds(self) -> None:
        state = self._state()
        self.assertFalse(state.is_healthy())

    def test_db_check_success_makes_it_healthy(self) -> None:
        state = self._state()
        state.mark_db_check(True)
        self.assertTrue(state.is_healthy())

    def test_db_check_failure_makes_it_unhealthy(self) -> None:
        state = self._state()
        state.mark_db_check(True)
        state.mark_db_check(False)
        self.assertFalse(state.is_healthy())

    def test_a_dead_loop_makes_it_unhealthy_even_with_a_good_db(self) -> None:
        state = self._state()
        state.mark_db_check(True)
        state.mark_loop_dead("scheduler")
        self.assertFalse(state.is_healthy())

    def test_snapshot_reports_checkpointer_and_loop_liveness(self) -> None:
        state = self._state()
        state.mark_checkpointer(True)
        state.mark_db_check(True)
        state.mark_scheduler_tick()
        state.mark_issue_sync_run()
        snapshot = state.snapshot()
        self.assertTrue(snapshot["healthy"])
        self.assertTrue(snapshot["checkpointer_enabled"])
        self.assertTrue(snapshot["db_ok"])
        self.assertIsNotNone(snapshot["scheduler_last_tick_seconds_ago"])
        self.assertIsNotNone(snapshot["issue_sync_last_run_seconds_ago"])
        self.assertEqual(snapshot["dead_loops"], [])

    def test_writes_are_mirrored_to_disk_as_json(self) -> None:
        state = self._state()
        state.mark_db_check(True)
        on_disk = json.loads(self.path.read_text())
        self.assertTrue(on_disk["db_ok"])
        self.assertTrue(on_disk["healthy"])

    def test_write_failure_is_swallowed_not_raised(self) -> None:
        state = HealthState(_path=Path(self._tmpdir.name) / "missing-dir" / "health.json")
        state.mark_db_check(True)  # must not raise even though the parent directory does not exist


if __name__ == "__main__":
    unittest.main()
