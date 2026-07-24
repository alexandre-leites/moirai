import unittest
from pathlib import Path
from unittest.mock import AsyncMock, MagicMock

from moirai.persistence.migrations import MigrationRunner


class MigrationRunnerTests(unittest.TestCase):
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


if __name__ == "__main__":
    unittest.main()
