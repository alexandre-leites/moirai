#!/bin/sh
# Report the orchestrator test suites that a default `go test` run did not
# execute, and say how to execute them.
#
# Why this exists: the PostgreSQL suites in orchestrator/internal/server carry
# the `integration` build tag, so `go test ./...` never compiles them. The Go
# toolchain has no way to tell anyone that: a package that passes in
# package-list mode prints nothing but its `ok` line, so neither a `t.Skip`
# message nor a `fmt.Println` from `TestMain` would ever reach the terminal
# (verified: only `go test` with no package arguments, or `-v`, shows test
# binary output). The result was a green `make test-orchestrator` that quietly
# omitted 100+ tests -- most of the workflow state machine. See issue #363.
#
# `make test-orchestrator` runs this immediately after the Go tests, so the
# omission is stated on screen every time instead of being inferred.
#
# The excluded set is derived from the toolchain rather than hardcoded, so it
# cannot drift: it is the difference between the test files `go list` reports
# with the tag and the ones it reports without it. That means any future build
# tag or constraint form is picked up without touching this script.
#
# Inputs (environment):
#   ROOT                     repository root. Defaults to this script's parent
#                            directory, so the script works from any cwd.
#   GO                       Go binary to invoke. Defaults to `go`.
#   LOOP_TEST_DATABASE_URL   when set, the notice drops the "how to get a
#                            PostgreSQL" instructions and just names the target.
#
# Output (stdout): a human-readable notice, or nothing at all when the tag
# excludes no test files. Always exits 0 -- this reports, it does not gate.
# Tested by scripts/integration-suite-notice_test.sh (`make test-integration-notice`).
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
root=${ROOT:-$(dirname -- "$script_dir")}
go_bin=${GO:-go}
module_dir="$root/orchestrator"

[ -d "$module_dir" ] || {
	printf '%s: no orchestrator module at %s\n' "$0" "$module_dir" >&2
	exit 1
}

# One test file per line, absolute. XTestGoFiles covers `package foo_test`
# files, which a suite could use just as well as the in-package form.
list_test_files() {
	# shellcheck disable=SC2016
	(cd "$module_dir" && "$go_bin" list "$@" -f \
		'{{$d := .Dir}}{{range .TestGoFiles}}{{$d}}/{{.}}
{{end}}{{range .XTestGoFiles}}{{$d}}/{{.}}
{{end}}' ./... | sed '/^$/d' | sort)
}

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

list_test_files >"$work/default"
list_test_files -tags integration >"$work/tagged"

# Files the tag adds, i.e. exactly what a default run leaves out.
excluded=$(grep -Fxv -f "$work/default" "$work/tagged" || true)
[ -n "$excluded" ] || exit 0

suites=0
tests=0
lines=''
printf '%s\n' "$excluded" >"$work/excluded"
while IFS= read -r file; do
	# `^func Test` is how the Go tool itself recognises a test function, so
	# this counts the same things `go test` would have run -- minus TestMain,
	# which is the harness, not a test, and which `go test` does not count
	# either. A file left with nothing (a fixtures-only or TestMain-only file)
	# is not a suite and is not listed: the notice should name what a
	# developer is missing, not every file the tag happens to hide.
	count=$(grep -E '^func Test' "$file" | grep -cvE '^func TestMain\(' || true)
	[ "$count" -gt 0 ] || continue
	suites=$((suites + 1))
	tests=$((tests + count))
	lines="$lines  - ${file#"$root"/} ($count tests)
"
done <"$work/excluded"

# Files were excluded, but none of them held a test worth naming.
[ "$suites" -gt 0 ] || exit 0

printf '\n'
printf '  NOT RUN: %d tests in %d PostgreSQL integration suites.\n' "$tests" "$suites"
printf '  They are behind the `integration` build tag, so the run above never\n'
printf '  compiled them. Most of the workflow state machine lives there.\n\n'
printf '%s\n' "$lines"
if [ -n "${LOOP_TEST_DATABASE_URL:-}" ]; then
	printf '  LOOP_TEST_DATABASE_URL is set -- run them with:\n\n'
	printf '    make test-postgres-integration\n\n'
	exit 0
fi
printf '  To run them, point LOOP_TEST_DATABASE_URL at a PostgreSQL:\n\n'
printf '    docker run -d --name moirai-test-postgres -p 5432:5432 \\\n'
printf '      -e POSTGRES_DB=loop_test -e POSTGRES_USER=loop \\\n'
printf '      -e POSTGRES_PASSWORD=loop-test-password postgres:16-alpine\n\n'
printf '    LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test \\\n'
printf '      make test-postgres-integration\n\n'
