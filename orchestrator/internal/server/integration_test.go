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
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

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

func (stubGitHub) ListIssues(context.Context, string) ([]githubIssue, error) { return nil, nil }
func (stubGitHub) FindOrCreatePR(context.Context, string, string, string, string, string) (githubPR, error) {
	return githubPR{Number: "1", URL: "https://example.test/pull/1", State: "OPEN", HeadSHA: "abc"}, nil
}
func (stubGitHub) Checks(context.Context, string, string) (checkState, error) {
	return checksPending, nil
}
func (stubGitHub) MergeSquash(context.Context, string, string) error { return nil }
func (stubGitHub) Merged(context.Context, string, string) (bool, error) {
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
