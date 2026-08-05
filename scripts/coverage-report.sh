#!/bin/sh
# Collect and report test coverage across the three Go modules, and the web
# suite, enforcing a minimum per-module statement-coverage threshold.
#
# Why this exists: coverage was previously invisible everywhere. `go test`
# without -coverprofile prints per-package percentages on demand but nothing is
# aggregated, CI never fails on a drop, and the web suite has no coverage
# provider at all. This script makes the numbers visible in one pass and gates
# on them so a regression cannot slip through silently.
#
# Thresholds are per-module and deliberately below today's coverage, so the
# gate is a floor against regressions rather than a bar the code must jump to.
# The orchestrator's real coverage lives in its `integration`-tagged PostgreSQL
# suites, so this script measures it the same way test-postgres-integration
# does: with LOOP_TEST_DATABASE_URL set. Without it, the orchestrator leg is
# skipped with a notice, exactly like the integration suites themselves.
#
# Inputs (environment):
#   ROOT                     repository root. Defaults to this script's parent
#                            directory.
#   GO                       Go binary to invoke. Defaults to `go`.
#   NPM                      npm binary to invoke. Defaults to `npm`.
#   LOOP_TEST_DATABASE_URL   when set, the orchestrator's integration suites
#                            are included in its coverage total.
#   COVERAGE_DIR             directory for coverage artifacts. Defaults to a
#                            temporary directory that is removed on exit.
#
# Output (stdout): per-module coverage lines and the total, plus the web
# summary. Exits non-zero when a module's statement coverage falls below its
# threshold.
#
# Exit codes:
#   0  all measured modules meet their thresholds (or the orchestrator was
#      skipped because LOOP_TEST_DATABASE_URL was unset)
#   1  a module's coverage is below its threshold

set -eu

ROOT="${ROOT:-"$(CDPATH= cd -- "$(dirname -- "$0")/.." && pwd)"}"
GO="${GO:-go}"
NPM="${NPM:-npm}"

# Statement-coverage floors, per module. Keep them strictly below today's
# measured coverage (see README or `make coverage` output) so the gate catches
# regressions without demanding new tests all at once.
THRESHOLD_ORCHESTRATOR=40
THRESHOLD_RUNNER=70
THRESHOLD_API=65
THRESHOLD_WEB=70

COVERAGE_DIR="${COVERAGE_DIR:-"$(mktemp -d)"}"
trap 'rm -rf "$COVERAGE_DIR"' EXIT INT TERM

status=0

report() {
	name="$1"
	dir="$2"
	profile="$3"
	threshold="$4"

	if [ ! -s "$profile" ]; then
		printf '%s: no coverage profile produced\n' "$name"
		return
	fi
	total="$(cd "$dir" && "$GO" tool cover -func="$profile" | awk '/^total:/ { print $NF }' | tr -d '%')"
	printf '%-14s coverage: %s%% (threshold %s%%)\n' "$name" "$total" "$threshold"
	if awk -v got="$total" -v want="$threshold" 'BEGIN { exit !(got < want) }'; then
		printf '  FAIL: below threshold\n' >&2
		status=1
	fi
}

# Go modules. The orchestrator's real coverage requires the integration tag
# and a database; skip that leg (with a notice) when the URL is unset.
# A failing suite is a failed run, not a coverage number: report it as such
# rather than letting `set -e` kill the script silently.
if [ -n "${LOOP_TEST_DATABASE_URL:-}" ]; then
	if (cd "$ROOT/orchestrator" && "$GO" test -tags integration -coverprofile="$COVERAGE_DIR/orchestrator.out" ./... >/dev/null 2>&1); then
		report orchestrator "$ROOT/orchestrator" "$COVERAGE_DIR/orchestrator.out" "$THRESHOLD_ORCHESTRATOR"
	else
		printf 'orchestrator: go test failed\n' >&2
		status=1
	fi
else
	printf '%s\n' "orchestrator: skipped (LOOP_TEST_DATABASE_URL unset; integration suites excluded)"
fi

if (cd "$ROOT/runner" && "$GO" test -coverprofile="$COVERAGE_DIR/runner.out" ./... >/dev/null 2>&1); then
	report runner "$ROOT/runner" "$COVERAGE_DIR/runner.out" "$THRESHOLD_RUNNER"
else
	printf 'runner: go test failed\n' >&2
	status=1
fi

if (cd "$ROOT/api" && "$GO" test -coverprofile="$COVERAGE_DIR/api.out" ./... >/dev/null 2>&1); then
	report api "$ROOT/api" "$COVERAGE_DIR/api.out" "$THRESHOLD_API"
else
	printf 'api: go test failed\n' >&2
	status=1
fi

# Web coverage through vitest. The provider (@vitest/coverage-v8) is a
# devDependency; vitest fails loudly if it is missing. A fresh checkout has no
# node_modules, so install first when absent.
if [ ! -d "$ROOT/web/node_modules" ]; then
	(cd "$ROOT/web" && "$NPM" ci >/dev/null 2>&1) || {
		printf 'web: npm ci failed\n' >&2
		status=1
	}
fi
(cd "$ROOT/web" && "$NPM" run test:coverage >"$COVERAGE_DIR/web.log" 2>&1) || {
	cat "$COVERAGE_DIR/web.log" >&2
	printf 'web: coverage run failed\n' >&2
	status=1
}
# vitest prints its own pass/fail threshold handling; surface the summary.
if grep -q "Coverage summary" "$COVERAGE_DIR/web.log"; then
	sed -n '/Coverage summary/,$p' "$COVERAGE_DIR/web.log"
else
	printf '%s\n' "web: no coverage summary in output" >&2
	status=1
fi

exit "$status"
