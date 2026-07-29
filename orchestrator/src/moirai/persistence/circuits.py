"""Half-open circuit probe resolution, shared by every writer that can end a probe.

A circuit in `half_open` is excluded from scheduling until its single probe
workflow resolves it, so exactly one thing must never happen: the circuit stays
`half_open` while its probe can no longer report anything. Several places have
to be able to end a probe -- `AsyncpgWorkflowPersistence` (both `transition` and
the terminal short-circuit in `load_state`), the control plane's raw-SQL offer
release paths, and the scheduler's reaper -- and issue #92 was largely a
collection of ways they disagreed about it. They share the SQL here so they
cannot drift again.

Two rules hold everywhere in this module:

- A probe is only resolved while the circuit is still `half_open` and still
  points at that workflow. Matching on the pointer alone let a stale pointer
  reopen a circuit that had been legitimately closed.
- "The probe did not deliver a verdict" reopens the circuit with a *fresh*
  `opened_at` and clears the pointer, which restarts the cooldown and makes the
  circuit claimable again. It is deliberately not counted as a new failure: no
  evidence about the project or provider was produced.
"""

from __future__ import annotations

from datetime import datetime, timedelta
from typing import Any

# Mirrors app.workflow_runs' terminal statuses (see workflows/persistence.py).
# A probe in any of these can never resolve its circuit by itself again.
_TERMINAL_STATUSES = "('completed', 'blocked', 'failed', 'cancelled')"

_REOPEN_PROBE = """
UPDATE app.{table}
SET state = 'open', opened_at = $2, probe_workflow_run_id = NULL, updated_at = $2
WHERE probe_workflow_run_id = $1 AND state = 'half_open'
"""

_REAP_ORPHANED_PROBE = f"""
UPDATE app.{{table}}
SET state = 'open', opened_at = $1, probe_workflow_run_id = NULL, updated_at = $1
WHERE state = 'half_open'
  AND updated_at <= $2
  AND NOT EXISTS (
      SELECT 1 FROM app.workflow_runs AS probe
      WHERE probe.id = app.{{table}}.probe_workflow_run_id
        AND probe.status NOT IN {_TERMINAL_STATUSES}
  )
"""

_TABLES = ("project_circuit_state", "provider_circuit_state")
_COUNTED_AS = {"project_circuit_state": "project_circuits", "provider_circuit_state": "provider_circuits"}


async def reopen_probe_circuits(connection: Any, workflow_run_id: Any, now: datetime) -> None:
    """Reopens every circuit whose half-open probe is this workflow run.

    Called when a probe reaches a terminal state that proves nothing -- a
    cancelled or failed run, an unanswered offer -- and when it is blocked.
    """
    for table in _TABLES:
        await connection.execute(_REOPEN_PROBE.format(table=table), workflow_run_id, now)


async def reap_orphaned_probes(
    connection: Any, now: datetime, cooldown: timedelta
) -> dict[str, int]:
    """Reopens half-open circuits no probe can resolve any more.

    Covers the probe workflow that was never inserted (a claim whose
    transaction rolled back after the pointer was written by an older release),
    the probe deleted with its project, and the probe that reached a terminal
    status without its terminal write reaching the circuit. The cooldown grace
    keeps a probe that is merely between its claim and its first write safe.

    It cannot reopen a circuit some other writer is deciding: every path that
    takes a run terminal writes `app.workflow_runs` and this circuit row in one
    transaction, so no snapshot ever shows a terminal probe beside a circuit
    that still points at it -- except after the control plane's runner-event
    path, where reopening is the correct outcome anyway.
    """
    reaped: dict[str, int] = {}
    for table in _TABLES:
        tag = await connection.execute(
            _REAP_ORPHANED_PROBE.format(table=table), now, now - cooldown
        )
        reaped[_COUNTED_AS[table]] = _affected_rows(tag)
    return reaped


def _affected_rows(command_tag: Any) -> int:
    """asyncpg returns UPDATE's command tag ("UPDATE 3") from execute()."""
    parts = str(command_tag).split()
    return int(parts[-1]) if len(parts) > 1 and parts[-1].isdigit() else 0
