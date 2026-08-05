# Security Policy

Moirai is a self-hosted control plane that holds GitHub credentials and executes autonomous software-engineering workflows. Security matters more than is typical for an OSS project of this size.

## Supported versions

Security fixes are applied to `main`. There are no long-term support branches; if you run Moirai, track `main` or a release tag and update regularly.

## Reporting a vulnerability

**Do not open a public GitHub issue for a security vulnerability.** 

Report it privately by opening a [security advisory](https://github.com/alexandre-leites/moirai/security/advisories/new), or by contacting the maintainers directly via the GitHub profile of the repository owner.

Please include:

- The affected version or commit.
- A description of the vulnerability and its impact.
- A minimal reproduction, if possible.
- Your suggested fix, if you have one.

You will receive a response as soon as possible. Please give maintainers a reasonable window to fix and release before disclosing publicly.

## What is in scope

- Remote code execution or privilege escalation.
- Exposure of credentials or secrets (GitHub tokens, session secrets, deployment secrets).
- Authentication or authorization bypass in the console, API, or orchestrator RPC surface.
- Data corruption or loss (project locks, job leases, workflow state).
- Anything that lets an unauthenticated or low-privileged party do what a higher-privileged one can.

## Security design notes

A few properties of the codebase relevant to a review (see `AGENTS.md` and `docs/architecture.md` for more):

- Production credentials never reach agent processes; secrets are redacted from logs.
- Job events are fenced on lease generation, job status, and a monotonic sequence number, so stale or replayed runner events are rejected.
- Workflow state persists outside agent sessions; the orchestrator is authoritative.
- The console and API enforce authentication and per-request authorization (see `api/internal/auth`).
