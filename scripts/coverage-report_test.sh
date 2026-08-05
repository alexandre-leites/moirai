#!/bin/sh
# Executable specification for scripts/coverage-report.sh.
#
# The script is the gate that makes a coverage regression fail CI (issue #372),
# so its three properties are worth pinning: it reports every module it
# measures, it exits non-zero when a module is below its threshold, and it
# says so on screen rather than dying silently. It shells out to `go` and
# `npm`, so the fixture stubs both with tiny fakes -- no real tests, no
# network, and a profile/percentage per module under test.
#
# Run locally with `make test-coverage-script`.
set -eu

script_dir=$(CDPATH='' cd -- "$(dirname -- "$0")" && pwd)
subject="$script_dir/coverage-report.sh"

failures=0
checks=0

work=$(mktemp -d)
trap 'rm -rf "$work"' EXIT INT TERM

# --- stubs -------------------------------------------------------------------

# The script calls `go test` from inside each module directory and later `go
# tool cover -func=<profile>` from the same directory. The fake writes a
# non-empty profile for `test` and prints the module's total from the
# per-module value the test seeds in $FAKE_COV/<profile-basename>.
cat >"$work/fake-go" <<'EOF'
#!/bin/sh
case "$1" in
	test)
		for arg in "$@"; do
			case "$arg" in
				-coverprofile=*) printf 'mode: set\nfixture/pkg:1,2 1 1\n' >"${arg#-coverprofile=}" ;;
			esac
		done
		[ -z "${FAKE_GO_TEST_FAIL:-}" ] || exit 1
		;;
	tool)
		# $3 is -func=<profile>
		profile="${3#-func=}"
		[ -s "$profile" ] || { echo "cover: no profile $profile" >&2; exit 1; }
		total=$(cat "$FAKE_COV/$(basename "$profile")")
		printf 'total:\t\t\t\t(statements)\t\t\t%s%%\n' "$total"
		;;
	*)
		echo "fake-go: unexpected args: $*" >&2
		exit 1
		;;
esac
EOF
chmod +x "$work/fake-go"

# The script runs `npm ci` when node_modules is absent, then `npm run
# test:coverage` and surfaces the "Coverage summary" block. The fake prints a
# summary and passes; a FAKE_NPM_FAIL makes it fail loudly instead.
cat >"$work/fake-npm" <<'EOF'
#!/bin/sh
if [ "$1" = "ci" ]; then
	exit 0
fi
if [ "$FAKE_NPM_FAIL" = "1" ]; then
	echo "npm error: coverage run exploded" >&2
	exit 1
fi
printf 'vitest output\nCoverage summary\nStatements   : 84.76%% ( 1374/1621 )\n'
EOF
chmod +x "$work/fake-npm"

# --- fixture: three Go modules and a web dir ---------------------------------

fixture() {
	for mod in orchestrator runner api; do
		mkdir -p "$work/$1/$mod"
		printf 'module fixture-%s\n\ngo 1.25\n' "$mod" >"$work/$1/$mod/go.mod"
	done
	mkdir -p "$work/$1/web"
}

run() {
	# shellcheck disable=SC2016
	GO="$work/fake-go" NPM="$work/fake-npm" FAKE_COV="$work/$1-cov" \
		ROOT="$work/$1" sh "$subject"
}

run_db() {
	# shellcheck disable=SC2016
	GO="$work/fake-go" NPM="$work/fake-npm" FAKE_COV="$work/$1-cov" \
		LOOP_TEST_DATABASE_URL=postgresql://x ROOT="$work/$1" sh "$subject"
}

seed() {
	mkdir -p "$work/$1-cov"
	printf '%s\n' "$2" >"$work/$1-cov/orchestrator.out"
	printf '%s\n' "$3" >"$work/$1-cov/runner.out"
	printf '%s\n' "$4" >"$work/$1-cov/api.out"
}

# fail <name> <detail>
fail() {
	printf 'FAIL %s\n%s\n' "$1" "$2" >&2
	failures=$((failures + 1))
}

# --- all modules above their thresholds passes -------------------------------

checks=$((checks + 1))
fixture happy
seed happy 49 80 72
actual=$(run_db happy 2>&1) || fail 'happy path' "exited non-zero:
$actual"
case "$actual" in
*'orchestrator   coverage: 49% (threshold 40%)'*) ;;
*) fail 'happy path reports the orchestrator' "$actual" ;;
esac
case "$actual" in
*'runner         coverage: 80% (threshold 70%)'*) ;;
*) fail 'happy path reports the runner' "$actual" ;;
esac
case "$actual" in
*'api            coverage: 72% (threshold 65%)'*) ;;
*) fail 'happy path reports the api' "$actual" ;;
esac
case "$actual" in
*'Coverage summary'*'84.76%'*) ;;
*) fail 'happy path surfaces the web summary' "$actual" ;;
esac
case "$actual" in
*FAIL*) fail 'happy path must not print FAIL' "$actual" ;;
*) ;;
esac

# --- a module below its threshold fails the run ------------------------------

checks=$((checks + 1))
fixture low
seed low 49 50 72
if actual=$(run_db low 2>&1); then
	fail 'below-threshold runner must exit non-zero' "$actual"
else
	:
fi
case "$actual" in
*'runner         coverage: 50% (threshold 70%)'*) ;;
*) fail 'below-threshold run still reports the offending module' "$actual" ;;
esac
case "$actual" in
*'FAIL: below threshold'*) ;;
*) fail 'below-threshold run names the failure' "$actual" ;;
esac

# --- the orchestrator is skipped without a database URL ----------------------

checks=$((checks + 1))
fixture nodb
seed nodb 49 80 72
actual=$(LOOP_TEST_DATABASE_URL= run nodb 2>&1) || fail 'skip path' "exited non-zero:
$actual"
case "$actual" in
*'orchestrator: skipped'*) ;;
*) fail 'without a database URL the orchestrator leg is skipped with a notice' "$actual" ;;
esac
case "$actual" in
*'orchestrator   coverage: 49%'*) fail 'a skipped orchestrator must not report coverage' "$actual" ;;
*) ;;
esac

# --- a failing go test is reported, not silent -------------------------------

checks=$((checks + 1))
fixture broken
seed broken 49 80 72
actual=$(FAKE_GO_TEST_FAIL=1 run_db broken 2>&1) || :
case "$actual" in
*'orchestrator: go test failed'*) ;;
*) fail 'a failing orchestrator suite is named' "$actual" ;;
esac

# --- a failing npm run is reported, not silent -------------------------------

checks=$((checks + 1))
actual=$(FAKE_NPM_FAIL=1 run_db happy 2>&1) || :
case "$actual" in
*'web: coverage run failed'*) ;;
*) fail 'a failing web coverage run is named' "$actual" ;;
esac

if [ "$failures" -gt 0 ]; then
	printf '%d of %d checks failed\n' "$failures" "$checks" >&2
	exit 1
fi
printf 'coverage-report.sh: %d checks passed\n' "$checks"
