# Web console design package

Approved 2026-07-29. Three artifacts, in reading order:

1. [`mockup.html`](mockup.html) — the **design benchmark**. A self-contained static mockup
   (open directly in a browser; no build step, no network). It is authoritative for visual
   design: tokens, layout, components, the phase-thread signature element, and copy tone.
   Every field it displays maps to an existing column in the orchestrator's Postgres schema or
   to a control called for by `docs/reviews/2026-07-29-platform-review.md`.
2. [`specification.md`](specification.md) — the **implementation contract**: design system,
   information architecture, view-by-view behavior and data mappings, and the full API surface
   (existing endpoints to widen, new read/control endpoints, SSE plan). Where mockup and spec
   disagree, the spec wins.
3. [`tasks.md`](tasks.md) — the **task breakdown**: phases A–E with per-task scope,
   dependencies, and acceptance criteria, written so each task can be implemented independently
   (including by an autonomous agent).

Supersedes the ad-hoc scope of the current `web/src/` pages; see `tasks/todo.md` Phase 3, which
this package makes concrete.
