#!/usr/bin/env bash
#
# Tests for loop-slots.sh. Each case runs against a throwaway git repository, so
# the real registry is never touched. Run: ./loop-slots.test.sh
#
# The cases that matter are the ones the stub silently got wrong: that a
# reservation is actually recorded, that capacity is enforced, and that a lease
# is reaped rather than held forever when the agent behind it died.

set -uo pipefail

SLOTS="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd -P)/loop-slots.sh"
PASS=0
FAIL=0

fail() { printf '  FAIL: %s\n' "$*"; FAIL=$((FAIL + 1)); }
pass() { PASS=$((PASS + 1)); }

expect_contains() { # haystack needle description
  case "$1" in
    *"$2"*) pass ;;
    *) fail "$3: expected to find '$2' in:"; printf '%s\n' "$1" | sed 's/^/        /' ;;
  esac
}

expect_missing() { # haystack needle description
  case "$1" in
    *"$2"*) fail "$3: did not expect '$2' in:"; printf '%s\n' "$1" | sed 's/^/        /' ;;
    *) pass ;;
  esac
}

# A fresh repository with the loop directories in place, cd'd into. It sets
# REPO_DIR rather than printing it: called in a command substitution the cd
# would happen in a subshell and the caller would silently stay in the previous
# repository, carrying its leases into the next case.
new_repo() {
  REPO_DIR="$(mktemp -d)"
  git -C "$REPO_DIR" init --quiet
  git -C "$REPO_DIR" config user.email test@example.com
  git -C "$REPO_DIR" config user.name test
  mkdir -p "$REPO_DIR/.claude/issue-loop" "$REPO_DIR/.claude/worktrees"
  cd "$REPO_DIR" || exit 1
}

# Backdate a lease's `started` field and its mtime, to simulate an agent that
# reserved a slot N minutes ago and has written nothing since.
age_lease() { # issue minutes
  local lease=".claude/issue-loop/inflight/issue-$1.lease"
  local when=$(( $(date -u +%s) - $2 * 60 ))
  sed -i.bak "s/^started=.*/started=$when/" "$lease" && rm -f "$lease.bak"
  touch -d "@$when" "$lease"
}

echo "loop-slots.sh"

# --- the self-check ---------------------------------------------------------
echo "  verify"
new_repo
out="$("$SLOTS" verify 2>&1)"
expect_contains "$out" "VERIFY=ok" "verify passes against a working registry"

# A helper that reports success without recording anything must fail verify.
# This is the exact shape of the stub that stranded issues 219 and 226.
stub="$(mktemp -d)/loop-slots.sh"
cat > "$stub" <<'STUB'
#!/bin/bash
case "$1" in
  count) echo "INFLIGHT=0 CEILING=5 AVAILABLE=5" ;;
  reserve) echo "RESERVED=$2" ;;
  verify) echo "VERIFY=ok" ;;
esac
STUB
chmod +x "$stub"
out="$("$stub" reserve 42; "$stub" count)"
expect_contains "$out" "INFLIGHT=0" "the stub still reports zero after reserving (regression fixture)"

# --- counting and capacity --------------------------------------------------
echo "  count / reserve"
new_repo
out="$("$SLOTS" count)"
expect_contains "$out" "INFLIGHT=0 CEILING=5 AVAILABLE=5" "empty registry counts zero"

out="$("$SLOTS" reserve 101 102)"
expect_contains "$out" "RESERVED=101 102" "both candidates reserved"
expect_contains "$out" "INFLIGHT_AFTER=2" "in-flight count moved"
out="$("$SLOTS" count)"
expect_contains "$out" "INFLIGHT=2 CEILING=5 AVAILABLE=3" "count reflects the reservation"

# The stub's central lie: reserving must change what a later, separate
# invocation reports, or the caller claims an issue it does not hold.
out="$("$SLOTS" reserve 103)"
expect_contains "$out" "INFLIGHT_BEFORE=2" "a later invocation sees the earlier reservation"

echo "  capacity"
new_repo
out="$(MAX_AGENTS=2 "$SLOTS" reserve 201 202 203 204)"
expect_contains "$out" "RESERVED=201 202" "reserves only up to the ceiling"
expect_contains "$out" "SKIPPED_NO_CAPACITY=203 204" "the rest are reported as skipped"
expect_contains "$out" "INFLIGHT_AFTER=2" "never exceeds the ceiling"

echo "  double claim"
new_repo
"$SLOTS" reserve 301 >/dev/null
out="$("$SLOTS" reserve 301 302)"
expect_contains "$out" "ALREADY_HELD=301" "an already-held issue is not re-reserved"
expect_contains "$out" "RESERVED=302" "the free one still is"
expect_contains "$out" "INFLIGHT_AFTER=2" "no double counting"

echo "  bad arguments"
new_repo
out="$("$SLOTS" reserve 401 not-a-number 2>&1)"
expect_contains "$out" "ERROR:" "a bad argument is an error"
expect_missing "$out" "RESERVED=" "and nothing is reserved on that pass"
out="$("$SLOTS" count)"
expect_contains "$out" "INFLIGHT=0" "the registry is untouched"

# --- tunables ---------------------------------------------------------------
echo "  tunables"
new_repo
printf 'MAX_AGENTS=1\nREAP_AFTER_MIN=90\nSTARTUP_GRACE_MIN=10\n' > .claude/issue-loop/tunables
out="$("$SLOTS" reserve 501 502)"
expect_contains "$out" "CEILING=1" "the ceiling is read from the tunables file"
expect_contains "$out" "SKIPPED_NO_CAPACITY=502" "and enforced for a sub-agent that never sourced env.sh"

# --- reaping ----------------------------------------------------------------
echo "  reap: no worktree"
new_repo
"$SLOTS" reserve 601 >/dev/null
age_lease 601 5
out="$("$SLOTS" reap)"
expect_missing "$out" "REAPED=601" "a fresh reservation is protected by the startup grace"

age_lease 601 15
out="$("$SLOTS" reap)"
expect_contains "$out" "REAPED=601" "a reservation whose worktree never appeared is reaped after the grace"
expect_contains "$out" "no worktree" "and says why"

echo "  reap: no activity"
new_repo
"$SLOTS" reserve 701 >/dev/null
mkdir -p .claude/worktrees/issue-701
echo hello > .claude/worktrees/issue-701/file.txt
age_lease 701 120
out="$("$SLOTS" reap)"
expect_missing "$out" "REAPED=701" "a worktree written to recently keeps its slot"

touch -d "@$(( $(date -u +%s) - 120 * 60 ))" .claude/worktrees/issue-701/file.txt
out="$("$SLOTS" reap)"
expect_contains "$out" "REAPED=701" "a worktree quiet for REAP_AFTER_MIN loses it"

echo "  reap: git churn does not fake liveness"
new_repo
"$SLOTS" reserve 801 >/dev/null
mkdir -p .claude/worktrees/issue-801/.git
echo stale > .claude/worktrees/issue-801/work.txt
touch -d "@$(( $(date -u +%s) - 120 * 60 ))" .claude/worktrees/issue-801/work.txt
echo fresh > .claude/worktrees/issue-801/.git/index    # written now
age_lease 801 120
out="$("$SLOTS" reap)"
expect_contains "$out" "REAPED=801" "activity inside .git/ is excluded"

echo "  reap: corrupt lease"
new_repo
"$SLOTS" reserve 901 >/dev/null
mkdir -p .claude/worktrees/issue-901
sed -i.bak 's/^started=.*/started=/' .claude/issue-loop/inflight/issue-901.lease
rm -f .claude/issue-loop/inflight/issue-901.lease.bak
touch -d "@$(( $(date -u +%s) - 200 * 60 ))" .claude/issue-loop/inflight/issue-901.lease
out="$("$SLOTS" reap)"
expect_contains "$out" "REAPED=901" "a lease with no start time is reaped rather than held forever"

# --- lifecycle --------------------------------------------------------------
echo "  bind / touch / release"
new_repo
"$SLOTS" reserve 1001 >/dev/null
out="$("$SLOTS" bind 1001 issue-1001 /tmp/wt-1001)"
expect_contains "$out" "BOUND=1001" "bind records the real branch and worktree"
expect_contains "$(cat .claude/issue-loop/inflight/issue-1001.lease)" "branch=issue-1001" "the branch is persisted"

out="$("$SLOTS" bind 1002 issue-1002 /tmp/wt 2>&1)"
expect_contains "$out" "NOSLOT=1002" "binding without a slot is refused"

out="$("$SLOTS" touch 1001)"
expect_contains "$out" "TOUCHED=1001" "a held issue heartbeats"
out="$("$SLOTS" touch 1002 2>&1)"
expect_contains "$out" "NOSLOT=1002" "a reaped issue reports NOSLOT"

out="$("$SLOTS" release 1001 1002)"
expect_contains "$out" "RELEASED=1001" "a held slot is released"
expect_contains "$out" "NOT-HELD=1002" "an unheld one says so"

echo "  reacquire"
new_repo
"$SLOTS" reserve 1101 >/dev/null
out="$("$SLOTS" reacquire 1101 issue-1101-b /tmp/wt-b)"
expect_contains "$out" "REACQUIRED=1101 held=yes" "a resumed agent re-takes its own counted slot"
out="$("$SLOTS" count)"
expect_contains "$out" "INFLIGHT=1" "without taking a second one"

out="$(MAX_AGENTS=1 "$SLOTS" reacquire 1102 2>&1)"
expect_contains "$out" "NOSLOT=1102" "reacquiring beyond the ceiling is refused"

# --- one registry across worktrees ------------------------------------------
echo "  registry is shared across worktrees"
new_repo
git commit --quiet --allow-empty -m init
"$SLOTS" reserve 1201 >/dev/null
git worktree add --quiet .claude/worktrees/issue-1201 -b issue-1201 2>/dev/null
out="$(cd .claude/worktrees/issue-1201 && "$SLOTS" count)"
expect_contains "$out" "INFLIGHT=1" "a call from inside a linked worktree reads the main registry"
out="$(cd .claude/worktrees/issue-1201 && "$SLOTS" touch 1201)"
expect_contains "$out" "TOUCHED=1201" "and can heartbeat its own lease from there"

# ----------------------------------------------------------------------------
printf '\n%s passed, %s failed\n' "$PASS" "$FAIL"
[ "$FAIL" -eq 0 ]
