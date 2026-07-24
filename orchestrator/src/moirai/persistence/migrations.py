from __future__ import annotations

import re
from pathlib import Path
from typing import Any


class MigrationRunner:
    MIGRATIONS_DIR = Path(__file__).resolve().parent.parent.parent.parent / "migrations"
    TRACKING_TABLE = "app.schema_version"
    _FILENAME_PATTERN = re.compile(r"^(\d+)_(.+)\.sql$")

    def __init__(self, pool: Any, schema: str = "app") -> None:
        self._pool = pool
        self._schema = schema

    async def _ensure_tracking_table(self, conn: Any) -> None:
        await conn.execute(f"""
            CREATE SCHEMA IF NOT EXISTS {self._schema};
            CREATE TABLE IF NOT EXISTS {self.TRACKING_TABLE} (
                version INTEGER PRIMARY KEY,
                name TEXT NOT NULL,
                applied_at TIMESTAMPTZ NOT NULL DEFAULT now(),
                checksum TEXT NOT NULL
            )
        """)

    async def _discover_migrations(self) -> list[tuple[int, str, str, str]]:
        if not self.MIGRATIONS_DIR.is_dir():
            return []
        entries: list[tuple[int, str, str, str]] = []
        for path in sorted(self.MIGRATIONS_DIR.iterdir()):
            if not path.is_file() or path.suffix != ".sql":
                continue
            m = self._FILENAME_PATTERN.match(path.name)
            if not m:
                continue
            version = int(m.group(1))
            name = m.group(2)
            contents = path.read_text("utf-8")
            checksum = str(hash(contents))
            entries.append((version, name, checksum, contents))
        entries.sort(key=lambda x: x[0])
        return entries

    async def _applied_versions(self, conn: Any) -> set[int]:
        rows = await conn.fetch(
            f"SELECT version, checksum FROM {self.TRACKING_TABLE} ORDER BY version"
        )
        return {r["version"] for r in rows}

    async def run(self) -> list[str]:
        migrations = await self._discover_migrations()
        if not migrations:
            return []
        async with self._pool.acquire() as conn:
            await self._ensure_tracking_table(conn)
            applied = await self._applied_versions(conn)
            results: list[str] = []
            for version, name, checksum, contents in migrations:
                if version in applied:
                    continue
                async with conn.transaction():
                    for statement in self._split_statements(contents):
                        if statement.strip():
                            await conn.execute(statement)
                    await conn.execute(
                        f"INSERT INTO {self.TRACKING_TABLE} (version, name, checksum) VALUES ($1, $2, $3)",
                        version,
                        name,
                        checksum,
                    )
                results.append(f"{version:03d}_{name}")
            return results

    @staticmethod
    def _split_statements(sql: str) -> list[str]:
        statements: list[str] = []
        for stmt in sql.split(";"):
            trimmed = stmt.strip()
            if trimmed:
                in_comment = False
                filtered_lines: list[str] = []
                for line in trimmed.splitlines():
                    stripped_line = line.strip()
                    if not in_comment and stripped_line.startswith("--"):
                        continue
                    if stripped_line.startswith("/*"):
                        in_comment = True
                    if in_comment and ("*/" in stripped_line):
                        in_comment = False
                        continue
                    if in_comment:
                        continue
                    if not in_comment:
                        filtered_lines.append(line)
                full = " ".join(filtered_lines).strip()
                if full:
                    statements.append(full + ";")
        return statements
