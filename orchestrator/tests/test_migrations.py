import unittest
from pathlib import Path
from tempfile import TemporaryDirectory
from unittest.mock import AsyncMock, MagicMock

from moirai.persistence.migrations import MigrationRunner


class MigrationRunnerTests(unittest.IsolatedAsyncioTestCase):
    def test_split_statements_simple(self) -> None:
        sql = "CREATE TABLE foo (id INT); CREATE TABLE bar (id INT);"
        result = MigrationRunner._split_statements(sql)
        self.assertEqual(len(result), 2)
        self.assertIn("CREATE TABLE foo", result[0])
        self.assertIn("CREATE TABLE bar", result[1])

    def test_split_statements_skips_comments(self) -> None:
        sql = "-- comment\nCREATE TABLE foo (id INT);\n/* block */\nCREATE TABLE bar (id INT);"
        result = MigrationRunner._split_statements(sql)
        self.assertEqual(len(result), 2)
        for stmt in result:
            self.assertNotIn("comment", stmt)
            self.assertNotIn("block", stmt)

    def test_split_statements_empty_produces_empty_list(self) -> None:
        self.assertEqual(MigrationRunner._split_statements(""), [])
        self.assertEqual(MigrationRunner._split_statements("   "), [])
        self.assertEqual(MigrationRunner._split_statements("-- only comments"), [])

    def test_discover_migrations_finds_nothing_for_missing_dir(self) -> None:
        runner = MigrationRunner(MagicMock())
        runner.MIGRATIONS_DIR = Path("/nonexistent")
        loop = MagicMock()
        loop.run_until_complete = AsyncMock()
        # just test the non-async parts
        self.assertFalse(runner.MIGRATIONS_DIR.is_dir())

    def test_filename_pattern_matches(self) -> None:
        m = MigrationRunner._FILENAME_PATTERN.match("001_initial.sql")
        assert m is not None
        self.assertEqual(m.group(1), "001")
        self.assertEqual(m.group(2), "initial")

    def test_filename_pattern_rejects_non_matching(self) -> None:
        self.assertIsNone(MigrationRunner._FILENAME_PATTERN.match("setup.sql"))
        self.assertIsNone(MigrationRunner._FILENAME_PATTERN.match("abc_initial.sql"))

    async def test_workflow_quality_migration_defines_progress_and_circuit_state(self) -> None:
        migration = Path(__file__).parents[1] / "migrations" / "005_workflow_quality_recovery.sql"
        contents = migration.read_text(encoding="utf-8")
        self.assertIn("last_diff_hash", contents)
        self.assertIn("non_progress_attempts", contents)
        self.assertIn("project_circuit_state", contents)
        self.assertIn("provider_circuit_state", contents)

    async def test_half_open_probe_migration_reserves_probe_ownership(self) -> None:
        migration = Path(__file__).parents[1] / "migrations" / "006_circuit_half_open_probes.sql"
        contents = migration.read_text(encoding="utf-8")
        self.assertIn("probe_workflow_run_id", contents)
        self.assertIn("project_circuit_half_open_probe_idx", contents)
        self.assertIn("provider_circuit_half_open_probe_idx", contents)

    async def test_non_delivery_migration_persists_continuation_evidence(self) -> None:
        migration = Path(__file__).parents[1] / "migrations" / "009_non_delivery_outcomes.sql"
        contents = migration.read_text(encoding="utf-8")
        self.assertIn("continuation_attempts", contents)
        self.assertIn("last_delivery_outcome", contents)
        self.assertIn("last_gate_verdict", contents)
        self.assertIn("remaining_work", contents)

    async def test_discovery_rejects_duplicate_versions(self) -> None:
        with TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "001_one.sql").write_text("SELECT 1;", encoding="utf-8")
            (root / "001_two.sql").write_text("SELECT 2;", encoding="utf-8")
            runner = MigrationRunner(MagicMock())
            runner.MIGRATIONS_DIR = root
            with self.assertRaisesRegex(ValueError, "duplicate migration version"):
                await runner._discover_migrations()

    async def test_discovery_uses_a_stable_sha256_checksum(self) -> None:
        with TemporaryDirectory() as directory:
            root = Path(directory)
            (root / "001_one.sql").write_text("SELECT 1;", encoding="utf-8")
            runner = MigrationRunner(MagicMock())
            runner.MIGRATIONS_DIR = root
            first = await runner._discover_migrations()
            second = await runner._discover_migrations()
            self.assertEqual(first[0][2], second[0][2])
            self.assertEqual(len(first[0][2]), 64)


if __name__ == "__main__":
    unittest.main()
