#!/bin/sh
# Runs `go test -coverprofile` for one Go module, prints a per-function and
# total coverage report to stdout/CI logs, and fails if the module's total
# statement coverage drops below its floor.
#
# Usage: scripts/go-coverage.sh <module-dir> <floor-percent>
#
# The floor is a minimum, not a target: see the per-module comments in the
# Makefile (`coverage-go`) for why each value is set where it is. Raise a
# module's floor in the same PR that raises its actual coverage -- never
# ahead of it, or the next unrelated PR inherits a red build it can't fix.
set -eu

if [ "$#" -ne 2 ]; then
  echo "usage: $0 <module-dir> <floor-percent>" >&2
  exit 2
fi

module_dir="$1"
floor="$2"
profile="$module_dir/coverage.out"

(cd "$module_dir" && go test -coverprofile=coverage.out ./...)

echo ""
echo "== coverage detail: $module_dir =="
(cd "$module_dir" && go tool cover -func=coverage.out)

total="$(cd "$module_dir" && go tool cover -func=coverage.out | tail -1 | awk '{print $NF}' | tr -d '%')"
rm -f "$profile"

if [ -z "$total" ]; then
  echo "::error::could not determine total coverage for $module_dir" >&2
  exit 1
fi

echo ""
echo "== $module_dir total coverage: ${total}% (floor: ${floor}%) =="

awk -v total="$total" -v floor="$floor" 'BEGIN { exit !(total + 0 >= floor + 0) }' || {
  echo "::error::$module_dir statement coverage ${total}% is below the ${floor}% floor" >&2
  exit 1
}
