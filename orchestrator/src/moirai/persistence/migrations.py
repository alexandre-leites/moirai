from __future__ import annotations

import re
from hashlib import sha256
from pathlib import Path
from typing import Any


def _is_legacy_checksum(value: str) -> bool:
    return len(value) != 64 or any(character not in "0123456789abcdef" for character in value.lower())


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
        versions: set[int] = set()
        for path in sorted(self.MIGRATIONS_DIR.iterdir()):
            if not path.is_file() or path.suffix != ".sql":
                continue
            match = self._FILENAME_PATTERN.match(path.name)
            if not match:
                continue
            version = int(match.group(1))
            if version in versions:
                raise ValueError(f"duplicate migration version: {version}")
            versions.add(version)
            name = match.group(2)
            contents = path.read_text("utf-8")
            checksum = sha256(contents.encode("utf-8")).hexdigest()
            entries.append((version, name, checksum, contents))
        entries.sort(key=lambda entry: entry[0])
        return entries

    async def _applied_versions(self, conn: Any) -> dict[int, str]:
        rows = await conn.fetch(
            f"SELECT version, checksum FROM {self.TRACKING_TABLE} ORDER BY version"
        )
        return {int(row["version"]): str(row["checksum"]) for row in rows}

    async def run(self) -> list[str]:
        migrations = await self._discover_migrations()
        if not migrations:
            return []
        async with self._pool.acquire() as conn:
            await self._ensure_tracking_table(conn)
            applied = await self._applied_versions(conn)
            results: list[str] = []
            for version, name, checksum, contents in migrations:
                previous_checksum = applied.get(version)
                if previous_checksum is not None:
                    if previous_checksum != checksum:
                        if _is_legacy_checksum(previous_checksum):
                            await conn.execute(
                                f"UPDATE {self.TRACKING_TABLE} SET checksum = $2 WHERE version = $1",
                                version,
                                checksum,
                            )
                        else:
                            raise ValueError(f"migration checksum mismatch: {version:03d}_{name}")
                    continue
                async with conn.transaction():
                    await conn.execute(contents)
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
