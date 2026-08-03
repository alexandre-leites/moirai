//go:build integration

// Task-source multiplicity tests for #293: multi-source sync isolation,
// zero-source no-op, and the data migration that backfills an existing
// (pre-#293) project's single implied source into app.project_task_sources.
package server

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/migrate"
)

// multiSourceGitHub is a TaskSource/CodeHost double whose ListTasks behaves
// per task source: it fails outright for one configured (by id) source and
// succeeds for every other, recording one task per successful call. This is
// what proves syncSource's per-source failure isolation actually depends on
// the taskSourceID argument #293 added to the TaskSource interface, not on
// some project-wide flag.
type multiSourceGitHub struct {
	stubGitHub
	mu         sync.Mutex
	failSource string
	callsBySrc map[string]int
}

func (g *multiSourceGitHub) ListTasks(_ context.Context, _, taskSourceID, _ string) ([]Task, error) {
	g.mu.Lock()
	if g.callsBySrc == nil {
		g.callsBySrc = map[string]int{}
	}
	g.callsBySrc[taskSourceID]++
	g.mu.Unlock()
	if taskSourceID == g.failSource {
		return nil, errors.New("this source's token was revoked")
	}
	return []Task{{
		ExternalID: "from-" + taskSourceID[:8], Title: "task from " + taskSourceID,
		URL: "https://example.test/" + taskSourceID, Eligible: true, State: "open",
	}}, nil
}

func (g *multiSourceGitHub) calls(taskSourceID string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.callsBySrc[taskSourceID]
}

// TestSyncProjectIsolatesPerSourceFailures pins #293's per-source isolation
// requirement: a project with two configured task sources where one fails
// must still sync the other, and record the failure/success independently in
// app.issue_sync_state (now keyed by task_source_id, not project_id) rather
// than one source's trouble silently swallowing or masking its sibling's
// result.
func TestSyncProjectIsolatesPerSourceFailures(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	// h.project() already created one source; capture its id before adding a
	// second one, since both are named "github-<random>" and looking it up
	// afterwards (ORDER BY name) would not reliably tell the two apart.
	firstSourceID := h.defaultTaskSource(projectID)
	secondSourceID := h.taskSource(projectID, "github", `{"ref":"acme/second"}`)

	adapter := &multiSourceGitHub{failSource: secondSourceID}
	h.setGitHub(adapter)

	err := h.syncProject(context.Background(), projectID)
	if err == nil {
		t.Fatal("syncProject: want an error surfaced from the failing second source")
	}
	if !strings.Contains(err.Error(), secondSourceID) {
		t.Fatalf("syncProject error %q does not name the failing source %s", err.Error(), secondSourceID)
	}

	// The healthy first source must have synced its task and recorded success
	// regardless of its sibling's failure.
	if adapter.calls(firstSourceID) != 1 {
		t.Fatalf("healthy source was called %d times, want exactly 1", adapter.calls(firstSourceID))
	}
	if got := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE task_source_id=$1`, firstSourceID); got == 0 {
		t.Fatal("the healthy source's task was not synced")
	}
	var firstFailures, secondFailures int
	if err := h.pool.QueryRow(context.Background(), `SELECT consecutive_failures FROM app.issue_sync_state WHERE task_source_id=$1`, firstSourceID).Scan(&firstFailures); err != nil {
		t.Fatal(err)
	}
	if firstFailures != 0 {
		t.Fatalf("healthy source's consecutive_failures = %d, want 0 -- the failing sibling must not have touched it", firstFailures)
	}
	if err := h.pool.QueryRow(context.Background(), `SELECT consecutive_failures FROM app.issue_sync_state WHERE task_source_id=$1`, secondSourceID).Scan(&secondFailures); err != nil {
		t.Fatal(err)
	}
	if secondFailures != 1 {
		t.Fatalf("failing source's consecutive_failures = %d, want 1", secondFailures)
	}
}

// TestSyncProjectWithZeroSourcesIsANoOp pins #293's "zero sources is valid"
// requirement directly against the sync path: a project with no configured
// task source at all (not even the auto-migrated default -- this project is
// created without ever calling h.project()/h.taskSource()) must sync without
// error and without discovering any issues, rather than erroring on "no
// source configured".
func TestSyncProjectWithZeroSourcesIsANoOp(t *testing.T) {
	h := newHarness(t)
	projectID := idgen.NewID()
	h.exec(`INSERT INTO app.projects(id,name,repository_mode,repository_url,default_branch) VALUES($1,$2,'managed_clone','https://github.com/acme/none.git','main')`,
		projectID, "zero-source-"+projectID[:8])

	adapter := &multiSourceGitHub{}
	h.setGitHub(adapter)

	if err := h.syncProject(context.Background(), projectID); err != nil {
		t.Fatalf("syncProject on a zero-source project: %v, want nil", err)
	}
	if got := h.scalar(`SELECT COUNT(*) FROM app.project_task_sources WHERE project_id=$1`, projectID); got != 0 {
		t.Fatalf("test setup: project has %d task sources, want 0", got)
	}
	if got := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE project_id=$1`, projectID); got != 0 {
		t.Fatalf("zero-source project synced %d issues, want 0", got)
	}

	// SyncNow must agree: syncing a specific, enabled, zero-source project
	// through the real RPC succeeds with zero issues, not an error.
	resp, err := h.Control.SyncNow(h.adminContext(), &controlv1.SyncNowRequest{ProjectId: projectID})
	if err != nil {
		t.Fatalf("SyncNow on a zero-source project: %v, want nil", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("SyncNow results = %d, want 1", len(resp.Results))
	}
	if resp.Results[0].Error != "" {
		t.Fatalf("SyncNow result error = %q, want empty", resp.Results[0].Error)
	}
	if resp.Results[0].SyncedIssues != 0 {
		t.Fatalf("SyncNow synced %d issues, want 0", resp.Results[0].SyncedIssues)
	}
}

// excludeMigrationFS wraps the real migrations tree but hides every file
// from excludeFrom onward (by filename, which sorts the same as by version
// thanks to the fixed-width zero-padded numeric prefix) from directory
// listings, so migrate.Apply run against it stops short of the real schema
// -- what lets TestMigration026BackfillsExistingProjects recreate a
// genuinely pre-#293 database (issue_tracker_type column, no
// app.project_task_sources) to migrate forward from, rather than only ever
// exercising 026 against a database that already has it applied.
//
// Excluding everything from excludeFrom onward, not just that one file, is
// what keeps this test correct as later migrations are added: golang-migrate
// tracks a single current version, so if a migration *after* 026 (027, say)
// were still visible here, applying this filtered tree would advance the
// tracked version straight past 026 without ever running it -- leaving 026
// permanently skipped once the "apply the rest" step below sees nothing
// pending.
type excludeMigrationFS struct {
	fs.FS
	excludeFrom string
}

func (e excludeMigrationFS) ReadDir(name string) ([]fs.DirEntry, error) {
	entries, err := fs.ReadDir(e.FS, name)
	if err != nil {
		return nil, err
	}
	kept := entries[:0]
	for _, entry := range entries {
		if entry.Name() < e.excludeFrom {
			kept = append(kept, entry)
		}
	}
	return kept, nil
}

// scratchDatabaseURL creates a fresh, empty database on the same PostgreSQL
// server LOOP_TEST_DATABASE_URL points at (dropping the environment's shared
// loop_test database is not an option -- every other integration test uses
// it concurrently), and registers its cleanup. Returns "" (skipping the
// test) if LOOP_TEST_DATABASE_URL is not set, matching newHarness.
func scratchDatabaseURL(t *testing.T, name string) string {
	t.Helper()
	base := os.Getenv("LOOP_TEST_DATABASE_URL")
	if base == "" {
		t.Skip("LOOP_TEST_DATABASE_URL is not set")
	}
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatalf("parse LOOP_TEST_DATABASE_URL: %v", err)
	}
	admin := *parsed
	admin.Path = "/postgres"
	ctx := context.Background()
	adminPool, err := pgxpool.New(ctx, admin.String())
	if err != nil {
		t.Fatalf("connect to maintenance database: %v", err)
	}
	defer adminPool.Close()
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name)); err != nil {
		t.Fatalf("drop pre-existing scratch database: %v", err)
	}
	if _, err := adminPool.Exec(ctx, fmt.Sprintf(`CREATE DATABASE %s`, name)); err != nil {
		t.Fatalf("create scratch database: %v", err)
	}
	t.Cleanup(func() {
		cleanupCtx := context.Background()
		cleanupPool, err := pgxpool.New(cleanupCtx, admin.String())
		if err != nil {
			return
		}
		defer cleanupPool.Close()
		_, _ = cleanupPool.Exec(cleanupCtx, fmt.Sprintf(`DROP DATABASE IF EXISTS %s WITH (FORCE)`, name))
	})
	scratch := *parsed
	scratch.Path = "/" + name
	return scratch.String()
}

// TestMigration026BackfillsExistingProjects proves migration 026's data
// migration end to end: a project seeded in the exact shape it had before
// #293 (app.projects.issue_tracker_type set, app.issues keyed the old way,
// an app.issue_sync_state row keyed by project_id with a real failure
// streak already on it) must come out the other side of 026 with exactly
// one app.project_task_sources row carrying that project's old provider and
// repository_url, its issue re-keyed onto that source with nothing else
// about it disturbed, and its sync history (consecutive_failures/last_error)
// preserved rather than reset -- "existing projects keep working with zero
// manual intervention" is the acceptance criterion this pins directly against
// a simulated old project, not just a fresh one.
func TestMigration026BackfillsExistingProjects(t *testing.T) {
	// -293 namespaces this scratch database against any other concurrent
	// issue-loop worktree exercising the same test suite on this Postgres
	// server (see the gh-issue-loop concurrency-hazard convention).
	dbURL := scratchDatabaseURL(t, "loop_migration_test_293")
	ctx := context.Background()

	preSchema := excludeMigrationFS{FS: os.DirFS("../.."), excludeFrom: "026_project_task_sources.sql"}
	if err := migrate.Apply(ctx, dbURL, preSchema); err != nil {
		t.Fatalf("apply pre-#293 migrations: %v", err)
	}

	pool, err := pgxpool.New(ctx, dbURL)
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()

	projectID, issueID := idgen.NewID(), idgen.NewID()
	if _, err := pool.Exec(ctx, `INSERT INTO app.projects(id,name,repository_mode,repository_url,default_branch,issue_tracker_type)
		VALUES($1,$2,'managed_clone','https://github.com/acme/legacy.git','main','local_file')`,
		projectID, "legacy-"+projectID[:8]); err != nil {
		t.Fatalf("seed legacy project: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,url,state,eligible,external_created_at,external_updated_at)
		VALUES($1,$2,'local_file','9','9','Legacy issue','https://example.test/9','open',true,now(),now())`,
		issueID, projectID); err != nil {
		t.Fatalf("seed legacy issue: %v", err)
	}
	if _, err := pool.Exec(ctx, `INSERT INTO app.issue_sync_state(project_id,consecutive_failures,last_error,updated_at)
		VALUES($1,3,'legacy failure',now())`, projectID); err != nil {
		t.Fatalf("seed legacy issue_sync_state: %v", err)
	}

	// Applying the full (unfiltered) migrations tree from here only has 026
	// left pending -- this is the upgrade itself.
	if err := migrate.Apply(ctx, dbURL, os.DirFS("../..")); err != nil {
		t.Fatalf("apply migration 026: %v", err)
	}

	var sourceCount int
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM app.project_task_sources WHERE project_id=$1`, projectID).Scan(&sourceCount); err != nil {
		t.Fatal(err)
	}
	if sourceCount != 1 {
		t.Fatalf("project_task_sources rows for the legacy project = %d, want exactly 1", sourceCount)
	}

	var sourceID, provider, configuration string
	if err := pool.QueryRow(ctx, `SELECT id::text, provider, configuration::text FROM app.project_task_sources WHERE project_id=$1`, projectID).Scan(&sourceID, &provider, &configuration); err != nil {
		t.Fatal(err)
	}
	if provider != "local_file" {
		t.Fatalf("migrated source provider = %q, want %q (the project's old issue_tracker_type)", provider, "local_file")
	}
	if want := `"ref": "https://github.com/acme/legacy.git"`; !strings.Contains(configuration, "https://github.com/acme/legacy.git") {
		t.Fatalf("migrated source configuration = %s, want it to carry the old repository_url (looked for %s)", configuration, want)
	}

	var issueTaskSourceID string
	if err := pool.QueryRow(ctx, `SELECT task_source_id::text FROM app.issues WHERE id=$1`, issueID).Scan(&issueTaskSourceID); err != nil {
		t.Fatal(err)
	}
	if issueTaskSourceID != sourceID {
		t.Fatalf("legacy issue's task_source_id = %s, want the migrated source's id %s", issueTaskSourceID, sourceID)
	}

	var syncSourceID string
	var consecutiveFailures int
	var lastError string
	if err := pool.QueryRow(ctx, `SELECT task_source_id::text, consecutive_failures, last_error FROM app.issue_sync_state WHERE task_source_id=$1`, sourceID).Scan(&syncSourceID, &consecutiveFailures, &lastError); err != nil {
		t.Fatalf("issue_sync_state was not re-keyed onto the migrated source: %v", err)
	}
	if consecutiveFailures != 3 || lastError != "legacy failure" {
		t.Fatalf("migration disturbed sync history: consecutive_failures=%d last_error=%q, want 3 and %q", consecutiveFailures, lastError, "legacy failure")
	}

	var issueTrackerTypeStillExists bool
	if err := pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM information_schema.columns WHERE table_schema='app' AND table_name='projects' AND column_name='issue_tracker_type')`).Scan(&issueTrackerTypeStillExists); err != nil {
		t.Fatal(err)
	}
	if issueTrackerTypeStillExists {
		t.Fatal("app.projects.issue_tracker_type still exists after migration 026, want it dropped")
	}
}

// sealCredential encrypts value the same way SetProjectCredential does, for a
// test that needs to write a source-scoped credential directly through
// UpsertProjectCredentialForSource -- no gRPC RPC exposes that yet (see the
// PR description), so exercising it means calling the generated query
// itself.
func sealCredential(t *testing.T, value string) (ciphertext, nonce []byte) {
	t.Helper()
	aead, err := configuredCipher()
	if err != nil {
		t.Fatal(err)
	}
	nonce = make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	return aead.Seal(nil, nonce, []byte(value), nil), nonce
}

func uuidParam(t *testing.T, id string) pgtype.UUID {
	t.Helper()
	var value pgtype.UUID
	if err := value.Scan(id); err != nil {
		t.Fatalf("parse %q as a uuid: %v", id, err)
	}
	return value
}

// TestProjectCredentialSourceScopingPreventsCollision proves #293's
// credential re-keying decision actually holds: two task sources of the same
// provider (so both plausibly want a "github_token") can each store their own
// without overwriting or shadowing the other, which is exactly the collision
// a bare (project_id, kind) key could not avoid. It also confirms the
// fallback direction GetProjectCredentialSecret documents: a source with no
// credential of its own falls back to the project-level one, and a
// CodeHost-style lookup (no task source at all) only ever sees the
// project-level slot, never a source-scoped credential leaking into it.
func TestProjectCredentialSourceScopingPreventsCollision(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	ctx := context.Background()
	projectID, _ := h.project()
	sourceA := h.defaultTaskSource(projectID)
	sourceB := h.taskSource(projectID, "github", `{"ref":"acme/second"}`)

	ciphertextA, nonceA := sealCredential(t, "token-for-source-a")
	if _, err := h.queries.UpsertProjectCredentialForSource(ctx, db.UpsertProjectCredentialForSourceParams{
		ProjectID: projectID, Kind: "github_token", TaskSourceID: uuidParam(t, sourceA), Ciphertext: ciphertextA, Nonce: nonceA,
	}); err != nil {
		t.Fatalf("UpsertProjectCredentialForSource (source A): %v", err)
	}
	ciphertextB, nonceB := sealCredential(t, "token-for-source-b")
	if _, err := h.queries.UpsertProjectCredentialForSource(ctx, db.UpsertProjectCredentialForSourceParams{
		ProjectID: projectID, Kind: "github_token", TaskSourceID: uuidParam(t, sourceB), Ciphertext: ciphertextB, Nonce: nonceB,
	}); err != nil {
		t.Fatalf("UpsertProjectCredentialForSource (source B): %v", err)
	}

	aead, err := configuredCipher()
	if err != nil {
		t.Fatal(err)
	}
	open := func(secret db.GetProjectCredentialSecretRow) string {
		t.Helper()
		plaintext, err := aead.Open(nil, secret.Nonce, secret.Ciphertext, nil)
		if err != nil {
			t.Fatalf("decrypt: %v", err)
		}
		return string(plaintext)
	}

	secretA, err := h.queries.GetProjectCredentialSecret(ctx, db.GetProjectCredentialSecretParams{ProjectID: projectID, Kind: "github_token", TaskSourceID: sourceA})
	if err != nil {
		t.Fatalf("GetProjectCredentialSecret (source A): %v", err)
	}
	if got := open(secretA); got != "token-for-source-a" {
		t.Fatalf("source A resolved %q, want its own token -- the two sources' credentials collided", got)
	}
	secretB, err := h.queries.GetProjectCredentialSecret(ctx, db.GetProjectCredentialSecretParams{ProjectID: projectID, Kind: "github_token", TaskSourceID: sourceB})
	if err != nil {
		t.Fatalf("GetProjectCredentialSecret (source B): %v", err)
	}
	if got := open(secretB); got != "token-for-source-b" {
		t.Fatalf("source B resolved %q, want its own token -- the two sources' credentials collided", got)
	}

	// A CodeHost-style lookup (taskSourceID "", i.e. project-level only) must
	// see neither source-scoped credential: there is no project-level row yet.
	if _, err := h.queries.GetProjectCredentialSecret(ctx, db.GetProjectCredentialSecretParams{ProjectID: projectID, Kind: "github_token", TaskSourceID: ""}); !errors.Is(err, pgx.ErrNoRows) {
		t.Fatalf("project-level lookup with only source-scoped credentials configured = %v, want pgx.ErrNoRows", err)
	}

	// Setting a project-level credential afterwards must not disturb either
	// source-scoped one -- and a third source with none of its own falls back
	// to it, while sources A and B keep preferring their own.
	if _, err := h.Control.SetProjectCredential(h.adminContext(), &controlv1.SetProjectCredentialRequest{
		ProjectId: projectID, Kind: "github_token", Value: "project-level-token",
	}); err != nil {
		t.Fatalf("SetProjectCredential: %v", err)
	}
	secretAAfter, err := h.queries.GetProjectCredentialSecret(ctx, db.GetProjectCredentialSecretParams{ProjectID: projectID, Kind: "github_token", TaskSourceID: sourceA})
	if err != nil {
		t.Fatal(err)
	}
	if got := open(secretAAfter); got != "token-for-source-a" {
		t.Fatalf("source A resolved %q after a project-level credential was set, want its own token to still win", got)
	}

	sourceC := h.taskSource(projectID, "github", `{"ref":"acme/third"}`)
	secretC, err := h.queries.GetProjectCredentialSecret(ctx, db.GetProjectCredentialSecretParams{ProjectID: projectID, Kind: "github_token", TaskSourceID: sourceC})
	if err != nil {
		t.Fatalf("GetProjectCredentialSecret (source C, no credential of its own): %v", err)
	}
	if got := open(secretC); got != "project-level-token" {
		t.Fatalf("source C (no credential of its own) resolved %q, want the project-level fallback", got)
	}
}
