# Moirai Improvement Plan

## Platform review remediation (2026-07-29) — tracked as GitHub issues #88–#108

Source: `docs/reviews/2026-07-29-platform-review.md` (findings F1–F16, competitor analysis,
autonomy roadmap L1–L5). Each item is a filed issue on `alexandre-leites/moirai` with
what/why/how/acceptance criteria. Some overlap Phase 1/3 items below (e.g. the pipeline
node, label reconciliation) — the issues are the more current, specific version.

P1 (core loop): [ ] #88 event-driven graph (root defect, do first) · [ ] #89 runner
result-doc success bug · [ ] #90 pipeline_passed inference · [ ] #91 offer expiry cancels
runs · [ ] #92 circuit wedges · [ ] #93 lost terminal events · [ ] #104 autonomy L1
goal-gate/continuation (needs #89) · [ ] #106 autonomy L3 packet context.
P2 (durability): [ ] #94 stalled-run recovery · [ ] #95 offer reservation leak ·
[ ] #96 replay idempotency · [ ] #97 forward blocked results · [ ] #98 ci_repair_attempts ·
[ ] #99 label namespace · [ ] #100 retain failed work · [ ] #105 autonomy L2 non-delivery
outcome (needs #88, #97).
P3 (hardening/autonomy): [ ] #101 non-progress fingerprints · [ ] #102 runner lifecycle
checklist · [ ] #103 bootstrap NameError · [ ] #107 autonomy L4 escalation ladder ·
[ ] #108 autonomy L5 goal verifier.

---

Based on a four-way assessment (orchestrator, runner, API/web, cross-cutting) against
PROJECT.md, 2026-07-28. CI on main is green — but green CI currently proves very little
(see Phase 2). Ordering principle: first make the product actually work end-to-end, then
make failures visible, then finish MVP features, then harden.

## Phase 0 — Ship blockers (product cannot run today)

- [ ] **Fix duplicate migration version 003.** `003_runner_capacity.sql` and
  `003_workflow_transition_outbox.sql` both parse to version 3; fresh-DB boot hits the
  `schema_version` PK and aborts. Renumber outbox → `004_`, add a duplicate-version guard
  in `_discover_migrations`, make `ADD CONSTRAINT` idempotent, replace `str(hash())`
  checksums with sha256. (`orchestrator/src/moirai/persistence/migrations.py:41,58`)
- [ ] **Seed LangGraph state from the DB.** `project_id`, `issue_id`, `branch_name`,
  `human_approval_required`, merge config are never written into graph state, so PR
  creation, check polling, merge, issue close, and human approval are all silently
  skipped. Load them in `PersistedWorkflowRuntime.run` / a `load_state` on persistence.
  (`orchestrator/src/moirai/workflows/runtime.py:62-89`, `grpc/runner_control.py:283`)
- [ ] **Release project lock on terminal workflow status.** Currently only offer
  expiry/rejection deletes lock rows — one finished workflow bricks its project forever.
  Delete the lock inside the terminal-transition transaction.
  (`orchestrator/src/moirai/workflows/persistence.py:48-51`)
- [ ] **Ship `schemas/` into the orchestrator image.** Build context is `./orchestrator`
  so `/schemas` can't be copied; `SchemaNotFoundError` escapes `accept_event` and kills
  the runner stream. Vendor schemas under `src/moirai/` with `importlib.resources`, and
  degrade a missing schema to "untrusted result" instead of an exception.
  (`workflows/schema_validation.py:7`, `orchestrator/Dockerfile:18-20`)
- [ ] **Make runner delivery actually able to commit and push.** No git committer
  identity in image or config (`delivery.go:51`, `runner/Dockerfile`), no credential
  helper for HTTPS token push (`delivery.go:78-81`). Inject `user.name`/`user.email` and
  a token credential path from config.
- [ ] **Fix web healthcheck port** (`compose.yaml:73` probes :80, nginx listens :8080)
  and **align Go builder images** (`golang:1.24-alpine` vs `go 1.25.0` in go.mod; set
  `GOTOOLCHAIN=local` so mismatches fail loudly).
- [ ] **Fix README quickstart** — it names 2 of 4 required secret files and points at an
  env var compose doesn't read (`LOOP_INITIAL_ADMIN_PASSWORD` vs `_FILE`). Following it
  verbatim produces a stack that won't start.

## Phase 1 — Correctness and safety of what exists

- [ ] **Lease fencing while disconnected.** Runner evaluates lease expiry only on the
  heartbeat path; during a long disconnect an execution can keep running and push after
  the orchestrator re-fences the job. Add an independent expiry ticker.
  (`runner/internal/dispatch/control_loop.go:232-245`)
- [ ] **Unbreak the outbox retry path.** `PersistedWorkflowRuntime.run` catches
  everything and marks runs failed; `_drain_outbox_entry` therefore always marks rows
  processed — a transient DB blip permanently fails a workflow. Distinguish transient
  (re-raise → retry) from deterministic (fail) errors.
  (`workflows/runtime.py:95-125`, `persistence/control_plane.py:1520`)
- [ ] **Pending GitHub checks must wait, not repair.** `checks_pass` returns False for
  pending, which burns CI-repair budget. Tri-state result + wait/poll self-edge with the
  30s cadence and max-age cutoff. Also: stop `push` from incrementing
  `ci_repair_attempts` (`workflows/nodes.py:96-98`), make `merge` treat already-merged
  as success (`nodes.py:148-154`).
- [ ] **Implement the real local-pipeline node.** Today `pipeline_passed` is inferred
  from the developer execution's exit code — this violates the core "deterministic gates"
  principle. Read `project_pipeline_steps`, dispatch a pipeline execution, persist
  `pipeline_runs`. (`workflows/nodes.py:82-83`, `runner_events.py:231-245`)
- [ ] **Stop crashing the runner on recoverable races.** Stale-ack / transient send
  errors currently propagate to `os.Exit(1)`; one corrupt recovery manifest also bricks
  startup. Log-and-continue; reserve fatal for auth failures.
  (`runner/internal/dispatch/control_loop.go:149-175`, `agents/recovery.go:31-33`)
- [ ] **Split 403 from 401 in the API client** so a non-admin action doesn't log the
  user out of the SPA. Add per-request gRPC deadlines while there.
  (`api/internal/orchestrator/client.go:190`, `handlers.go:212,237`, `web/src/api.ts:110`)
- [ ] **Graceful runner shutdown + drain.** SIGTERM currently races in-flight
  executions; `ControlLoop.Drain()` is dead code with no wire message. Add the Drain
  message and wait for in-flight work. (`runner/cmd/runner/main.go:239-264`,
  `proto/runner_control.proto`)

## Phase 2 — Make failure visible (testing & CI)

- [ ] **Real Postgres integration tests.** All orchestrator DB tests string-match SQL
  against hand-rolled fakes; ~700 lines of SQL are unverified (which is how the 003
  collision shipped). Add testcontainers + a CI job with a postgres service running real
  migrations + `AsyncpgControlPlane`.
- [ ] **CI builds images + compose smoke test.** No `docker build` runs in CI; add build
  of all four images and a `compose up → wait healthy → curl /ready` gate. This would
  have caught the Go-version skew, the schemas gap, and the web healthcheck.
- [ ] **Graph-level end-to-end test** that drives a workflow from runner event →
  LangGraph → PR/merge with a fake code host, asserting state actually flows (the current
  "e2e" test injects state manually, which is why Phase 0's seeding gap was invisible).
- [ ] **Web test infrastructure.** vitest + Testing Library + MSW; first targets:
  `api.ts` error mapping/CSRF, auth bootstrap/401, form validation. Wire `make test-web`
  (eslint currently never runs in CI) into ci.yml.
- [ ] CI hygiene: `-race` for api job, deduplicate the `validate` job, `permissions:`
  block, concurrency group, dependabot config, govulncheck/pip-audit/npm audit.

## Phase 3 — Finish the MVP surface (per AGENTS.md, implementation-first)

> **Console design approved (2026-07-29):** the management-UI portion of this phase is now
> fully specified in `docs/design/web-console/` (benchmark mockup + implementation spec +
> task breakdown A1–E3 with acceptance criteria). Implement from that package; the bullets
> below remain as the original context.

- [ ] **SSE end-to-end** (the spec's live-update backbone, absent at every layer):
  streaming RPC in `control_plane.proto` → orchestrator event stream → API SSE proxy
  (`internal/http/events/`) → web `EventSource` hook replacing fetch-once pages.
  `proxy_buffering off` in nginx.
- [ ] **Operational control RPCs + UI**: workflow retry/resume/cancel/block (proto →
  orchestrator → API → UI), runner enable/disable/drain/revoke, queue endpoints + queue
  page, runners page (API endpoint already exists), workflow detail route with phases,
  attempts, events/logs.
- [ ] **Human approval path**: wire `issues.human_approval_required` into workflow state
  (Phase 0 seeding is the prerequisite) so the `wait_for_human` interrupt is reachable;
  surface approval in the workflow detail UI.
- [ ] **Progress detection & failure fingerprints (§27)**: diff hash, fingerprint
  persistence (columns already exist, never written), bump `last_progress_at` only on
  real progress, 4-step non-progress ladder.
- [ ] **Circuit breakers (§28.2/28.3)**: provider + project breaker state consulted by
  `schedule()`.
- [ ] **Richer task packet & prompts**: acceptance criteria / plan / previousFailures in
  the packet; the 12 spec prompt sections; reviewer-specific fresh-context prompt
  (§20.3 is a core product differentiator and currently unimplemented).
- [ ] **Issue-tracker/code-host surface completion**: `add_comment` with idempotency
  markers, `get_issue`, `remove_labels`; `update_pull_request`, `get_default_branch_head`
  etc.; fix unreachable blocked/delivered label reconciliation
  (`issue_sync.py:217` vs `control_plane.py:240`).
- [ ] **Docker execution mode for real**: config path for CPU/memory/network (currently
  hardcoded `--network none`, which breaks any LLM-calling agent), Docker pipeline
  runner, secrets not passed via argv.

## Phase 4 — Hardening, observability, DX

- [ ] **Structured orchestrator logging**: JSON formatter; today `extra={...}` fields are
  silently discarded and there's no timestamp/level. (`main.py:13`)
- [ ] **Metrics + request-ID propagation**: no metrics exist anywhere (spec names ~21);
  API request IDs die at the gRPC boundary. Prometheus endpoints + inject request ID into
  gRPC metadata.
- [ ] **TLS for gRPC**: runner has a complete mTLS client that nothing can use;
  orchestrator only does `add_insecure_port`, API client hardcodes insecure creds.
- [ ] **Secrets & compose hardening**: `RUNNER_REGISTRATION_TOKEN` → `_FILE` secret,
  bind API port to loopback, `cap_drop`/`no-new-privileges`/read-only rootfs, runner
  healthcheck (probe exists, unused), wire the dead `LOOP_API_COOKIE_KEY` into cookie
  signing or delete it, bind CSRF token to the orchestrator session (plumbing exists on
  the orchestrator side, unused).
- [ ] **Promote hardcoded timings to config** per §16.5 (lease 60s, renewal 15s, buffers,
  timeouts across the runner).
- [ ] **Runner event-path efficiency**: per-event full-outbox fsync + synchronous sends
  block agent stdout; batch persistence, async sends, surface drop counts.
- [ ] **Docs**: per-service READMEs (69 lines total for 13.6k LOC), runner env var
  reference (3 of ~25 documented), OpenAPI spec for the public API, `make help` +
  aggregate `make test`, remove dead code (`domain/contracts.py`, unused
  `verify_gh_ready`, dead `github_token` config).
- [ ] **Web polish pass**: visible error/loading states (many silent `.catch()`s),
  `res.ok` checks, a11y (focus styles, aria-live, document head), form validation,
  404 route.

## Review

_To be filled in as phases complete._
