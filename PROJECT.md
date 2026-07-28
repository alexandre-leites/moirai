# Moirai Platform

A self-hosted control plane for durable, autonomous software-engineering workflows.

**Workflow engine:** LangGraph  
**Database:** PostgreSQL  
**Stack:** Go (API, Runner) / Python (Orchestrator) / TypeScript (Web UI)

---

## Project idea

Moirai manages several software projects, continuously discovers eligible issues, ranks those issues globally, and assigns the highest-priority available issue to the next compatible runner. Each runner executes one job at a time. The scheduler permits only one active workflow per project, preventing two agents from modifying the same repository concurrently.

Each issue is processed through a durable LangGraph workflow: planning, implementation, deterministic project validation, independent AI review, pull-request creation, GitHub checks, repair loops, optional human approval, automatic merge, and issue completion.

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
- **Project concurrency.** One active workflow per project. Project stays locked during all phases (implementation, review, repair, waiting for checks, waiting for approval).
- **Runner fleet.** Multiple runner containers, one job per runner at a time, outbound gRPC connections, capability/label advertisement, heartbeats, lease renewals, safe reconnection, drain/revoke support.
- **Durable issue workflow.** LangGraph with PostgreSQL checkpoint persistence, resume after orchestrator restart, bounded repair loops, human interrupts, workflow history preserved.
- **Portable integrations.** Generic issue-tracker interface, code-host interface, and agent-backend interface. GitHub CLI adapters for the MVP. OpenCode backend first, with local-process and Docker execution modes.
- **Complete delivery flow.** Branch or worktree preparation, agent planning, implementation, deterministic local pipeline, independent AI review, repair cycles, push, PR creation, GitHub check monitoring, optional human approval, automatic merge, issue completion.
- **Web administration.** Local login, project registration/configuration, runner tokens and status, global queue, workflow dashboard with phase and attempt tracking, logs and events, retry/resume/cancel/block/approve controls.

### Design principles

| Principle | Description |
|---|---|
| Orchestrator is authoritative | Runner memory, agent conversation state, and process output are not authoritative |
| Agents do not decide completion | Completing requires deterministic gates: repo changes, pipeline, AI review, checks, human approval, merge |
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
- LangGraph per-issue workflow: prepare, plan, implement, local pipeline, AI review, repair, push, PR, GitHub checks, human approval, merge, issue completion.
- GitHub issue-tracker adapter (via `gh` CLI).
- GitHub code-host adapter (via `gh` CLI).
- OpenCode agent backend.
- Local-process and Docker execution modes.
- Web UI: login, project configuration, runner status, queue, workflow timeline, logs, approval, retry/cancel/block controls.
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
- Arbitrary user-designed workflows or visual LangGraph editor.
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
        ┌─────── Orchestrator (Python + LangGraph) ───────┐
        │               │                                 │
        │          PostgreSQL                      GitHub CLI adapters
        │               │                                 │
        └── bidirectional gRPC ◄──► Runners (Go, capacity 1 each) ──► OpenCode
```

### Service boundaries

**Orchestrator (Python + LangGraph).** The control plane. Owns all durable state: PostgreSQL, LangGraph checkpoints, scheduler, runner registry, project locks, leases, and workflow state. Exposes internal gRPC services for API requests and runner control. Runs issue-tracker and code-host adapters.

**Public API (Go).** Exposes `/api/v1` REST endpoints. Authenticates users. Translates REST calls into internal gRPC calls. Forwards live events to the Web UI over SSE. Has no database access.

**Runner (Go).** Registers with a one-time token, then authenticates with a persistent credential. Opens an outbound bidirectional gRPC stream. Advertises capabilities and labels. Accepts at most one job. Executes agent operations locally or in Docker. Streams logs. Reconnects with the same identity after disconnection.

**Web UI (React + TypeScript).** Static build served by Nginx. Communicates with the API over REST and SSE. Stores no permanent secrets.

### Network topology

Three isolated Docker networks: `database` (postgres + orchestrator, internal), `control` (orchestrator + api + runner, internal), `public` (api + web, external on ports 8080 and 3000).

### Data model

PostgreSQL is the single system of record. Tables cover: users and sessions, projects with JSON configuration, issue snapshots with priority and eligibility, workflow runs with phase tracking and retry counters, jobs and leases with generation fencing, runner identities and credentials, pipeline runs, AI reviews, pull requests, human approvals, and audit events. LangGraph uses a separate PostgreSQL schema for checkpoint storage.
