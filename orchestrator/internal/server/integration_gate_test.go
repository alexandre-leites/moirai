//go:build integration

package server

import (
	"fmt"
	"os"
	"testing"
)

// Asking for this package's PostgreSQL suites without a PostgreSQL is a
// mistake, not a configuration. Every one of these tests used to call t.Skip
// when LOOP_TEST_DATABASE_URL was unset, which turned
// `go test -tags integration ./...` into a run that reported success having
// exercised nothing -- the same silent-green failure mode issue #363 is about,
// one build tag deeper. `make test-postgres-integration` guards the variable
// before it invokes the toolchain, but nothing stopped anyone from invoking
// the toolchain directly.
//
// So the whole package refuses to run instead: one legible message and a
// non-zero exit, rather than 100+ skips nobody reads. Nothing here affects a
// default `go test ./...`, which does not build this file at all -- a
// developer without a database is never asked for one.
func TestMain(m *testing.M) {
	if os.Getenv("LOOP_TEST_DATABASE_URL") == "" {
		fmt.Fprint(os.Stderr, `
LOOP_TEST_DATABASE_URL is not set, and every test in this package needs it.

These are the orchestrator's PostgreSQL integration suites: the state machine's
correctness is its SQL, so there is nothing here that can run without a real
database. Refusing is deliberate -- skipping them would report success for a
run that tested nothing.

Start one and try again:

    docker run -d --name moirai-test-postgres -p 5432:5432 \
      -e POSTGRES_DB=loop_test -e POSTGRES_USER=loop \
      -e POSTGRES_PASSWORD=loop-test-password postgres:16-alpine

    LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test \
      make test-postgres-integration

To run only the tests that do not need a database, drop the tag: `+"`make test-orchestrator`"+`.
`)
		os.Exit(1)
	}
	os.Exit(m.Run())
}
