# Moirai Platform

A self-hosted control plane for durable, autonomous software-engineering workflows.

**Workflow engine:** Go state machine with PostgreSQL-persisted workflow runs  
**Database:** PostgreSQL  
**Stack:** Go (Orchestrator, API, Runner) / TypeScript (Web UI)

---

## Project idea

Moirai manages several software projects, continuously discovers eligible issues, ranks those issues globally, and assigns the highest-priority available issue to the next compatible runner. Each runner executes one job at a time. The scheduler permits only one active workflow per project, preventing two agents from modifying the same repository concurrently.

Each issue is processed through a durable workflow whose state is persisted in PostgreSQL: implementation, pull-request creation, GitHub checks, automatic merge, and issue completion.

The platform is designed around four boundaries:

1. **The orchestrator owns durable state and decisions.**
2. **Runners execute work but do not own workflow truth.**
3. **Issue trackers and coding agents are accessed through portable interfaces.**
4. **Deterministic checks decide whether work is complete; an agent cannot declare success by itself.**

The MVP uses GitHub Issues and OpenCode, but neither is embedded into the core domain model. GitHub is implemented behind issue-tracker and code-host interfaces. OpenCode is implemented behind a generic agent-backend interface.

---

## Problem statement

Coding agents can implement scoped tasks, but autonomous software delivery requires a reliable system around the agent. A simple fetch-implement-repeat loop does not reliably handle:

- Several projects competing for a limited runner fleet.
- Global issue prioritization.
- Project-level concurrency restrictions.
- Agent crashes or provider outages.
- Orchestrator restarts and network disconnections.
- Processes that hang indefinitely.
- Agents that repeat the same failed action or forget the original objective.
- CI failures after a pull request is opened.
- Review findings that require another implementation cycle.
- Human approval for selected work.
- Duplicate pull requests or duplicated external side effects.
- Runner ownership after a lease expires.
- Replacing one agent product or issue tracker with another.
- Visibility into active work, failures, logs, and recovery.

Moirai solves these by treating the agent as one replaceable execution component inside a deterministic, persisted workflow.

---

## Goals

### MVP goals

- **Multiple projects.** Register several projects, enable/disable independently, support managed clones and existing local paths, synchronize eligible issues from each.
- **Global scheduling.** One global queue across all enabled projects, highest numeric priority wins, creation timestamp as tie-breaker, skip projects with active workflows.
- **Project concurrency.** One active workflow per project. The project stays locked for every phase of a run — in the Go V1 orchestrator that is implementation, pull-request delivery, and waiting for checks, and it extends to review, repair, and waiting for approval when those phases are implemented.
- **Runner fleet.** Multiple runner containers, one job per runner at a time, outbound gRPC connections, capability/label advertisement, heartbeats, lease renewals, safe reconnection, drain/revoke support.
- **Durable issue workflow.** A Go state machine whose every transition is persisted to PostgreSQL, so workflows survive orchestrator restart and workflow history is preserved. It is event-driven: the orchestrator dispatches an agent execution and advances the run only on the runner's terminal event. Retries are manual — there are no automatic workflow retries or execution deadlines.
- **Portable integrations.** Generic issue-tracker interface, code-host interface, and agent-backend interface. GitHub CLI adapters for the MVP. OpenCode backend first, with local-process and Docker execution modes.
- **Complete delivery flow.** Branch or worktree preparation, an opt-in planning phase, implementation, push, PR creation, GitHub check monitoring, an opt-in human-approval gate, automatic merge, issue completion. (Deterministic local pipeline, independent AI review, and repair cycles are target scope not yet implemented in the Go V1 orchestrator -- see #250's remaining follow-ups #352/#353/#354.)
- **Web administration.** Local login, project registration/configuration, runner tokens and status, global queue, workflow dashboard with phase and attempt tracking, logs and events, retry/resume/cancel/block/approve controls.

### Design principles

| Principle | Description |
|---|---|
| Orchestrator is authoritative | Runner memory, agent conversation state, and process output are not authoritative |
| Agents do not decide completion | Completing requires deterministic gates: repo changes, pipeline, AI review, checks, human approval, merge. V1 enforces the GitHub-check gate (a run merges only on green), the runner's result-document evidence, and (for a project that opts in) a human-approval gate between green checks and merge; the pipeline and AI review gates are not implemented in V1 |
| Portability through interfaces | Provider-specific behavior is translated at adapter boundaries |
| Durable state outside conversations | An agent session can be replaced without losing the task |
| Bounded loops | Every retry loop has explicit limits (attempts, duration, executions) |
| Idempotent side effects | All external operations (labels, branches, PRs, merge, close) are safe to retry |
| Lease fencing | Job events carry a generation counter; stale generation events are rejected |

---

## In scope (MVP)

- Register and configure multiple projects with managed clone or existing-path modes.
- Numeric priority labels on issues.
- Global scheduler that selects the highest-priority eligible issue across all unlocked projects.
- Single-project concurrency lock.
- Runner registration via one-time tokens, outbound gRPC, heartbeats, lease renewals, reconnection, drain, and revocation.
- Per-issue workflow state machine: prepare, an opt-in planning phase (`planning`, projectConfig.RequirePlanning -- #351), implement, push, PR, GitHub checks, an opt-in human-approval gate (`waiting_human`, resolved by `SubmitHumanDecision`), merge, issue completion. Local pipeline, AI review, and repair cycles are specified above but are **not implemented in the Go V1 orchestrator** (see #250's remaining follow-ups #352/#353/#354); their schema columns and RPCs remain reserved.
- GitHub issue-tracker adapter (via `gh` CLI).
- GitHub code-host adapter (via `gh` CLI).
- OpenCode agent backend.
- Local-process and Docker execution modes.
- Web UI: login, project configuration (including the `requireHumanApproval` opt-in toggle), runner status, queue, workflow timeline, logs, retry/cancel/block controls, and the decision panel for the human-approval gate: `SubmitHumanDecision` resolves a `waiting_human` run to either merge (approved) or `blocked` (changes requested).
- Docker Compose deployment with full network isolation.

---

## Out of scope (MVP)

- Kubernetes deployment.
- Multi-region operation or HA orchestrator.
- Hosted multi-tenant SaaS.
- Enterprise SSO or role-based access control.
- Multi-job runners (one job per runner).
- Multiple simultaneous issues for the same project.
- Cross-repository atomic changes.
- Native Jira, Linear, GitLab integrations.
- Native Claude Code, Codex, or Aider integrations.
- Arbitrary user-designed workflows or a visual workflow editor.
- Billing, cost allocation, or production credential management.
- Object-storage log archival.

---

## High-level architecture

```
Browser / future mobile         Web UI (React + TypeScript)
        │                               │
        └────────── REST + SSE ──────────┘
                        │
               Public API (Go)
                        │
                  internal gRPC
                        │
        ┌────────────── Orchestrator (Go) ────────────────┐
        │               │                                 │
        │          PostgreSQL                      GitHub CLI adapters
        │               │                                 │
        └── bidirectional gRPC ◄──► Runners (Go, capacity 1 each) ──► OpenCode
```

### Service boundaries

**Orchestrator (Go).** The control plane. Owns all durable state: PostgreSQL, workflow state, scheduler, runner registry, project locks, and leases. Exposes internal gRPC services for API requests and runner control. Runs issue-tracker and code-host adapters.

**Public API (Go).** Exposes `/api/v1` REST endpoints. Authenticates users. Translates REST calls into internal gRPC calls. Forwards live events to the Web UI over SSE. Has no database access.

**Runner (Go).** Registers with a one-time token, then authenticates with a persistent credential. Opens an outbound bidirectional gRPC stream. Advertises capabilities and labels. Accepts at most one job. Executes agent operations locally or in Docker. Streams logs. Reconnects with the same identity after disconnection.

**Web UI (React + TypeScript).** Static build served by Nginx. Communicates with the API over REST and SSE. Stores no permanent secrets.

### Network topology

Three isolated Docker networks: `database` (postgres + orchestrator, internal), `control` (orchestrator + api + runner, internal), `public` (api + web, external on ports 8080 and 3000).

### Data model

PostgreSQL is the single system of record. Tables cover: users and sessions, projects with JSON configuration, issue snapshots with priority and eligibility, workflow runs with phase tracking and retry counters, jobs and leases with generation fencing, runner identities and credentials, pipeline runs, AI reviews, pull requests, human approvals, and audit events.
