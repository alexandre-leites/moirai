# Lessons

Patterns worth not repeating, written after the fact. Each one is here because
something shipped broken, not because it seemed like a good idea in the abstract.

## A green CI run is not evidence that a feature works

**2026-08-01, v0.2.0.** Two defects shipped in a release whose CI was entirely
green, and both made the product unusable:

- The orchestrator was attached only to `internal: true` networks, so it had no
  route to `api.github.com`. Issue sync, pull requests, check polling and merging
  all failed. Every automated check passed, because nothing in the suite asks a
  container to reach the internet.
- `syncNow` read the request body with `io.ReadAll` and then decoded from
  `r.Body`, which the read had drained. Every request carrying a body was
  rejected with `EOF`. The console always sends one, so the button never worked.
  These handlers had no tests at all.

**Why:** CI proved the code compiled, the unit tests passed, and the stack came
up healthy. None of those is the same as "an operator can do the thing". A
container reports healthy while being unable to reach anything it needs.

**How to apply:** before calling a feature done, exercise the path an operator
would, against a running stack, from outside. For anything that talks to a third
party, that means actually reaching it — `docker compose exec <svc>` and make the
call. For any HTTP endpoint the console calls, send the request the console
actually sends, byte for byte, rather than a request that seems equivalent.

## When a handler reads a body, check who else reads it

**2026-08-01.** `io.ReadAll(r.Body)` followed by `decodeJSON(r, …)` is two reads
of a one-shot stream. The second gets `EOF`, and the failure looks like a
malformed request from the client rather than a bug in the server.

**Why:** the error message points away from the cause. `"Invalid request body:
EOF"` reads as "the caller sent nothing", which sends you to inspect the caller.

**How to apply:** read the body once, into memory, and decode from those bytes.
While there, check the two adjacent mistakes that were in the same nine lines: a
read error silently treated as "no body" (which made a request that asked for one
project act on all of them), and a decoder that accepts unknown fields (so a
typo in a field name silently changes what the request means).

## A deleted Makefile target fails silently

**2026-08-01.** A Compose restructure deleted `test-release-tags` along with the
proto targets. The proto ones were restored; that one was not. `make
test-release-tags` then printed "Nothing to be done" and exited 0, so `make
validate` stopped checking the release trigger to image tag mapping without ever
failing.

**Why:** make treats a target declared in `.PHONY` with no recipe as satisfied.
Deleting a recipe is not a build error, and the success output is indistinguishable
from real work.

**How to apply:** when a refactor moves or rewrites part of a Makefile, diff the
target list before and after, not just the hunks shown. "Nothing to be done for
X" in output that should have run something is a failure, and worth reading as
one.

The same shape shows up in the Postgres integration suite: a test class that
leaves a workflow run in a non-terminal status fails a *different* class, because
`find_stalled_workflow_runs` scans the whole shared database. A check that passes
while testing nothing, and a check that fails for something it did not do, are
the same problem seen from two sides.

## Input a human supplies must not live where the system may delete

**2026-08-01, v0.2.2.** `agent:ready` is how an operator says "work on this
issue". It sits inside the `agent:` prefix, which label reconciliation is
allowed to delete anything from. A run that ended in a status the mapping did
not name yielded "no desired labels", so reconciliation removed the whole
namespace and took the operator's request with it. Re-applying by hand only had
it removed again on the next pass.

The same file shows the correct instinct applied elsewhere: `agent-priority:N`
is deliberately *outside* the managed prefix, with a comment saying it is user
input and must survive every sync. The rule was known and applied to one label
and not the other.

**Why:** a namespace is a permission. Putting a human's input inside one the
system may clear means the system will eventually clear it, and the operator
sees their action silently undone rather than an error.

**How to apply:** when a prefix or namespace grants blanket delete authority,
enumerate what lives in it and ask of each item "who writes this?". Anything a
human writes needs to be outside it, or explicitly protected.

## One value, one canonical form, enforced at the boundary

**2026-08-01, v0.2.2.** GitHub reports issue state as `"OPEN"`. It was stored
verbatim; six separate queries compared against `'open'`. Every read silently
matched nothing, so a project whose issues had just synchronised reported `0/0`
issues and an empty queue — while the scheduler, which did not filter on state
at all, could still act on those same rows.

**Why:** the failure is silent and distributed. No query errors; they just
return nothing, and the bug presents as "the feature does nothing" in a place
far from the write.

**How to apply:** normalise at the single point of entry, not at each
comparison — six readers is six chances to miss one. Then make the invariant the
database's job (`CHECK (state = lower(state))`), so the next caller that forgets
gets an error instead of a project whose issues quietly stop being scheduled.
Where a value crosses a system boundary, check what the *other side* actually
sends rather than what the field name suggests.

