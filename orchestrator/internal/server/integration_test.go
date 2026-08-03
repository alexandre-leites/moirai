//go:build integration

// These tests run the orchestrator's SQL against a real PostgreSQL, because the
// SQL is where its correctness lives: mutual exclusion is a primary key,
// fencing is a WHERE clause, and atomicity is a transaction boundary. None of
// that is observable from a unit test, and every lifecycle defect this suite
// pins was one a unit test could not have caught.
//
// Run with:
//
//	LOOP_TEST_DATABASE_URL=postgresql://loop:loop-test-password@localhost:5432/loop_test \
//	    go test -tags integration -race ./internal/server/
package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgxpool"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/metrics"
	"github.com/loop-engineering/orchestrator/internal/migrate"
	"google.golang.org/grpc/metadata"
)

type harness struct {
	*Server
	pool *pgxpool.Pool
	t    *testing.T
}

func newHarness(t *testing.T) *harness {
	t.Helper()
	url := os.Getenv("LOOP_TEST_DATABASE_URL")
	if url == "" {
		t.Skip("LOOP_TEST_DATABASE_URL is not set")
	}
	ctx := context.Background()
	// Applying the real migrations directory is itself a check: it proves the
	// shipped .sql files parse and apply, which nothing else verifies.
	if err := migrate.Apply(ctx, url, os.DirFS("../..")); err != nil {
		t.Fatalf("apply migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	if _, err := pool.Exec(ctx, `TRUNCATE app.workflow_events,app.job_offers,app.jobs,app.project_locks,app.workflow_runs,app.issues,app.projects,app.runners,app.user_sessions,app.users,app.audit_events RESTART IDENTITY CASCADE`); err != nil {
		t.Fatalf("reset database: %v", err)
	}
	server, err := NewWithGitHub(pool, "test", stubGitHub{})
	if err != nil {
		t.Fatal(err)
	}
	return &harness{Server: server, pool: pool, t: t}
}

// stubGitHub keeps delivery out of these tests: they are about the lifecycle
// around an execution, not about talking to GitHub.
type stubGitHub struct{}

func (stubGitHub) ListIssues(context.Context, string, string) ([]githubIssue, error) { return nil, nil }
func (stubGitHub) FindOrCreatePR(context.Context, string, string, string, string, string, string) (githubPR, error) {
	return githubPR{Number: "1", URL: "https://example.test/pull/1", State: "OPEN", HeadSHA: "abc"}, nil
}
func (stubGitHub) Checks(context.Context, string, string, string) (checkState, error) {
	return checksPending, nil
}
func (stubGitHub) MergeSquash(context.Context, string, string, string) error { return nil }
func (stubGitHub) Merged(context.Context, string, string, string) (bool, error) {
	return false, nil
}

func (h *harness) exec(query string, args ...any) {
	h.t.Helper()
	if _, err := h.pool.Exec(context.Background(), query, args...); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
}

func (h *harness) scalar(query string, args ...any) int {
	h.t.Helper()
	var count int
	if err := h.pool.QueryRow(context.Background(), query, args...).Scan(&count); err != nil {
		h.t.Fatalf("%s: %v", query, err)
	}
	return count
}

// project seeds an enabled project with one eligible open issue.
func (h *harness) project() (projectID, issueID string) {
	h.t.Helper()
	projectID, issueID = newID(), newID()
	h.exec(`INSERT INTO app.projects(id,name,repository_mode,repository_url,default_branch) VALUES($1,$2,'managed_clone','https://github.com/acme/demo.git','main')`, projectID, "demo-"+projectID[:8])
	h.exec(`INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,url,state,eligible,external_created_at,external_updated_at) VALUES($1,$2,'github','42','42','Fix scheduler','https://example.test/issues/42','open',true,now(),now())`, issueID, projectID)
	return projectID, issueID
}

// runner seeds an online, freshly heartbeating runner and registers a
// control-stream session, which is what ScheduleOnce uses to decide who can be
// offered work.
func (h *harness) runner() string {
	h.t.Helper()
	runnerID := newID()
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,$2,'online','1','[]'::jsonb,now())`, runnerID, "runner-"+runnerID[:8])
	if !h.addSession(runnerID, make(chan *runnerv1.OrchestratorToRunner, 16)) {
		h.t.Fatal("runner session was already registered")
	}
	return runnerID
}

// adminContext creates an admin user with a live session and returns a context
// carrying the session and CSRF metadata a mutating RPC requires.
func (h *harness) adminContext() context.Context {
	h.t.Helper()
	userID, session, csrf := newID(), newID(), newID()
	hash, err := passwordHash("Correct-Horse-1")
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO app.users(id,username,password_hash,role) VALUES($1,$2,$3,'admin')`, userID, "admin-"+userID[:8], hash)
	h.exec(`INSERT INTO app.user_sessions(id,user_id,token_hash,csrf_token_hash,expires_at,last_seen_at) VALUES($1,$2,$3,$4,now()+interval '1 hour',now())`, newID(), userID, hashSecret(session), hashSecret(csrf))
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, session, csrfHeader, csrf))
}

// runJob drives a scheduled job to the point where the runner holds its lease,
// returning the job and workflow identifiers.
func (h *harness) runJob(runnerID string) (jobID, workflowID string) {
	h.t.Helper()
	ctx := context.Background()
	scheduled, err := h.ScheduleOnce(ctx)
	if err != nil || !scheduled {
		h.t.Fatalf("ScheduleOnce() = (%v, %v), want a scheduled job", scheduled, err)
	}
	if err := h.pool.QueryRow(ctx, `SELECT id::text,workflow_run_id::text FROM app.jobs WHERE runner_id=$1 ORDER BY offered_at DESC LIMIT 1`, runnerID).Scan(&jobID, &workflowID); err != nil {
		h.t.Fatal(err)
	}
	if _, _, err := h.acceptOffer(ctx, runnerID, jobID); err != nil {
		h.t.Fatalf("acceptOffer: %v", err)
	}
	return jobID, workflowID
}

// A project lock is what serialises work on a repository. Two schedulers racing
// for the same project must not both win it.
func TestOnlyOneWorkflowRunsPerProject(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	h.exec(`INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,url,state,eligible,external_created_at,external_updated_at) VALUES($1,$2,'github','43','43','Second issue','https://example.test/issues/43','open',true,now(),now())`, newID(), projectID)
	h.runner()
	h.runner()

	var wg sync.WaitGroup
	for range 2 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := h.ScheduleOnce(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()

	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE project_id=$1`, projectID); locks != 1 {
		t.Fatalf("project_locks = %d, want 1", locks)
	}
	if runs := h.scalar(`SELECT COUNT(*) FROM app.workflow_runs WHERE project_id=$1`, projectID); runs != 1 {
		t.Fatalf("workflow runs = %d, want 1; two agents would be editing one repository", runs)
	}
}

// V1 has no automatic retry. A failed execution must not be re-dispatched by
// the very next scheduler tick.
func TestFailedExecutionParksItsIssueInsteadOfRespawning(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	var generation int64
	if err := h.pool.QueryRow(context.Background(), `SELECT lease_generation FROM app.jobs WHERE id=$1`, jobID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 1, Type: "failed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}

	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("a failed run kept its project lock, which stops the project scheduling anything again")
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 0 {
		t.Fatal("a failed run left its issue eligible, so the scheduler re-dispatches it forever")
	}
	scheduled, err := h.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if scheduled {
		t.Fatal("the scheduler re-created the failed work with no operator action")
	}
	if runs := h.scalar(`SELECT COUNT(*) FROM app.workflow_runs WHERE project_id=$1`, projectID); runs != 1 {
		t.Fatalf("workflow runs = %d, want 1", runs)
	}
	// The runner event vocabulary is shared with the console, which switches on
	// bare type names to render the timeline and the agent log pane.
	if events := h.scalar(`SELECT COUNT(*) FROM app.workflow_events WHERE workflow_run_id=$1 AND event_type='failed' AND severity='error'`, workflowID); events != 1 {
		t.Fatal("the failure was not recorded under the event type and severity the console reads")
	}
}

// Retry is the only way back from a failed run, so it must leave the project
// able to schedule. The original implementation took the project lock and set a
// status nothing consumed, which disabled the project permanently.
func TestRetryReopensTheIssueAndFreesTheProject(t *testing.T) {
	h := newHarness(t)
	_, issueID := h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)
	var generation int64
	if err := h.pool.QueryRow(context.Background(), `SELECT lease_generation FROM app.jobs WHERE id=$1`, jobID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 1, Type: "failed", PayloadJson: "{}",
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := h.RetryWorkflow(h.adminContext(), &controlv1.RetryWorkflowRequest{WorkflowRunId: workflowID}); err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}

	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks`); locks != 0 {
		t.Fatal("retry held the project lock, so nothing can ever be scheduled for that project again")
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 1 {
		t.Fatal("retry did not reopen the issue, so there is nothing for the scheduler to pick up")
	}
	scheduled, err := h.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if !scheduled {
		t.Fatal("the scheduler could not start work after a retry")
	}
	if runs := h.scalar(`SELECT COUNT(*) FROM app.workflow_runs`); runs != 2 {
		t.Fatalf("workflow runs = %d, want the retried run alongside the original", runs)
	}
}

// A runner that dies mid-job cannot report anything: every write path it has is
// fenced on an unexpired lease. Only the recovery sweep can release the project.
func TestRecoverySweepReclaimsAnExpiredLease(t *testing.T) {
	h := newHarness(t)
	_, issueID := h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)
	h.exec(`UPDATE app.jobs SET lease_expires_at=now()-interval '1 minute' WHERE id=$1`, jobID)

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("the abandoned run kept its project lock; that project is disabled until someone edits the database")
	}
	if open := h.scalar(`SELECT COUNT(*) FROM app.jobs WHERE id=$1 AND status IN ('preparing','running')`, jobID); open != 0 {
		t.Fatal("the job is still marked in flight after its lease expired")
	}
	if failed := h.scalar(`SELECT COUNT(*) FROM app.workflow_runs WHERE id=$1 AND status='failed'`, workflowID); failed != 1 {
		t.Fatal("the workflow run was left non-terminal, so the console shows it as active forever")
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 0 {
		t.Fatal("the abandoned run left its issue eligible, which respawns the work automatically")
	}
}

// Nothing else writes 'offline' outside an operator revoke, so a runner that
// stopped heartbeating is reported online forever — including every runner
// after a restart.
func TestRecoverySweepMarksStaleRunnersOffline(t *testing.T) {
	h := newHarness(t)
	beating := h.runner()
	silent, never := newID(), newID()
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,'silent','online','1','[]'::jsonb,now()-interval '10 minutes')`, silent)
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels) VALUES($1,'never','online','1','[]'::jsonb)`, never)

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatal(err)
	}

	for name, runnerID := range map[string]string{"stopped heartbeating": silent, "never heartbeated": never} {
		if online := h.scalar(`SELECT COUNT(*) FROM app.runners WHERE id=$1 AND status='online'`, runnerID); online != 0 {
			t.Fatalf("a runner that %s is still reported online", name)
		}
	}
	// The predicate is heartbeat age rather than "this process holds a stream",
	// so a second orchestrator replica cannot mark the first one's runners off.
	if online := h.scalar(`SELECT COUNT(*) FROM app.runners WHERE id=$1 AND status='online'`, beating); online != 1 {
		t.Fatal("the recovery sweep marked a live, heartbeating runner offline")
	}
}

// An offer has no lease yet, so the lease sweep cannot see it, while its
// project lock is held from the moment it is written.
func TestRecoverySweepReclaimsAnUnansweredOffer(t *testing.T) {
	h := newHarness(t)
	_, issueID := h.project()
	runnerID := h.runner()
	ctx := context.Background()
	scheduled, err := h.ScheduleOnce(ctx)
	if err != nil || !scheduled {
		t.Fatalf("ScheduleOnce() = (%v, %v)", scheduled, err)
	}
	h.exec(`UPDATE app.jobs SET offered_at=now()-interval '1 hour' WHERE runner_id=$1`, runnerID)

	if err := h.RecoverOnce(ctx); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks`); locks != 0 {
		t.Fatal("an offer nobody answered kept the project lock, wedging that project permanently")
	}
	// Nothing ran, so nothing was spent: this work should be offered again
	// rather than waiting for an operator to retry it.
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 1 {
		t.Fatal("an unanswered offer parked the issue even though no execution ran")
	}
}

// Refusing to read "no checks" as success means a repository whose checks never
// report would hold its project lock forever unless the wait is bounded.
func TestRecoverySweepBlocksAnAbandonedCheckWait(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)
	var generation int64
	if err := h.pool.QueryRow(context.Background(), `SELECT lease_generation FROM app.jobs WHERE id=$1`, jobID).Scan(&generation); err != nil {
		t.Fatal(err)
	}
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}
	if waiting := h.scalar(`SELECT COUNT(*) FROM app.workflow_runs WHERE id=$1 AND status='waiting_github_checks'`, workflowID); waiting != 1 {
		t.Fatal("delivery did not reach the check wait")
	}
	h.exec(`UPDATE app.workflow_runs SET updated_at=now()-interval '7 hours' WHERE id=$1`, workflowID)

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	if blocked := h.scalar(`SELECT COUNT(*) FROM app.workflow_runs WHERE id=$1 AND status='blocked' AND blocking_reason<>''`, workflowID); blocked != 1 {
		t.Fatal("a check wait that never resolves was not bounded, so its project lock is held forever")
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks`); locks != 0 {
		t.Fatal("the abandoned check wait kept its project lock")
	}
}

// Blocking is the operator saying "stop until I say otherwise". Without parking
// the issue the scheduler re-created the run on the next one-second tick.
func TestBlockingParksTheIssueAndCancellingDoesNot(t *testing.T) {
	for _, action := range []string{"block", "cancel"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t)
			_, issueID := h.project()
			runnerID := h.runner()
			_, workflowID := h.runJob(runnerID)
			ctx := h.adminContext()
			var err error
			if action == "block" {
				_, err = h.BlockWorkflow(ctx, &controlv1.BlockWorkflowRequest{WorkflowRunId: workflowID, Reason: "operator stopped it"})
			} else {
				_, err = h.CancelWorkflow(ctx, &controlv1.CancelWorkflowRequest{WorkflowRunId: workflowID, Reason: "operator stopped it"})
			}
			if err != nil {
				t.Fatalf("%s: %v", action, err)
			}
			if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks`); locks != 0 {
				t.Fatalf("%s did not release the project lock", action)
			}
			eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID)
			if action == "block" && eligible != 0 {
				t.Fatal("blocking left the issue eligible, so the scheduler restarts the work the operator stopped")
			}
			if action == "cancel" && eligible != 1 {
				t.Fatal("cancelling parked the issue; cancelling returns the work to the queue by design")
			}
		})
	}
}

// The lease generation is what stops a runner that lost its lease from still
// driving the job — including the secret resolution that fences on it.
func TestStaleLeaseGenerationIsRejected(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, _ := h.runJob(runnerID)
	var generation int64
	if err := h.pool.QueryRow(context.Background(), `SELECT lease_generation FROM app.jobs WHERE id=$1`, jobID).Scan(&generation); err != nil {
		t.Fatal(err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation + 1, EventSequence: 1, Type: "log", PayloadJson: "{}",
	}); err == nil {
		t.Fatal("an event from a generation the runner does not hold was accepted")
	}
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 2, Type: "log", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("a current-generation event was rejected: %v", err)
	}
	// Sequence numbers are monotonic, so a replay of an already-applied event
	// must not be applied a second time.
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: generation, EventSequence: 2, Type: "log", PayloadJson: "{}",
	}); err == nil {
		t.Fatal("a replayed event sequence was accepted")
	}
	if _, err := h.pool.Exec(context.Background(), `SELECT 1`); err != nil {
		t.Fatal(err)
	}
}

// Cancelling withdraws the offer as well as the job. Without that, a runner
// that answers late revives the job with a fresh lease and regains the secret
// access the cancellation was meant to end.
func TestCancelledWorkCannotBeAcceptedLater(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	ctx := context.Background()
	scheduled, err := h.ScheduleOnce(ctx)
	if err != nil || !scheduled {
		t.Fatalf("ScheduleOnce() = (%v, %v)", scheduled, err)
	}
	var jobID, workflowID string
	if err := h.pool.QueryRow(ctx, `SELECT id::text,workflow_run_id::text FROM app.jobs WHERE runner_id=$1`, runnerID).Scan(&jobID, &workflowID); err != nil {
		t.Fatal(err)
	}

	if _, err := h.CancelWorkflow(h.adminContext(), &controlv1.CancelWorkflowRequest{WorkflowRunId: workflowID, Reason: "operator stopped it"}); err != nil {
		t.Fatalf("CancelWorkflow: %v", err)
	}
	if _, _, err := h.acceptOffer(ctx, runnerID, jobID); err == nil {
		t.Fatal("a cancelled job was revived by a late acceptance, restoring the runner's lease and its access to project secrets")
	}
	if revived := h.scalar(`SELECT COUNT(*) FROM app.jobs WHERE id=$1 AND status<>'cancelled'`, jobID); revived != 0 {
		t.Fatal("the cancelled job left the cancelled state")
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks`); locks != 0 {
		t.Fatal("cancelling did not release the project lock")
	}
	_ = time.Now
}

// metricSample reads one series out of Prometheus exposition text as written,
// reporting whether it was exported at all. The value is returned unparsed so
// that "absent" and "exported as something unparseable" stay different answers.
func metricSample(body, series string) (string, bool) {
	for line := range strings.Lines(body) {
		line = strings.TrimSpace(line)
		if strings.HasPrefix(line, "#") {
			continue
		}
		name, value, found := strings.Cut(line, " ")
		if !found || name != series {
			continue
		}
		return value, true
	}
	return "", false
}

func mustMetricSample(t *testing.T, body, series string) float64 {
	t.Helper()
	value, exported := metricSample(body, series)
	if !exported {
		t.Fatalf("%s was not exported; body:\n%s", series, body)
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		t.Fatalf("%s was exported as %q, which is not a number", series, value)
	}
	return parsed
}

// The series #124 moved off the API and the runner have to carry the database's
// real numbers here, or they are placeholders again in a new place. This seeds
// a state where every count is different, so a snapshot that reported the same
// value for two of them would fail.
func TestMetricsSnapshotReportsTheDatabaseState(t *testing.T) {
	h := newHarness(t)
	ctx := context.Background()
	// Two eligible open issues in enabled projects: the queue depth.
	queuedProject, _ := h.project()
	h.exec(`INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,url,state,eligible,external_created_at,external_updated_at) VALUES($1,$2,'github','43','43','Second issue','https://example.test/issues/43','open',true,now(),now())`, newID(), queuedProject)
	// Neither of these counts: one project is disabled, one issue is closed.
	disabledProject, _ := h.project()
	h.exec(`UPDATE app.projects SET enabled=false WHERE id=$1`, disabledProject)
	h.exec(`UPDATE app.issues SET state='closed' WHERE project_id=$1`, queuedProject)
	h.exec(`UPDATE app.issues SET state='open' WHERE project_id=$1 AND external_id IN ('42','43')`, queuedProject)

	// One active workflow run and one that finished; only the first is active.
	activeRun, doneRun := newID(), newID()
	var issueID string
	if err := h.pool.QueryRow(ctx, `SELECT id::text FROM app.issues WHERE project_id=$1 LIMIT 1`, queuedProject).Scan(&issueID); err != nil {
		t.Fatal(err)
	}
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'running','implementation')`, activeRun, queuedProject, issueID, "thread-"+activeRun)
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'completed','done')`, doneRun, queuedProject, issueID, "thread-"+doneRun)
	// One scheduled job, and one that has finished.
	h.exec(`INSERT INTO app.jobs(id,workflow_run_id,project_id,status) VALUES($1,$2,$3,'running')`, newID(), activeRun, queuedProject)
	h.exec(`INSERT INTO app.jobs(id,workflow_run_id,project_id,status) VALUES($1,$2,$3,'completed')`, newID(), doneRun, queuedProject)

	// Four runners. The heartbeat age must be the oldest *enabled* one: 600s.
	// The disabled and revoked runners are far older, and picking either the
	// newest enabled runner or an out-of-service one is the failure this pins.
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,$2,'online','1','[]'::jsonb,now()-interval '600 seconds')`, newID(), "stale-"+newID()[:8])
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,$2,'online','1','[]'::jsonb,now()-interval '5 seconds')`, newID(), "fresh-"+newID()[:8])
	h.exec(`INSERT INTO app.runners(id,name,enabled,status,version,labels,last_seen_at) VALUES($1,$2,false,'offline','1','[]'::jsonb,now()-interval '9 hours')`, newID(), "disabled-"+newID()[:8])
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at,revoked_at) VALUES($1,$2,'offline','1','[]'::jsonb,now()-interval '20 hours',now())`, newID(), "revoked-"+newID()[:8])

	snapshot, err := h.MetricsSnapshot(ctx)
	if err != nil {
		t.Fatalf("MetricsSnapshot: %v", err)
	}
	if snapshot.QueueDepth != 2 {
		t.Errorf("QueueDepth = %d, want 2", snapshot.QueueDepth)
	}
	if snapshot.ActiveWorkflows != 1 {
		t.Errorf("ActiveWorkflows = %d, want 1", snapshot.ActiveWorkflows)
	}
	if snapshot.ScheduledJobs != 1 {
		t.Errorf("ScheduledJobs = %d, want 1", snapshot.ScheduledJobs)
	}
	if snapshot.EnabledRunners != 2 {
		t.Errorf("EnabledRunners = %d, want 2", snapshot.EnabledRunners)
	}
	if !snapshot.HeartbeatKnown {
		t.Fatal("HeartbeatKnown = false with two enabled runners")
	}
	if age := snapshot.OldestHeartbeatAge; age < 590*time.Second || age > 900*time.Second {
		t.Errorf("OldestHeartbeatAge = %s, want ~600s: the oldest enabled runner, not the newest and not an out-of-service one", age)
	}

	// The console RPC and the Prometheus surface run the same query, so they
	// cannot disagree about what "queue depth" means.
	rpc, err := h.GetSchedulerMetrics(h.adminContext(), &controlv1.GetSchedulerMetricsRequest{})
	if err != nil {
		t.Fatalf("GetSchedulerMetrics: %v", err)
	}
	if int64(rpc.GetQueueDepth()) != snapshot.QueueDepth || int64(rpc.GetActiveWorkflows()) != snapshot.ActiveWorkflows || int64(rpc.GetScheduledJobs()) != snapshot.ScheduledJobs {
		t.Errorf("GetSchedulerMetrics() = %+v, disagrees with the exported snapshot %+v", rpc, snapshot)
	}

	// End to end: a real listener, scraped over HTTP, serving those numbers.
	exporter := metrics.New("127.0.0.1:0", h.Server)
	if err := exporter.Start(); err != nil {
		t.Fatalf("start metrics listener: %v", err)
	}
	defer func() {
		shutdown, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		if err := exporter.Shutdown(shutdown); err != nil {
			t.Errorf("Shutdown() = %v", err)
		}
	}()
	response, err := http.Get("http://" + exporter.Addr() + "/metrics")
	if err != nil {
		t.Fatalf("scrape: %v", err)
	}
	defer response.Body.Close()
	scraped, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	body := string(scraped)
	for series, want := range map[string]float64{
		"moirai_queue_depth":      2,
		"moirai_active_workflows": 1,
		"moirai_scheduled_jobs":   1,
		"moirai_enabled_runners":  2,
	} {
		if got := mustMetricSample(t, body, series); got != want {
			t.Errorf("%s = %v, want %v", series, got, want)
		}
	}
	if age := mustMetricSample(t, body, "moirai_runner_heartbeat_age_seconds"); age < 590 || age > 900 {
		t.Errorf("moirai_runner_heartbeat_age_seconds = %v, want ~600", age)
	}
	if failures := mustMetricSample(t, body, "moirai_orchestrator_metrics_scrape_errors_total"); failures != 0 {
		t.Errorf("moirai_orchestrator_metrics_scrape_errors_total = %v after a healthy scrape, want 0", failures)
	}
}

// A scrape must not be able to take the orchestrator down, and must not report
// zeros it did not measure, when the database is gone.
func TestScrapeSurvivesAnUnreachableDatabase(t *testing.T) {
	h := newHarness(t)
	url := os.Getenv("LOOP_TEST_DATABASE_URL")
	pool, err := pgxpool.New(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	server, err := NewWithGitHub(pool, "test", stubGitHub{})
	if err != nil {
		t.Fatal(err)
	}
	exporter := metrics.New("", server)
	pool.Close()

	recorder := httptest.NewRecorder()
	exporter.Handler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("GET /metrics = %d against a closed pool, want 200", recorder.Code)
	}
	body := recorder.Body.String()
	if value, exported := metricSample(body, "moirai_queue_depth"); exported {
		t.Errorf("moirai_queue_depth = %v with no database, want the series to be absent rather than zero", value)
	}
	if count := mustMetricSample(t, body, "moirai_orchestrator_metrics_scrape_errors_total"); count != 1 {
		t.Errorf("moirai_orchestrator_metrics_scrape_errors_total = %v, want 1", count)
	}
	// The process is still serving, and the healthy harness pool is untouched.
	if _, err := h.MetricsSnapshot(context.Background()); err != nil {
		t.Fatalf("MetricsSnapshot on a live pool after a failed scrape: %v", err)
	}
}

// A runner that registered and never connected has no heartbeat to age from.
// Ignoring it would let a fleet that never came up report the age of the one
// runner that did, so its age is counted from registration instead -- the same
// rule the runner applies to itself, where the age counts from process start
// until the first heartbeat.
func TestHeartbeatAgeCountsANeverConnectedRunnerFromRegistration(t *testing.T) {
	h := newHarness(t)
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at,registered_at) VALUES($1,$2,'offline','1','[]'::jsonb,NULL,now()-interval '3000 seconds')`, newID(), "never-connected")
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,$2,'online','1','[]'::jsonb,now()-interval '10 seconds')`, newID(), "connected")

	snapshot, err := h.MetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("MetricsSnapshot: %v", err)
	}

	if snapshot.EnabledRunners != 2 {
		t.Errorf("EnabledRunners = %d, want 2", snapshot.EnabledRunners)
	}
	if age := snapshot.OldestHeartbeatAge; age < 2990*time.Second || age > 3300*time.Second {
		t.Errorf("OldestHeartbeatAge = %s, want ~3000s: a runner that never connected is not fresh, and not invisible", age)
	}
}

// outboundChannel returns the control-stream channel registered for a runner,
// which is what a test reads to observe messages the orchestrator enqueues
// for it (JobOffer, CancelExecution, DrainRunner, ...).
func (h *harness) outboundChannel(runnerID string) chan *runnerv1.OrchestratorToRunner {
	h.t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	outbound := h.sessions[runnerID]
	if outbound == nil {
		h.t.Fatalf("runner %s has no registered session", runnerID)
	}
	return outbound
}

// drainOffer discards the JobOffer message ScheduleOnce leaves on a runner's
// channel, so a test can then look past it at whatever the orchestrator sends
// next.
func (h *harness) drainOffer(outbound chan *runnerv1.OrchestratorToRunner) {
	h.t.Helper()
	select {
	case message := <-outbound:
		if message.GetOffer() == nil {
			h.t.Fatalf("expected a JobOffer to drain, got %T", message.GetMessage())
		}
	case <-time.After(time.Second):
		h.t.Fatal("timed out waiting for the job offer")
	}
}

// leaseGeneration reads the fencing generation the runner holds, which every
// execution event has to carry to be accepted.
func (h *harness) leaseGeneration(jobID string) int64 {
	h.t.Helper()
	var generation int64
	if err := h.pool.QueryRow(context.Background(), `SELECT lease_generation FROM app.jobs WHERE id=$1`, jobID).Scan(&generation); err != nil {
		h.t.Fatal(err)
	}
	return generation
}

// runState is the terminal shape of a run: everything the console reads to tell
// a deliberate stop from a crash.
type runState struct {
	status, phase, blocking, terminal string
	completed                         bool
}

func (h *harness) runState(workflowID string) runState {
	h.t.Helper()
	var state runState
	var blocking, terminal *string
	if err := h.pool.QueryRow(context.Background(), `SELECT status,current_phase,blocking_reason,terminal_reason,completed_at IS NOT NULL FROM app.workflow_runs WHERE id=$1`, workflowID).
		Scan(&state.status, &state.phase, &blocking, &terminal, &state.completed); err != nil {
		h.t.Fatal(err)
	}
	state.blocking, state.terminal = stringValue(blocking), stringValue(terminal)
	return state
}

// The defect this pins: an agent that stopped deliberately and said why was
// stored as an anonymous crash. The runner keeps the terminal event type
// `failed` because that vocabulary is a shared contract, and marks the payload
// `blocked: true` (runner/README.md, "An agent-reported block is not a crash"),
// so the run itself has to end `blocked` with the agent's account in
// blocking_reason. Ending it `failed` with an empty reason kept it out of the
// console's needs-attention triage -- waiting_human union blocked -- which is
// the one place an operator looks for work that is waiting on them.
func TestAgentDeclaredBlockEndsTheRunBlockedWithItsReason(t *testing.T) {
	h := newHarness(t)
	_, issueID := h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	payload := `{"status":"blocked","blocked":true,"exitCode":0,"summary":"the deployment credential is missing","remainingWork":["obtain DEPLOY_KEY","re-run the migration"],"failureFingerprint":"execution:abc","error":"the agent reported itself blocked"}`
	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "failed", PayloadJson: payload,
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked: a stated stop is being filed as an anonymous failure", state.status)
	}
	// A phase left at `failed` under a `blocked` status is the same defect one
	// column over: the console reads both.
	if state.phase != "blocked" {
		t.Fatalf("current_phase = %q, want blocked to match the status", state.phase)
	}
	if !strings.Contains(state.blocking, "the deployment credential is missing") {
		t.Fatalf("blocking_reason = %q, want the agent's summary", state.blocking)
	}
	if !strings.Contains(state.blocking, "obtain DEPLOY_KEY") || !strings.Contains(state.blocking, "re-run the migration") {
		t.Fatalf("blocking_reason = %q, want the remaining work the agent named", state.blocking)
	}
	if len(state.blocking) > maxBlockingReasonBytes || state.terminal != state.blocking {
		t.Fatalf("terminal_reason = %q, blocking_reason = %q: the two are written together everywhere else", state.terminal, state.blocking)
	}
	if !state.completed {
		t.Fatal("a blocked run has no completed_at, so it reads as still running")
	}

	// The event type is a shared vocabulary and is stored exactly as it
	// arrived. Only the run's derived status changes.
	if events := h.scalar(`SELECT COUNT(*) FROM app.workflow_events WHERE workflow_run_id=$1 AND event_type='failed' AND severity='error'`, workflowID); events != 1 {
		t.Fatal("the terminal event was not stored under the type and severity the console reads")
	}
	if jobs := h.scalar(`SELECT COUNT(*) FROM app.jobs WHERE id=$1 AND status='failed' AND finished_at IS NOT NULL`, jobID); jobs != 1 {
		t.Fatal("the job's status left the vocabulary it shares with the event type")
	}

	// Lock release and issue parking are the parts that must NOT have changed:
	// a block is a non-delivering outcome like any other.
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("a blocked run kept its project lock, so the project can never schedule again")
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 0 {
		t.Fatal("a blocked run left its issue eligible, so the scheduler re-dispatches the same block forever")
	}
	scheduled, err := h.ScheduleOnce(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if scheduled {
		t.Fatal("the scheduler re-created the blocked work with no operator action")
	}

	// The gauge's help text is "runs that have not reached a terminal status".
	// A blocked run has reached one, so it must not be counted.
	snapshot, err := h.MetricsSnapshot(context.Background())
	if err != nil {
		t.Fatalf("MetricsSnapshot: %v", err)
	}
	if snapshot.ActiveWorkflows != 0 {
		t.Fatalf("moirai_active_workflows = %d after the only run blocked, want 0", snapshot.ActiveWorkflows)
	}

	// Retry is still the way back, and it is gated on the terminal status.
	if _, err := h.RetryWorkflow(h.adminContext(), &controlv1.RetryWorkflowRequest{WorkflowRunId: workflowID}); err != nil {
		t.Fatalf("RetryWorkflow on an agent-blocked run: %v", err)
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 1 {
		t.Fatal("retry did not reopen the issue of an agent-blocked run")
	}
}

// The inverse defect, which would be worse than the original: a genuine crash
// filed as a deliberate block hides a real failure behind "someone decided to
// stop". The payload is agent-supplied, so a malformed one must degrade to
// `failed` rather than reject the terminal event -- losing a terminal event
// strands the run and its project lock.
func TestOnlyAGenuineBlockDeclarationDivertsTheTerminalStatus(t *testing.T) {
	for name, payload := range map[string]string{
		"a crash":                      `{"status":"failed","exitCode":1,"error":"agent exited 1","failureFingerprint":"execution:abc"}`,
		"an empty payload":             `{}`,
		"a payload that is not object": `"blocked"`,
		"a false flag":                 `{"status":"blocked","blocked":false,"summary":"x"}`,
		"a null flag":                  `{"blocked":null,"summary":"x"}`,
		"a stringly-typed flag":        `{"blocked":"true","summary":"x"}`,
		"a status with no flag":        `{"status":"blocked","summary":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t)
			h.project()
			runnerID := h.runner()
			jobID, workflowID := h.runJob(runnerID)

			if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
				JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "failed", PayloadJson: payload,
			}); err != nil {
				t.Fatalf("persistExecutionEvent(%s): %v", payload, err)
			}

			state := h.runState(workflowID)
			if state.status != "failed" || state.phase != "failed" {
				t.Fatalf("status/phase = %q/%q for payload %s, want failed: a crash filed as a block is a hidden failure", state.status, state.phase, payload)
			}
			if state.blocking != "" {
				t.Fatalf("blocking_reason = %q for payload %s, want it left empty", state.blocking, payload)
			}
			if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
				t.Fatal("the failed run kept its project lock")
			}
		})
	}
}

// Agent prose reaching a text column: PostgreSQL rejects a NUL byte outright,
// and the whole terminal event fails with it -- the run would keep its project
// lock and never end. The bound is the orchestrator's own; the runner applies
// one too, but the orchestrator does not get to assume a runner it does not
// control did so.
func TestAnAgentBlockSurvivesHostileAndOversizedProse(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	entries := make([]string, 40)
	for index := range entries {
		entries[index] = strings.Repeat("漢", 400)
	}
	payload, err := json.Marshal(map[string]any{
		"status":  "blocked",
		"blocked": true,
		// An ANSI introducer, which PostgreSQL stores in jsonb happily and
		// which must not survive into an operator-facing column. A NUL byte
		// cannot be tested here: jsonb rejects one anywhere in the payload,
		// so the whole event insert fails before the reason is composed --
		// a separate defect, pinned against the composer in server_test.go.
		"summary":       "credential missing\x1b[31m and more prose",
		"remainingWork": entries,
	})
	if err != nil {
		t.Fatal(err)
	}

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "failed", PayloadJson: string(payload),
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "blocked" {
		t.Fatalf("status = %q, want blocked", state.status)
	}
	if len(state.blocking) > maxBlockingReasonBytes {
		t.Fatalf("blocking_reason is %d bytes, over the %d byte bound", len(state.blocking), maxBlockingReasonBytes)
	}
	if strings.ContainsAny(state.blocking, "\x00\x1b") || !utf8.ValidString(state.blocking) {
		t.Fatalf("blocking_reason = %q kept a control byte or invalid UTF-8", state.blocking)
	}
	if !strings.Contains(state.blocking, "credential missing") {
		t.Fatalf("blocking_reason = %q lost the agent's words", state.blocking)
	}
	// The remaining-work list is the half an operator acts on, so it has to
	// survive the bound rather than be crowded out by prose, and the list it
	// opens has to close.
	if !strings.Contains(state.blocking, "\u6f22") || !strings.HasSuffix(state.blocking, ")") {
		t.Fatalf("blocking_reason = %q dropped the remaining work or left its list unclosed", state.blocking)
	}
}

// Only a `failed` event is read for a block declaration. The guard matters
// because `completed` is the delivery path: deliverWorkflow opens the pull
// request under `WHERE id=$1 AND status='completed'`, so a `completed` event
// diverted to `blocked` would lose a delivered branch, and a `cancelled` one
// reached no outcome of its own to declare. Neither carries the flag today —
// the runner only sets it beside a `failed` event — so this pins the guard
// rather than any current runner behaviour.
func TestOnlyAFailedEventIsReadForABlockDeclaration(t *testing.T) {
	for eventType, want := range map[string]string{"completed": "waiting_github_checks", "cancelled": "cancelled"} {
		t.Run(eventType, func(t *testing.T) {
			h := newHarness(t)
			h.project()
			runnerID := h.runner()
			jobID, workflowID := h.runJob(runnerID)

			if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
				JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: eventType,
				PayloadJson: `{"status":"blocked","blocked":true,"summary":"the deployment credential is missing"}`,
			}); err != nil {
				t.Fatalf("persistExecutionEvent(%s): %v", eventType, err)
			}

			state := h.runState(workflowID)
			if state.status != want {
				t.Fatalf("a %s event carrying blocked:true left the run %q, want %q", eventType, state.status, want)
			}
			if state.blocking != "" {
				t.Fatalf("blocking_reason = %q for a %s event, want it left empty", state.blocking, eventType)
			}
		})
	}
}

// Cancelling or blocking a workflow used to only update the database: the
// runner still holding the job's lease was never told, so it kept the agent
// running indefinitely (issue #260). Both actions must reach the runner's
// control stream with the execution ID and the lease generation it is
// currently holding, so the runner can match it against its active
// execution before the database's own bump of that generation.
func TestCancelAndBlockNotifyTheRunnersControlStream(t *testing.T) {
	for _, action := range []string{"cancel", "block"} {
		t.Run(action, func(t *testing.T) {
			h := newHarness(t)
			h.project()
			runnerID := h.runner()
			outbound := h.outboundChannel(runnerID)
			jobID, workflowID := h.runJob(runnerID)
			h.drainOffer(outbound)
			generationBeforeCancel := h.leaseGeneration(jobID)

			ctx := h.adminContext()
			var err error
			if action == "block" {
				_, err = h.BlockWorkflow(ctx, &controlv1.BlockWorkflowRequest{WorkflowRunId: workflowID, Reason: "operator stopped it"})
			} else {
				_, err = h.CancelWorkflow(ctx, &controlv1.CancelWorkflowRequest{WorkflowRunId: workflowID, Reason: "operator stopped it"})
			}
			if err != nil {
				t.Fatalf("%s: %v", action, err)
			}

			select {
			case message := <-outbound:
				cancellation := message.GetCancel()
				if cancellation == nil {
					t.Fatalf("%s sent %T on the control stream, want a CancelExecution", action, message.GetMessage())
				}
				if want := implementExecutionID(jobID); cancellation.GetExecutionId() != want {
					t.Fatalf("%s sent execution ID %q, want %q", action, cancellation.GetExecutionId(), want)
				}
				if cancellation.GetLeaseGeneration() != generationBeforeCancel {
					t.Fatalf("%s sent lease generation %d, want the generation the runner was holding (%d)", action, cancellation.GetLeaseGeneration(), generationBeforeCancel)
				}
			case <-time.After(time.Second):
				t.Fatalf("%s never sent a CancelExecution to the runner", action)
			}
		})
	}
}

// A workflow can be cancelled or blocked before the scheduler ever gave it a
// job — while the run is still in "planning", say — in which case there is no
// runner to notify. That must stay a no-op rather than an error or a panic.
func TestCancelWithoutAJobDoesNotErrorOrNotifyAnyone(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	runnerID := h.runner()
	outbound := h.outboundChannel(runnerID)
	workflowID := newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase,branch_name) VALUES($1,$2,$3,$4,'planning','planning','agent/'||$4)`, workflowID, projectID, issueID, workflowID)

	if _, err := h.CancelWorkflow(h.adminContext(), &controlv1.CancelWorkflowRequest{WorkflowRunId: workflowID, Reason: "operator stopped it"}); err != nil {
		t.Fatalf("CancelWorkflow: %v", err)
	}

	select {
	case message := <-outbound:
		t.Fatalf("cancelling a jobless run sent %T to an unrelated runner", message.GetMessage())
	default:
	}
}

// A workflow's job can outlive the runner's session — the runner crashed or
// lost its connection, and the recovery sweep has not yet reclaimed the lease.
// Cancelling it must still succeed and must not block waiting for a session
// that will never accept a send.
func TestCancelWithADisconnectedRunnerStillSucceeds(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)
	h.removeSession(runnerID, h.outboundChannel(runnerID))

	done := make(chan error, 1)
	go func() {
		_, err := h.CancelWorkflow(h.adminContext(), &controlv1.CancelWorkflowRequest{WorkflowRunId: workflowID, Reason: "operator stopped it"})
		done <- err
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("CancelWorkflow: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("CancelWorkflow blocked waiting on a disconnected runner's session")
	}
	if cancelled := h.scalar(`SELECT COUNT(*) FROM app.jobs WHERE id=$1 AND status='cancelled'`, jobID); cancelled != 1 {
		t.Fatal("cancelling with a disconnected runner did not still cancel the job in the database")
	}
}

// Draining or revoking a runner used to only flip its database row: a runner
// with an in-flight job kept running it and only stopped receiving new offers.
// Both actions must push a DrainRunner down the runner's control stream.
func TestSetRunnerStateNotifiesTheRunnerOnDrainAndRevoke(t *testing.T) {
	for _, state := range []string{"drain", "revoke"} {
		t.Run(state, func(t *testing.T) {
			h := newHarness(t)
			runnerID := h.runner()
			outbound := h.outboundChannel(runnerID)

			if _, err := h.SetRunnerState(h.adminContext(), &controlv1.SetRunnerStateRequest{RunnerId: runnerID, State: state}); err != nil {
				t.Fatalf("SetRunnerState(%s): %v", state, err)
			}

			select {
			case message := <-outbound:
				if message.GetDrain() == nil {
					t.Fatalf("SetRunnerState(%s) sent %T, want a DrainRunner", state, message.GetMessage())
				}
			case <-time.After(time.Second):
				t.Fatalf("SetRunnerState(%s) never sent DrainRunner to the runner", state)
			}
		})
	}
}

// Enabling a runner is neither a drain nor a revoke, so it must not push
// anything to the control stream — an enable that also cancelled the runner's
// current job would be a much larger behavior change than "let it take new
// work again".
func TestSetRunnerStateEnableDoesNotNotifyTheRunner(t *testing.T) {
	h := newHarness(t)
	runnerID := h.runner()
	outbound := h.outboundChannel(runnerID)
	if _, err := h.SetRunnerState(h.adminContext(), &controlv1.SetRunnerStateRequest{RunnerId: runnerID, State: "drain"}); err != nil {
		t.Fatalf("SetRunnerState(drain): %v", err)
	}
	<-outbound // discard the drain notification from setting up the precondition

	if _, err := h.SetRunnerState(h.adminContext(), &controlv1.SetRunnerStateRequest{RunnerId: runnerID, State: "enable"}); err != nil {
		t.Fatalf("SetRunnerState(enable): %v", err)
	}

	select {
	case message := <-outbound:
		t.Fatalf("SetRunnerState(enable) sent %T to the runner, want nothing", message.GetMessage())
	default:
	}
}
