#!/usr/bin/env bash
# loop-slots.sh — durable, crash-safe slot accounting for the autonomous issue-worker loop.
#
# The loop must never spawn more than MAX_AGENTS concurrent issue agents, even when a
# scheduled fire overlaps a previous pass that is still working. This script is the single
# source of truth for "how many agents are in flight" and for atomically reserving slots
# before anything is claimed on GitHub.
#
# DURABLE STATE
#   $LOOP_DIR/inflight/issue-<N>.lease   one flat key=value file per in-flight issue
#   $LOOP_DIR/run.lock                   flock target serialising the reserve section
#
# WHY IT IS CRASH-SAFE
#   1. flock(2) is released by the kernel when the holding process exits, including on
#      SIGKILL. The critical section can never deadlock.
#   2. A lease is a hint, never the authority. `reap` releases any lease whose worktree has
#      vanished or whose worktree has seen no file writes for REAP_AFTER_MIN minutes. A
#      killed agent writes nothing, so its lease ages out. Maximum leak is REAP_AFTER_MIN.
#   3. Leases are keyed by issue number, so re-running or resuming rewrites the same path.
#      Double counting is structurally impossible.
#   4. A lease younger than STARTUP_GRACE_MIN is protected from missing-worktree reaping,
#      covering the legitimate window between reserving a slot and creating the worktree.
#   5. Leases are written atomically (temp file + rename), so a crash mid-write cannot leave
#      a torn record. A lease that is corrupt anyway is treated as dead, never as immortal.
#
# USAGE
#   loop-slots.sh count                        -> INFLIGHT=<n> CEILING=<n> AVAILABLE=<n>
#   loop-slots.sh list                         -> table of live leases plus the counts
#   loop-slots.sh reap                         -> release stale leases, print each release
#   loop-slots.sh reserve <N> [N...]           -> reap, count, reserve up to AVAILABLE
#   loop-slots.sh bind <N> <branch> <worktree> -> record the real branch/worktree for N
#   loop-slots.sh reacquire <N> [branch] [wt]  -> re-take a slot for a resumed agent
#   loop-slots.sh release <N> [N...]           -> return slots; reports HELD vs NOT-HELD
#   loop-slots.sh touch <N> [N...]             -> heartbeat; reports NOSLOT if reaped
#
# ENVIRONMENT (all optional; defaults shown)
#   MAX_AGENTS=5           hard ceiling on simultaneous issue agents
#   REAP_AFTER_MIN=90      minutes of no worktree write activity before a lease is stale
#   STARTUP_GRACE_MIN=10   minutes a fresh lease is protected from missing-worktree reaping
#   LOOP_DIR               defaults to <main repo root>/.claude/issue-loop
#   WORKTREE_DIR           defaults to <main repo root>/.claude/worktrees
#   AGENT_ID               recorded in the lease for traceability
#
# REQUIRES GNU coreutils/findutils and util-linux flock (Linux). Not BSD/macOS compatible.

set -euo pipefail

MAX_AGENTS="${MAX_AGENTS:-5}"
REAP_AFTER_MIN="${REAP_AFTER_MIN:-90}"
STARTUP_GRACE_MIN="${STARTUP_GRACE_MIN:-10}"

# Validate the tunables up front. These are hand-edited in env.sh, and a typo would otherwise
# surface as an opaque arithmetic/unbound-variable crash in the middle of a pass.
for _v in MAX_AGENTS REAP_AFTER_MIN STARTUP_GRACE_MIN; do
  if ! [[ "${!_v}" =~ ^[0-9]+$ ]]; then
    echo "FATAL: $_v must be a non-negative integer, got '${!_v}'" >&2; exit 3
  fi
done
[ "$MAX_AGENTS" -ge 1 ] || { echo "FATAL: MAX_AGENTS must be at least 1" >&2; exit 3; }

for dep in flock find stat date git; do
  command -v "$dep" >/dev/null 2>&1 || { echo "FATAL: required tool '$dep' not found" >&2; exit 3; }
done
stat -c %Y . >/dev/null 2>&1 || { echo "FATAL: GNU stat required (stat -c)" >&2; exit 3; }

# Resolve the MAIN repository root, not the current worktree. `git rev-parse --show-toplevel`
# returns the linked worktree when called from inside one, which would silently create a
# second, empty registry and bypass the ceiling entirely. --git-common-dir always points at
# the shared .git of the main checkout.
_common="$(git rev-parse --git-common-dir 2>/dev/null)" || {
  echo "FATAL: not inside a git repository" >&2; exit 3; }
_common="$(cd "$_common" && pwd -P)"
REPO="$(dirname "$_common")"

LOOP_DIR="${LOOP_DIR:-$REPO/.claude/issue-loop}"
WORKTREE_DIR="${WORKTREE_DIR:-$REPO/.claude/worktrees}"
INFLIGHT="$LOOP_DIR/inflight"
LOCK="$LOOP_DIR/run.lock"

mkdir -p "$INFLIGHT"
[ -e "$LOCK" ] || : > "$LOCK"

now() { date +%s; }

lease_path() { printf '%s/issue-%s.lease' "$INFLIGHT" "$1"; }

die() { echo "ERROR: $*" >&2; exit 2; }

# Issue numbers are used to build paths. Validate before touching the filesystem, and
# validate the WHOLE argument list before writing anything, so a bad argument can never
# abort a partially-completed reserve.
validate_ids() {
  local n
  for n in "$@"; do
    [[ "$n" =~ ^[0-9]+$ ]] || die "invalid issue number '$n' (must be a positive integer)"
  done
}

lease_field() {
  # lease_field <file> <key> -- prints empty string if absent or unreadable
  awk -F= -v k="$2" '$1==k { sub(/^[^=]*=/, ""); print; exit }' "$1" 2>/dev/null || true
}

# Read a field that must be an integer. Anything else (corrupt, truncated, empty) yields 0,
# which makes the record look infinitely old and therefore reapable. A corrupt lease must
# never become an immortal lease.
lease_int() {
  local v; v="$(lease_field "$1" "$2")"
  [[ "$v" =~ ^[0-9]+$ ]] && printf '%s' "$v" || printf '0'
}

write_lease() {
  # write_lease <n> <branch> <worktree> -- atomic: temp file + rename
  local n="$1" branch="$2" wt="$3" lease tmp
  lease="$(lease_path "$n")"
  tmp="$(mktemp "$INFLIGHT/.tmp.XXXXXX")"
  {
    echo "issue=$n"
    echo "branch=$branch"
    echo "worktree=$wt"
    echo "started=$(now)"
    echo "agent=${AGENT_ID:--}"
  } > "$tmp"
  mv -f "$tmp" "$lease"
}

# Find the worktree for an issue. STEP 3 permits a suffixed name (issue-<N>-b) when the
# default is taken, so a missing default path must not be read as "the agent is dead".
resolve_worktree() {
  local n="$1" recorded="${2:-}" d
  if [ -n "$recorded" ] && [ -d "$recorded" ]; then printf '%s' "$recorded"; return 0; fi
  for d in "$WORKTREE_DIR/issue-$n" "$WORKTREE_DIR/issue-$n"-*; do
    [ -d "$d" ] && { printf '%s' "$d"; return 0; }
  done
  return 1
}

# Newest write activity: the lease file, or any file in the worktree excluding .git internals
# and heavy generated trees. -newermt with -quit stops at the first hit rather than walking
# the whole tree. Git's own index churn is excluded so it cannot fake liveness.
has_recent_activity() {
  local lease="$1" wt="$2" threshold="$3" lm hit
  lm="$(stat -c %Y "$lease" 2>/dev/null || echo 0)"
  [ "$lm" -ge "$threshold" ] && return 0
  [ -d "$wt" ] || return 1
  # Capture into a variable rather than piping to `grep -q`. GNU find exits non-zero on ANY
  # traversal error (e.g. one unreadable subdirectory) even when it did print a match, and
  # `set -o pipefail` would turn that into "no activity" -- reaping a demonstrably live agent.
  hit="$(find "$wt" \( -name .git -o -name node_modules -o -name .venv -o -name venv \
       -o -name __pycache__ -o -name .mypy_cache -o -name .ruff_cache \
       -o -name .pytest_cache -o -name target -o -name dist -o -name build \) -prune \
       -o -type f -newermt "@$threshold" -print -quit 2>/dev/null || true)"
  [ -n "$hit" ]
}

# Release stale leases. Prints one "REAPED <n> <reason>" line per release.
# Callers hold the flock; do_reap never takes it itself.
do_reap() {
  local t_now stale_threshold grace_threshold lease n wt started tmp
  t_now="$(now)"
  stale_threshold=$(( t_now - REAP_AFTER_MIN * 60 ))
  grace_threshold=$(( t_now - STARTUP_GRACE_MIN * 60 ))

  shopt -s nullglob
  # Sweep orphaned write_lease temp files (a crash between mktemp and mv). They are never
  # counted, but without this they accumulate forever.
  for tmp in "$INFLIGHT"/.tmp.*; do
    { [ -f "$tmp" ] && [ "$(stat -c %Y "$tmp" 2>/dev/null || echo 0)" -lt "$grace_threshold" ] \
      && rm -f "$tmp"; } || true
  done

  for lease in "$INFLIGHT"/issue-*.lease; do
    n="$(basename "$lease" .lease)"; n="${n#issue-}"
    [[ "$n" =~ ^[0-9]+$ ]] || { rm -f "$lease"; echo "REAPED $n malformed-lease-name"; continue; }

    started="$(lease_int "$lease" started)"
    wt="$(resolve_worktree "$n" "$(lease_field "$lease" worktree)" || true)"

    if [ -z "$wt" ]; then
      # No worktree anywhere. Reap once past the startup grace window. A corrupt `started`
      # reads as 0, so a corrupt lease with no worktree is reaped immediately rather than
      # consuming a slot forever.
      if [ "$started" -lt "$grace_threshold" ]; then
        rm -f "$lease"; echo "REAPED $n worktree-missing"
      fi
      continue
    fi

    # Worktree exists but nothing has been written for REAP_AFTER_MIN -> the agent is gone.
    # The issue deliberately keeps its working label (see FAILURE HANDLING); only the local
    # slot is returned.
    if ! has_recent_activity "$lease" "$wt" "$stale_threshold"; then
      rm -f "$lease"; echo "REAPED $n stale-no-activity-${REAP_AFTER_MIN}m"
    fi
  done
  shopt -u nullglob
}

count_inflight() {
  shopt -s nullglob
  local leases=( "$INFLIGHT"/issue-*.lease )
  shopt -u nullglob
  echo "${#leases[@]}"
}

emit_counts() {
  local n avail
  n="$(count_inflight)"
  avail=$(( MAX_AGENTS - n ))
  [ "$avail" -lt 0 ] && avail=0
  echo "INFLIGHT=$n CEILING=$MAX_AGENTS AVAILABLE=$avail"
}

take_lock() {
  exec 9>"$LOCK"
  flock -w 30 9 || die "could not acquire $LOCK within 30s"
}

cmd="${1:-count}"; shift || true

case "$cmd" in
  count)
    take_lock; emit_counts
    ;;

  list)
    shopt -s nullglob
    printf '%-8s %-24s %-8s %-10s %s\n' ISSUE BRANCH AGE ALIVE AGENT
    for lease in "$INFLIGHT"/issue-*.lease; do
      n="$(basename "$lease" .lease)"; n="${n#issue-}"
      started="$(lease_int "$lease" started)"
      if [ "$started" -gt 0 ]; then age="$(( ( $(now) - started ) / 60 ))m"; else age='?'; fi
      wt="$(resolve_worktree "$n" "$(lease_field "$lease" worktree)" || true)"
      if [ -z "$wt" ]; then alive='NO-WT'
      elif has_recent_activity "$lease" "$wt" "$(( $(now) - REAP_AFTER_MIN * 60 ))"; then alive='active'
      else alive='quiet'; fi
      printf '%-8s %-24s %-8s %-10s %s\n' \
        "$n" "$(lease_field "$lease" branch)" "$age" "$alive" "$(lease_field "$lease" agent)"
    done
    shopt -u nullglob
    emit_counts
    ;;

  reap)
    take_lock; do_reap; emit_counts
    ;;

  # Atomically: reap stale leases, recount, then reserve as many candidates as fit under the
  # ceiling. One process, one flock, so two overlapping fires can never both believe the same
  # slot is free.
  reserve)
    [ "$#" -gt 0 ] || die "reserve needs at least one issue number"
    validate_ids "$@"
    take_lock
    do_reap
    before="$(count_inflight)"
    avail=$(( MAX_AGENTS - before ))
    [ "$avail" -lt 0 ] && avail=0

    reserved=(); held=(); skipped=()
    for n in "$@"; do
      if [ -e "$(lease_path "$n")" ]; then held+=("$n"); continue; fi
      if [ "${#reserved[@]}" -ge "$avail" ]; then skipped+=("$n"); continue; fi
      write_lease "$n" "issue-$n" "$WORKTREE_DIR/issue-$n"
      reserved+=("$n")
    done

    echo "INFLIGHT_BEFORE=$before CEILING=$MAX_AGENTS AVAILABLE=$avail"
    echo "RESERVED=${reserved[*]:-}"
    echo "ALREADY_HELD=${held[*]:-}"
    echo "SKIPPED_NO_CAPACITY=${skipped[*]:-}"
    echo "INFLIGHT_AFTER=$(count_inflight)"
    ;;

  # Record the real branch and worktree once STEP 3 has created them. Required whenever a
  # suffixed name (issue-<N>-b) was used, otherwise the reaper looks for the wrong path.
  bind)
    [ "$#" -ge 3 ] || die "usage: bind <N> <branch> <worktree>"
    validate_ids "$1"
    # Take the lock BEFORE testing for the lease. Testing first is a TOCTOU: an overlapping
    # reserve->reap holding the lock can delete the lease in the window, and bind would then
    # recreate it as an extra record, pushing INFLIGHT past the ceiling.
    take_lock
    lease="$(lease_path "$1")"
    [ -e "$lease" ] || die "no lease for issue $1; reserve or reacquire it first"
    write_lease "$1" "$2" "$3"
    echo "BOUND $1 branch=$2 worktree=$3"
    ;;

  # Re-take a slot for an agent resumed after an infrastructure kill. Counts against the
  # ceiling like any other lease, so a resumed agent can never run uncounted.
  reacquire)
    [ "$#" -ge 1 ] || die "usage: reacquire <N> [branch] [worktree]"
    validate_ids "$1"
    take_lock
    do_reap
    n="$1"; branch="${2:-issue-$n}"; wt="${3:-$WORKTREE_DIR/issue-$n}"
    if [ -e "$(lease_path "$n")" ]; then
      write_lease "$n" "$branch" "$wt"; echo "REACQUIRED $n (lease refreshed)"
    elif [ "$(count_inflight)" -lt "$MAX_AGENTS" ]; then
      write_lease "$n" "$branch" "$wt"; echo "REACQUIRED $n (new lease)"
    else
      echo "DENIED $n at-capacity"; emit_counts; exit 1
    fi
    emit_counts
    ;;

  release)
    [ "$#" -gt 0 ] || die "release needs at least one issue number"
    validate_ids "$@"
    take_lock
    for n in "$@"; do
      lease="$(lease_path "$n")"
      if [ -e "$lease" ]; then rm -f "$lease"; echo "RELEASED $n"
      else echo "NOT-HELD $n (already reaped or never reserved)"; fi
    done
    emit_counts
    ;;

  touch)
    [ "$#" -gt 0 ] || die "touch needs at least one issue number"
    validate_ids "$@"
    rc=0
    for n in "$@"; do
      lease="$(lease_path "$n")"
      if [ -e "$lease" ]; then touch "$lease"; echo "HEARTBEAT $n"
      else echo "NOSLOT $n (lease was reaped; call reacquire before continuing)"; rc=1; fi
    done
    exit "$rc"
    ;;

  *)
    echo "usage: loop-slots.sh {count|list|reap|reserve <N...>|bind <N> <branch> <wt>|reacquire <N>|release <N...>|touch <N...>}" >&2
    exit 2
    ;;
esac
