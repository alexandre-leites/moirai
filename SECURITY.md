# Security Policy

## Reporting a vulnerability

Please **do not** open a public GitHub issue for security vulnerabilities.

Instead, report it privately using
[GitHub Security Advisories](https://github.com/alexandre-leites/moirai/security/advisories/new)
for this repository (*Security → Advisories → Report a vulnerability*). This
opens a private discussion with the maintainers and lets us coordinate a fix
and disclosure timeline before any details become public.

Please include as much of the following as you can:

- A description of the vulnerability and its potential impact.
- Steps to reproduce, or a proof-of-concept.
- The affected component (`orchestrator`, `api`, `runner`, `web`) and version
  or commit.
- Any known mitigation.

We will acknowledge new reports as soon as we can, investigate, and follow up
with next steps. Please give us a reasonable amount of time to address the
issue before any public disclosure.

## Scope

Moirai is a self-hosted control plane that holds credentials with real-world
reach — GitHub tokens, per-project secrets, and (when configured) coding
agent provider keys — and drives autonomous code changes and pull requests
against connected repositories. Vulnerabilities of particular interest
include (non-exhaustively):

- Authentication or session handling in the Web UI or API.
- Leakage of stored or in-flight credentials (GitHub tokens, project
  credentials, agent provider keys) — including via logs, which the runner
  redacts on a best-effort basis.
- Privilege or tenant-isolation issues between projects.
- Runner registration, lease fencing, or gRPC stream authentication bypass.
- Injection or command-execution issues in the orchestrator's GitHub CLI
  adapters, or in the runner's local-process/Docker execution paths.
- Anything allowing an attacker to trigger unintended repository writes
  (branches, commits, pull requests, merges) outside the intended workflow.

## Supported versions

Moirai does not yet have a long-term-support policy; it is pre-1.0 and
evolving quickly. Security fixes are applied to the `main` branch and
released as the next version. See [`docs/release.md`](docs/release.md) for
the release process and [`README.md`](README.md#published-images) for
currently published image tags.

| Version | Supported |
| --- | --- |
| Latest release (`latest` tag) | Yes |
| Older releases | Best-effort; please upgrade |

## Disclosure

Once a fix is available, we will publish a GitHub Security Advisory and a
new release. Credit is given to reporters who wish to be named, unless
anonymity is requested.
