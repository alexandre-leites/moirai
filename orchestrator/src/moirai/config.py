from __future__ import annotations

import os
import stat
from collections.abc import Mapping
from dataclasses import dataclass
from pathlib import Path


class ConfigurationError(ValueError):
    pass


@dataclass(frozen=True)
class OrchestratorConfig:
    database_url: str
    grpc_bind: str
    github_token: str | None = None

    @classmethod
    def from_environment(cls, environment: Mapping[str, str] | None = None) -> OrchestratorConfig:
        values = os.environ if environment is None else environment
        return cls(
            database_url=read_secret(values, "LOOP_DATABASE_URL"),
            grpc_bind=read_bind(values.get("LOOP_GRPC_BIND", "0.0.0.0:50051")),
            github_token=read_optional_secret(values, "LOOP_GITHUB_TOKEN"),
        )


def read_secret(environment: Mapping[str, str], name: str) -> str:
    direct = environment.get(name)
    file_name = f"{name}_FILE"
    file_path = environment.get(file_name)
    if direct and file_path:
        raise ConfigurationError(f"configure only one of {name} or {file_name}")
    if direct:
        return require_value(name, direct)
    if not file_path:
        raise ConfigurationError(f"{name} or {file_name} is required")
    return read_secret_file(file_name, file_path)


def read_optional_secret(environment: Mapping[str, str], name: str) -> str | None:
    if name not in environment and f"{name}_FILE" not in environment:
        return None
    return read_secret(environment, name)


def read_secret_file(name: str, path_value: str) -> str:
    path = Path(path_value)
    try:
        metadata = path.stat()
    except OSError as error:
        raise ConfigurationError(f"{name} cannot be read") from error
    if not stat.S_ISREG(metadata.st_mode):
        raise ConfigurationError(f"{name} must reference a regular file")
    if metadata.st_size > 16_384:
        raise ConfigurationError(f"{name} exceeds the maximum supported size")
    try:
        value = path.read_text(encoding="utf-8")
    except (OSError, UnicodeError) as error:
        raise ConfigurationError(f"{name} cannot be read") from error
    return require_value(name, value)


def require_value(name: str, value: str) -> str:
    normalized = value.strip()
    if not normalized:
        raise ConfigurationError(f"{name} must not be empty")
    if "\x00" in normalized:
        raise ConfigurationError(f"{name} contains an invalid character")
    return normalized


def read_bind(value: str) -> str:
    bind = require_value("LOOP_GRPC_BIND", value)
    if bind.startswith("["):
        host_end = bind.find("]")
        valid = host_end > 1 and bind[host_end + 1 :].startswith(":")
    else:
        valid = bind.count(":") == 1
    if not valid:
        raise ConfigurationError("LOOP_GRPC_BIND must be a host and port")
    port_text = bind.rsplit(":", maxsplit=1)[1]
    try:
        port = int(port_text)
    except ValueError as error:
        raise ConfigurationError("LOOP_GRPC_BIND port must be numeric") from error
    if not 1 <= port <= 65_535:
        raise ConfigurationError("LOOP_GRPC_BIND port must be between 1 and 65535")
    return bind
