#!/bin/sh
# Executable specification for scripts/integration-suite-notice.sh.
#
# The notice is the only thing standing between a developer and a green
# `make test-orchestrator` that quietly skipped the workflow state machine
# (issue #363), so its two properties are worth pinning: it must find the
# tag-excluded files without being told which they are, and it must stay silent
# when nothing is excluded (a notice that cries wolf gets ignored).
#
# Each case builds a throwaway Go module -- no dependencies, so no network --
# and points the subject at it with ROOT. Run locally with
# `make test-integration-notice`.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
subject="$script_dir/integration-suite-notice.sh"

failures=0
checks=0

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# fixture <name> -- builds $work/<name>/orchestrator as a Go module and echoes
# the root to use as ROOT.
fixture() {
	dir="$work/$1/orchestrator/internal/server"
	mkdir -p "$dir"
	printf 'module fixture\n\ngo 1.25\n' >"$work/$1/orchestrator/go.mod"
	printf 'package server\n' >"$dir/server.go"
	printf 'package server\n\nimport "testing"\n\nfunc TestUnitOne(t *testing.T) {}\n' \
		>"$dir/unit_test.go"
	printf '%s' "$work/$1"
}

# fail <name> <detail>
fail() {
	printf 'FAIL %s\n%s\n' "$1" "$2" >&2
	failures=$((failures + 1))
}

# --- the excluded suites are found, named and counted ------------------------

checks=$((checks + 1))
root=$(fixture tagged)
cat >"$root/orchestrator/internal/server/db_test.go" <<'EOF'
//go:build integration

package server

import "testing"

func TestDatabaseOne(t *testing.T) {}
func TestDatabaseTwo(t *testing.T) {}
EOF
# A constraint the script must not mistake for the tagged form: this file is
# part of the *default* build and is dropped by -tags integration, so it is
# never "not run" by a default `go test`.
# TestMain is the harness, not a test. Counting it would inflate the notice,
# and a file holding only TestMain is not a suite worth naming.
cat >"$root/orchestrator/internal/server/gate_test.go" <<'EOF'
//go:build integration

package server

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }
EOF
cat >"$root/orchestrator/internal/server/nodb_test.go" <<'EOF'
//go:build !integration

package server

import "testing"

func TestWithoutDatabase(t *testing.T) {}
EOF
actual=$(ROOT="$root" sh "$subject" 2>&1) || fail 'tagged fixture' "exited non-zero:
$actual"
case "$actual" in
*'NOT RUN: 2 tests in 1 PostgreSQL integration suites.'*) ;;
*) fail 'tagged fixture counts the excluded tests' "$actual" ;;
esac
case "$actual" in
*'internal/server/db_test.go (2 tests)'*) ;;
*) fail 'tagged fixture names the excluded file' "$actual" ;;
esac
case "$actual" in
*nodb_test.go*) fail 'a !integration file must not be reported as excluded' "$actual" ;;
*) ;;
esac
case "$actual" in
*gate_test.go*) fail 'a TestMain-only file is not a suite and must not be listed' "$actual" ;;
*) ;;
esac
case "$actual" in
*'make test-postgres-integration'*) ;;
*) fail 'tagged fixture states how to run them' "$actual" ;;
esac

# --- the notice is actionable for a developer with no PostgreSQL -------------

checks=$((checks + 1))
actual=$(ROOT="$root" LOOP_TEST_DATABASE_URL= sh "$subject" 2>&1)
case "$actual" in
*'docker run'*postgres:16-alpine*) ;;
*) fail 'without a database URL the notice says how to get one' "$actual" ;;
esac

# --- ...and stays short for one who already has it ---------------------------

checks=$((checks + 1))
actual=$(ROOT="$root" LOOP_TEST_DATABASE_URL=postgresql://x sh "$subject" 2>&1)
case "$actual" in
*'docker run'*) fail 'with a database URL the notice must not re-explain docker' "$actual" ;;
*) ;;
esac
case "$actual" in
*'make test-postgres-integration'*) ;;
*) fail 'with a database URL the notice still names the target' "$actual" ;;
esac

# --- nothing excluded, nothing said ------------------------------------------

checks=$((checks + 1))
root=$(fixture untagged)
actual=$(ROOT="$root" sh "$subject" 2>&1) || fail 'untagged fixture' "exited non-zero:
$actual"
if [ -n "$actual" ]; then
	fail 'a tree with no tag-excluded tests prints nothing' "$actual"
fi

# --- excluded files that hold no test are not a notice either ----------------

checks=$((checks + 1))
root=$(fixture gateonly)
cat >"$root/orchestrator/internal/server/gate_test.go" <<'EOF'
//go:build integration

package server

import (
	"os"
	"testing"
)

func TestMain(m *testing.M) { os.Exit(m.Run()) }
EOF
actual=$(ROOT="$root" sh "$subject" 2>&1) || fail 'gate-only fixture' "exited non-zero:
$actual"
if [ -n "$actual" ]; then
	fail 'a tag that hides only a TestMain prints nothing' "$actual"
fi

# --- a missing module is a hard error, not a silent no-op --------------------

checks=$((checks + 1))
if actual=$(ROOT="$work/nonexistent" sh "$subject" 2>&1); then
	fail 'a missing orchestrator module must exit non-zero' "$actual"
fi

if [ "$failures" -gt 0 ]; then
	printf '%d of %d checks failed\n' "$failures" "$checks" >&2
	exit 1
fi
printf 'integration-suite-notice.sh: %d checks passed\n' "$checks"
