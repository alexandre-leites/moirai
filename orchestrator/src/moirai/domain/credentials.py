"""Which stored credential backs which task-packet environment reference.

A task packet declares what an execution needs as ``EnvironmentRef{name,
secret_ref, path}``. Before per-project credentials the name was resolved from
whatever the runner host happened to have under it, and ``secret_ref`` was
documented as carried for audit only. This table is what gives it a backend:
a name listed here can be answered by the control plane from the project's own
credential, and a name that is not stays the runner's own business.

Two families of credential live here:

* The two *git* kinds, whose environment names are fixed because the code that
  consumes them -- ``git`` and ``ssh`` -- is fixed too.
* *Agent* credentials, stored under ``agent:<NAME>``, where ``<NAME>`` is any
  environment variable an agent harness reads: ``OPENROUTER_API_KEY``,
  ``ANTHROPIC_API_KEY``, a subscription credentials file. The name is chosen by
  whoever configures the deployment or the project, so nothing here can
  enumerate them; what this module does instead is say which names are *legal*
  and which are reserved.

Kept in the domain layer because both the persistence resolver and the
in-memory control plane used by tests have to agree on it.
"""

from __future__ import annotations

import re

# EnvironmentRef name -> app.project_credentials.kind, for the fixed git kinds.
CREDENTIAL_KIND_BY_ENVIRONMENT_NAME = {
    "GITHUB_TOKEN": "github_token",
    "GIT_SSH_KEY": "ssh_private_key",
}

# How a resolved value has to reach the tools that use it. A token is an
# environment variable that git and the agent read directly. A private key is
# useless as one -- ssh takes a path -- so the runner writes it to a private
# file and passes that path instead.
CREDENTIAL_DELIVERY = {
    "github_token": "environment",
    "ssh_private_key": "file",
}

# The fixed kinds. Kept here so the database CHECK, the gRPC validation and this
# table cannot drift apart.
VALID_CREDENTIAL_KINDS = tuple(CREDENTIAL_DELIVERY)

# Every agent credential kind is this prefix followed by the environment name it
# is delivered under. One namespace, so a stored kind is self-describing and the
# database CHECK is a single regular expression.
AGENT_KIND_PREFIX = "agent:"

ENVIRONMENT_NAME = re.compile(r"^[A-Z_][A-Z0-9_]{0,127}$")
AGENT_CREDENTIAL_KIND = re.compile(r"^agent:[A-Z_][A-Z0-9_]{0,127}$")

# Names an agent credential may not claim.
#
# HOME, PATH and TMPDIR are what the runner's minimal environment is built from,
# so a credential under one of those names would not deliver a secret -- it
# would repoint the agent's home directory or its search path. The loader
# variables execute attacker-chosen code in every child process. The two git
# names already have kinds of their own, and allowing a second row for the same
# environment name would make "which one wins" a question with no answer.
RESERVED_AGENT_ENVIRONMENT_NAMES = frozenset(
    {
        "HOME",
        "PATH",
        "TMPDIR",
        "LD_PRELOAD",
        "LD_LIBRARY_PATH",
        "DYLD_INSERT_LIBRARIES",
        *CREDENTIAL_KIND_BY_ENVIRONMENT_NAME,
    }
)

# A file-delivered credential lands at this path *below the execution HOME*, so
# the bound is on a relative path and not on a filesystem path in general.
MAX_CREDENTIAL_FILE_PATH_LENGTH = 256


def is_agent_credential_kind(kind: str) -> bool:
    return kind.startswith(AGENT_KIND_PREFIX)


def agent_credential_kind(name: str) -> str:
    """The stored kind for an agent environment name, validated."""
    validate_agent_environment_name(name)
    return f"{AGENT_KIND_PREFIX}{name}"


def agent_environment_name(kind: str) -> str:
    """The environment name an ``agent:`` kind is delivered under."""
    if not AGENT_CREDENTIAL_KIND.fullmatch(kind):
        raise ValueError(f"unknown credential kind: {kind}")
    return kind[len(AGENT_KIND_PREFIX) :]


def validate_agent_environment_name(name: str) -> None:
    if not ENVIRONMENT_NAME.fullmatch(name):
        raise ValueError(
            "an agent credential name must be an environment variable name "
            "(upper case, digits and underscores, e.g. OPENROUTER_API_KEY)"
        )
    if name in RESERVED_AGENT_ENVIRONMENT_NAMES:
        raise ValueError(f"{name} is reserved and cannot hold an agent credential")


def validate_credential_kind(kind: str) -> None:
    """Raises unless `kind` is one this deployment knows how to store."""
    if kind in VALID_CREDENTIAL_KINDS:
        return
    if not is_agent_credential_kind(kind):
        raise ValueError(f"unknown credential kind: {kind}")
    if not AGENT_CREDENTIAL_KIND.fullmatch(kind):
        raise ValueError(f"unknown credential kind: {kind}")
    validate_agent_environment_name(agent_environment_name(kind))


def credential_kind_for_environment(name: str) -> str | None:
    """The kind that could back an environment reference, or None.

    None means "no stored credential can answer this name", which is the runner
    being told to look in its own environment instead. A *legal* agent name
    always maps to a kind whether or not any project has stored one -- absence
    is answered later, by the lookup, not by this table.
    """
    fixed = CREDENTIAL_KIND_BY_ENVIRONMENT_NAME.get(name)
    if fixed is not None:
        return fixed
    if not ENVIRONMENT_NAME.fullmatch(name) or name in RESERVED_AGENT_ENVIRONMENT_NAMES:
        return None
    return f"{AGENT_KIND_PREFIX}{name}"


def normalize_credential_file_path(kind: str, file_path: str) -> str:
    """Validates a file destination, which is relative to the execution HOME.

    An agent credential with a path is written into the throwaway home the
    runner builds for the execution, because that is the only ``~`` the agent
    process ever sees (``MinimalEnvironment`` overrides HOME). Absolute paths
    and ``..`` are refused: the destination has to stay inside a directory the
    runner owns and discards, or the "throwaway" part stops being true.
    """
    path = file_path.strip()
    if not path:
        return ""
    if not is_agent_credential_kind(kind):
        raise ValueError("only an agent credential can be delivered as a file")
    if len(path) > MAX_CREDENTIAL_FILE_PATH_LENGTH:
        raise ValueError("credential file path is too long")
    if path.startswith(("/", "~")) or "\\" in path or "\x00" in path:
        raise ValueError("credential file path must be relative to the agent home directory")
    segments = path.split("/")
    if any(segment in ("", ".", "..") for segment in segments):
        raise ValueError("credential file path must be relative to the agent home directory")
    if any(character.isspace() for character in path):
        raise ValueError("credential file path must not contain whitespace")
    return path


def parse_agent_credential_refs(value: str) -> tuple[tuple[str, str], ...]:
    """Parses ``LOOP_AGENT_CREDENTIAL_REFS`` into (name, file path) pairs.

    ``OPENROUTER_API_KEY,OPENCODE_AUTH=.local/share/opencode/auth.json``

    A bare name is delivered as an environment variable; ``NAME=path`` is
    written to that path below the agent's home directory and the variable
    carries the path instead. These are *declarations*, never values: what the
    deployment holds under each name stays in the runner's own environment.
    """
    if not value.strip():
        return ()
    refs: list[tuple[str, str]] = []
    seen: set[str] = set()
    for entry in value.split(","):
        entry = entry.strip()
        if not entry:
            continue
        name, separator, path = entry.partition("=")
        name = name.strip()
        validate_agent_environment_name(name)
        if name in seen:
            raise ValueError(f"{name} is declared more than once")
        seen.add(name)
        refs.append(
            (name, normalize_credential_file_path(agent_credential_kind(name), path) if separator else "")
        )
    return tuple(refs)
