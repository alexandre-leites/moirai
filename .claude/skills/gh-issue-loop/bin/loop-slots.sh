#!/usr/bin/env bash
#
# Durable, crash-safe slot registry for the gh-issue-loop skill (SKILL.md §5).
#
# One lease file per in-flight issue under $LOOP_DIR/inflight/. Reserving is
# serialised by an flock on $LOOP_DIR/run.lock, so two overlapping fires can
# never both conclude the same slot is free. A lease is a hint and never the
# authority on liveness: anything whose worktree vanished or went quiet ages
# out, so the maximum slot leak is REAP_AFTER_MIN rather than forever.
#
# This replaces a stub that answered every query with hardcoded zeros. The stub
# was worse than an absent helper: the skill's `[ -x ... ]` guard passed, the
# caller claimed the issue, and only then discovered its slot had not been
# recorded -- stranding the issue under WORKING_LABEL with no work done. See
# `verify`, which exists so that failure mode cannot recur silently.

set -euo pipefail

readonly DEFAULT_MAX_AGENTS=5
readonly DEFAULT_REAP_AFTER_MIN=90
readonly DEFAULT_STARTUP_GRACE_MIN=10

# Directories whose churn must not read as agent activity: git rewrites its
# index on every command, and dependency and build trees are written by tooling
# rather than by the agent. A killed agent would otherwise look alive for as
# long as anything else touched the worktree.
readonly PRUNED_DIRS=(.git node_modules .venv __pycache__ .cache caches dist build target .mypy_cache .ruff_cache .pytest_cache)

usage() {
  cat >&2 <<'EOF'
usage: loop-slots.sh <command> [arguments]

  count                          INFLIGHT=<n> CEILING=<n> AVAILABLE=<n>
  list                           table of live leases, liveness, and the counts
  reap                           release stale leases, print each release
  reserve <N> [N...]             reap, recount, reserve up to AVAILABLE candidates
  bind <N> <branch> <worktree>   record the real branch/worktree once it exists
  reacquire <N> [branch] [wt]    re-take a counted slot for a resumed agent
  release <N> [N...]             return slots; prints RELEASED or NOT-HELD
  touch <N> [N...]               heartbeat; reports NOSLOT if already reaped
  verify                         self-check that this helper records what it reports

Ceiling may be overridden per invocation: MAX_AGENTS=3 loop-slots.sh reserve 101
EOF
}

die() { printf 'ERROR: %s\n' "$*" >&2; exit 1; }

is_count() { [[ "${1:-}" =~ ^[0-9]+$ ]]; }
is_issue() { [[ "${1:-}" =~ ^[1-9][0-9]*$ ]]; }

now_epoch() { date -u +%s; }

file_mtime() {
  stat -c %Y "$1" 2>/dev/null || stat -f %m "$1" 2>/dev/null || echo 0
}

# --- environment ------------------------------------------------------------

# The main repository, not the worktree the caller happens to stand in:
# --git-common-dir points at the shared .git for every linked worktree, so a
# sub-agent calling `touch` from its own worktree reaches the one true registry
# instead of creating a second one beside it (SKILL.md §5, crash-safety note 5).
resolve_paths() {
  local common
  common="$(git rev-parse --git-common-dir 2>/dev/null)" \
    || die "not inside a git repository; the slot registry cannot be located"
  common="$(cd "$common" 2>/dev/null && pwd -P)" \
    || die "git common directory is unreadable"
  REPO="$(cd "$common/.." && pwd -P)"
  LOOP_DIR="$REPO/.claude/issue-loop"
  WORKTREE_DIR="$REPO/.claude/worktrees"
  INFLIGHT_DIR="$LOOP_DIR/inflight"
  LOCK_FILE="$LOOP_DIR/run.lock"
}

# Tunables come from $LOOP_DIR/tunables, and the environment overrides the file.
# The file is what makes the ceiling reach sub-agents at all: they call this
# helper by absolute path from their own shells, having never sourced env.sh, so
# an exported MAX_AGENTS never reaches them (SKILL.md §2).
load_tunables() {
  local file_max="$DEFAULT_MAX_AGENTS"
  local file_reap="$DEFAULT_REAP_AFTER_MIN"
  local file_grace="$DEFAULT_STARTUP_GRACE_MIN"
  local key value

  if [ -r "$LOOP_DIR/tunables" ]; then
    while IFS='=' read -r key value || [ -n "${key:-}" ]; do
      value="${value%%[[:space:]]*}"
      case "$key" in
        MAX_AGENTS)        [ -n "$value" ] && file_max="$value" ;;
        REAP_AFTER_MIN)    [ -n "$value" ] && file_reap="$value" ;;
        STARTUP_GRACE_MIN) [ -n "$value" ] && file_grace="$value" ;;
      esac
    done < "$LOOP_DIR/tunables"
  fi

  MAX_AGENTS="${MAX_AGENTS:-$file_max}"
  REAP_AFTER_MIN="${REAP_AFTER_MIN:-$file_reap}"
  STARTUP_GRACE_MIN="${STARTUP_GRACE_MIN:-$file_grace}"

  is_count "$MAX_AGENTS"        || die "MAX_AGENTS must be a non-negative integer, got '$MAX_AGENTS'"
  is_count "$REAP_AFTER_MIN"    || die "REAP_AFTER_MIN must be a non-negative integer, got '$REAP_AFTER_MIN'"
  is_count "$STARTUP_GRACE_MIN" || die "STARTUP_GRACE_MIN must be a non-negative integer, got '$STARTUP_GRACE_MIN'"
}

# --- lease files ------------------------------------------------------------

lease_path() { printf '%s/issue-%s.lease' "$INFLIGHT_DIR" "$1"; }

lease_field() {
  [ -r "$1" ] || return 1
  sed -n "s/^$2=//p" "$1" | head -1
}

# Temp file then rename, so a crash mid-write cannot leave a torn record and a
# resumed or repeated pass rewrites the same path rather than adding a second
# entry -- double counting is structurally impossible (SKILL.md §5, note 3).
write_lease() {
  local issue="$1" branch="$2" worktree="$3" started="$4" agent="$5"
  local target temp
  target="$(lease_path "$issue")"
  temp="$(mktemp "$INFLIGHT_DIR/.issue-$issue.XXXXXX")"
  printf 'issue=%s\nbranch=%s\nworktree=%s\nstarted=%s\nagent=%s\n' \
    "$issue" "$branch" "$worktree" "$started" "$agent" > "$temp"
  mv -f "$temp" "$target"
}

held_issues() {
  local path base
  for path in "$INFLIGHT_DIR"/issue-*.lease; do
    [ -e "$path" ] || continue
    base="${path##*/issue-}"
    printf '%s\n' "${base%.lease}"
  done
}

inflight_count() { held_issues | grep -c . || true; }

agent_id() { printf '%s' "${LOOP_AGENT_ID:-$$@$(hostname 2>/dev/null || echo unknown)}"; }

# --- liveness ---------------------------------------------------------------

# The recorded path first, then the conventional ones. A pass that died between
# reserving and creating the worktree has recorded no path yet, and a resumed
# agent may have moved to an `issue-<N>-b` style suffix.
find_worktree() {
  local issue="$1" recorded="${2:-}" candidate
  if [ -n "$recorded" ] && [ -d "$recorded" ]; then printf '%s' "$recorded"; return 0; fi
  if [ -d "$WORKTREE_DIR/issue-$issue" ]; then printf '%s' "$WORKTREE_DIR/issue-$issue"; return 0; fi
  for candidate in "$WORKTREE_DIR/issue-$issue"-*; do
    [ -d "$candidate" ] && { printf '%s' "$candidate"; return 0; }
  done
  return 1
}

# True when the lease file, or any non-pruned file in the worktree, was written
# at or after `threshold`. -quit stops at the first hit so this stays cheap on a
# large tree. Where find refuses the predicate the lease file's own mtime
# decides, which the `touch` heartbeat keeps honest.
has_activity_since() {
  local lease="$1" worktree="$2" threshold="$3"

  [ "$(file_mtime "$lease")" -ge "$threshold" ] && return 0
  [ -n "$worktree" ] && [ -d "$worktree" ] || return 1

  local prune=() name
  for name in "${PRUNED_DIRS[@]}"; do prune+=(-name "$name" -o); done
  unset 'prune[${#prune[@]}-1]'

  local hit
  hit="$(find "$worktree" \( "${prune[@]}" \) -prune -o \
              -type f -newermt "@$threshold" -print -quit 2>/dev/null || true)"
  [ -n "$hit" ]
}

# Rule 1: no worktree anywhere and the reservation has outlived the startup
#         grace window, so the pass died before it could create one.
# Rule 2: no write activity for REAP_AFTER_MIN. A live agent writes files
#         constantly; a killed one writes nothing.
# A lease whose `started` is corrupt or truncated reads as epoch 0, which makes
# it maximally old and so reaped on sight rather than holding a slot forever.
lease_is_stale() {
  local issue="$1" now="$2"
  local lease started worktree
  lease="$(lease_path "$issue")"
  [ -e "$lease" ] || return 1

  started="$(lease_field "$lease" started || true)"
  is_count "$started" || started=0

  worktree="$(find_worktree "$issue" "$(lease_field "$lease" worktree || true)" || true)"

  if [ -z "$worktree" ] && [ $(( now - started )) -ge $(( STARTUP_GRACE_MIN * 60 )) ]; then
    STALE_REASON="no worktree after ${STARTUP_GRACE_MIN}m grace"
    return 0
  fi
  if ! has_activity_since "$lease" "$worktree" $(( now - REAP_AFTER_MIN * 60 )); then
    STALE_REASON="no write activity for ${REAP_AFTER_MIN}m"
    return 0
  fi
  return 1
}

# Reaping returns the local slot only. Per the skill's failure handling the
# issue deliberately keeps WORKING_LABEL, so a reaped issue is not silently
# re-picked next pass -- it waits for a human.
do_reap() {
  local now issue
  now="$(now_epoch)"
  for issue in $(held_issues); do
    if lease_is_stale "$issue" "$now"; then
      rm -f "$(lease_path "$issue")"
      printf 'REAPED=%s reason=%s\n' "$issue" "$STALE_REASON"
    fi
  done
}

# --- locking ----------------------------------------------------------------

# flock(2) is released by the kernel when this process exits, including on
# SIGKILL, so the critical section can never deadlock.
take_lock() {
  exec 9>"$LOCK_FILE"
  flock 9
}

# --- commands ---------------------------------------------------------------

cmd_count() {
  local inflight available
  inflight="$(inflight_count)"
  available=$(( MAX_AGENTS - inflight ))
  [ "$available" -lt 0 ] && available=0
  printf 'INFLIGHT=%s CEILING=%s AVAILABLE=%s\n' "$inflight" "$MAX_AGENTS" "$available"
}

cmd_list() {
  local now issue lease branch worktree started age live
  now="$(now_epoch)"
  if [ -z "$(held_issues)" ]; then
    echo "No live leases."
  else
    printf '%-7s %-24s %-40s %-7s %s\n' ISSUE BRANCH WORKTREE AGE LIVE
    for issue in $(held_issues); do
      lease="$(lease_path "$issue")"
      branch="$(lease_field "$lease" branch || true)"
      worktree="$(lease_field "$lease" worktree || true)"
      started="$(lease_field "$lease" started || true)"
      is_count "$started" || started=0
      age=$(( (now - started) / 60 ))
      if lease_is_stale "$issue" "$now"; then live="stale: $STALE_REASON"; else live="live"; fi
      printf '%-7s %-24s %-40s %-7s %s\n' \
        "$issue" "${branch:--}" "${worktree:--}" "${age}m" "$live"
    done
  fi
  cmd_count
}

# reap -> count -> reserve inside one process holding one flock, so two
# overlapping fires can never both conclude the same slot is free.
cmd_reserve() {
  [ "$#" -ge 1 ] || die "reserve needs at least one issue number; reserved nothing"
  local issue
  for issue in "$@"; do
    is_issue "$issue" || die "'$issue' is not an issue number; reserved nothing"
  done

  take_lock
  do_reap >/dev/null

  local before available
  before="$(inflight_count)"
  available=$(( MAX_AGENTS - before ))
  [ "$available" -lt 0 ] && available=0
  printf 'INFLIGHT_BEFORE=%s CEILING=%s AVAILABLE=%s\n' "$before" "$MAX_AGENTS" "$available"

  local reserved=() already=() skipped=() now
  now="$(now_epoch)"
  for issue in "$@"; do
    if [ -e "$(lease_path "$issue")" ]; then
      already+=("$issue")
    elif [ "${#reserved[@]}" -lt "$available" ]; then
      write_lease "$issue" "" "" "$now" "$(agent_id)"
      reserved+=("$issue")
    else
      skipped+=("$issue")
    fi
  done

  printf 'RESERVED=%s\n' "${reserved[*]:-}"
  printf 'ALREADY_HELD=%s\n' "${already[*]:-}"
  printf 'SKIPPED_NO_CAPACITY=%s\n' "${skipped[*]:-}"
  printf 'INFLIGHT_AFTER=%s\n' "$(inflight_count)"
}

cmd_bind() {
  [ "$#" -eq 3 ] || die "bind needs <issue> <branch> <worktree>"
  is_issue "$1" || die "'$1' is not an issue number"
  take_lock
  local lease
  lease="$(lease_path "$1")"
  if [ ! -e "$lease" ]; then
    printf 'NOSLOT=%s\n' "$1"
    exit 1
  fi
  write_lease "$1" "$2" "$3" \
    "$(lease_field "$lease" started || now_epoch)" \
    "$(lease_field "$lease" agent || agent_id)"
  printf 'BOUND=%s branch=%s worktree=%s\n' "$1" "$2" "$3"
}

cmd_reacquire() {
  [ "$#" -ge 1 ] || die "reacquire needs an issue number"
  is_issue "$1" || die "'$1' is not an issue number"
  local issue="$1" branch="${2:-}" worktree="${3:-}"

  take_lock
  do_reap >/dev/null

  local lease
  lease="$(lease_path "$issue")"
  if [ -e "$lease" ]; then
    # Already counted: refresh in place rather than taking a second slot.
    write_lease "$issue" \
      "${branch:-$(lease_field "$lease" branch || true)}" \
      "${worktree:-$(lease_field "$lease" worktree || true)}" \
      "$(now_epoch)" "$(agent_id)"
    printf 'REACQUIRED=%s held=yes\n' "$issue"
    return 0
  fi

  local inflight
  inflight="$(inflight_count)"
  if [ "$inflight" -ge "$MAX_AGENTS" ]; then
    printf 'NOSLOT=%s\n' "$issue"
    cmd_count
    exit 1
  fi
  write_lease "$issue" "$branch" "$worktree" "$(now_epoch)" "$(agent_id)"
  printf 'REACQUIRED=%s held=no\n' "$issue"
}

cmd_release() {
  [ "$#" -ge 1 ] || die "release needs at least one issue number"
  take_lock
  local issue
  for issue in "$@"; do
    is_issue "$issue" || die "'$issue' is not an issue number"
    if [ -e "$(lease_path "$issue")" ]; then
      rm -f "$(lease_path "$issue")"
      printf 'RELEASED=%s\n' "$issue"
    else
      printf 'NOT-HELD=%s\n' "$issue"
    fi
  done
}

cmd_touch() {
  [ "$#" -ge 1 ] || die "touch needs at least one issue number"
  local issue status=0
  for issue in "$@"; do
    is_issue "$issue" || die "'$issue' is not an issue number"
    if [ -e "$(lease_path "$issue")" ]; then
      touch "$(lease_path "$issue")"
      printf 'TOUCHED=%s\n' "$issue"
    else
      printf 'NOSLOT=%s\n' "$issue"
      status=1
    fi
  done
  return "$status"
}

# Round-trips a reservation through the registry and checks the count actually
# moved. A helper that reports success without recording anything -- the stub
# this file replaces -- passes every `[ -x ]` existence check and then strands
# whatever the caller claimed. The loop runs this before it touches a label.
cmd_verify() {
  take_lock
  local probe=999999999 before after
  rm -f "$(lease_path "$probe")"
  before="$(inflight_count)"
  write_lease "$probe" "verify" "" "$(now_epoch)" "$(agent_id)"
  after="$(inflight_count)"
  rm -f "$(lease_path "$probe")"

  if [ "$after" -ne $(( before + 1 )) ]; then
    printf 'VERIFY=failed detail=a reservation did not change the in-flight count (%s -> %s)\n' "$before" "$after"
    exit 1
  fi
  if [ "$(inflight_count)" -ne "$before" ]; then
    printf 'VERIFY=failed detail=a release did not restore the in-flight count\n'
    exit 1
  fi
  printf 'VERIFY=ok registry=%s\n' "$INFLIGHT_DIR"
}

# --- entry point ------------------------------------------------------------

main() {
  [ "$#" -ge 1 ] || { usage; exit 1; }
  local command="$1"; shift

  case "$command" in
    -h|--help|help) usage; exit 0 ;;
  esac

  resolve_paths
  mkdir -p "$INFLIGHT_DIR"
  load_tunables

  case "$command" in
    count)     cmd_count "$@" ;;
    list)      cmd_list "$@" ;;
    reap)      take_lock; do_reap "$@" ;;
    reserve)   cmd_reserve "$@" ;;
    bind)      cmd_bind "$@" ;;
    reacquire) cmd_reacquire "$@" ;;
    release)   cmd_release "$@" ;;
    touch)     cmd_touch "$@" ;;
    verify)    cmd_verify "$@" ;;
    *)         usage; exit 1 ;;
  esac
}

main "$@"
