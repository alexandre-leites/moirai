"""Which stored credential backs which task-packet environment reference.

A task packet declares what an execution needs as ``EnvironmentRef{name,
secret_ref}``. Before per-project credentials the name was resolved from
whatever the runner host happened to have under it, and ``secret_ref`` was
documented as carried for audit only. This table is what gives it a backend:
a name listed here can be answered by the control plane from the project's own
credential, and a name that is not stays the runner's own business.

Kept in the domain layer because both the persistence resolver and the
in-memory control plane used by tests have to agree on it.
"""

from __future__ import annotations

# EnvironmentRef name -> app.project_credentials.kind
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

# The kinds a project may store. Kept here so the database CHECK, the gRPC
# validation and this table cannot drift apart.
VALID_CREDENTIAL_KINDS = tuple(CREDENTIAL_DELIVERY)
