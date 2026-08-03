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
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/metrics"
	"github.com/loop-engineering/orchestrator/internal/migrate"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
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

// sequencedGitHub lets a test control exactly which error (if any)
// FindOrCreatePR returns on each successive call: `errs[0]` on the first
// call, `errs[1]` on the second, and so on, falling back to `alwaysErr` (or
// success, if nil) once `errs` is exhausted. That is what lets a test drive
// deliverWorkflow through a transient failure, a successful retry, or an
// unbroken run of failures that exhausts the attempt budget, all without
// touching a real `gh` process.
type sequencedGitHub struct {
	mu        sync.Mutex
	errs      []error
	alwaysErr error
	pr        githubPR
}

func (g *sequencedGitHub) nextErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if len(g.errs) > 0 {
		err := g.errs[0]
		g.errs = g.errs[1:]
		return err
	}
	return g.alwaysErr
}

func (g *sequencedGitHub) ListIssues(context.Context, string, string) ([]githubIssue, error) {
	return nil, nil
}
func (g *sequencedGitHub) FindOrCreatePR(context.Context, string, string, string, string, string, string) (githubPR, error) {
	if err := g.nextErr(); err != nil {
		return githubPR{}, err
	}
	pr := g.pr
	if pr.Number == "" {
		pr = githubPR{Number: "9", URL: "https://example.test/pull/9", State: "OPEN", HeadSHA: "deadbeef"}
	}
	return pr, nil
}
func (g *sequencedGitHub) Checks(context.Context, string, string, string) (checkState, error) {
	if err := g.nextErr(); err != nil {
		return checksPending, err
	}
	return checksGreen, nil
}
func (g *sequencedGitHub) MergeSquash(context.Context, string, string, string) error {
	return g.nextErr()
}
func (g *sequencedGitHub) Merged(context.Context, string, string, string) (bool, error) {
	if err := g.nextErr(); err != nil {
		return false, err
	}
	return true, nil
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

// schedulable reports whether the scheduler's own candidate-set predicate
// (ListQueueEntries, ClaimSchedulableIssue) would offer this issue: opted in
// by the tracker's label (app.issues.eligible) AND not excluded by a
// failed/blocked/completed workflow run that has not been superseded by a
// retry. This is deliberately not "app.issues.eligible alone" (see #268):
// that column is purely label-driven now, so a parked or delivered issue can
// still read eligible=true while being permanently excluded from scheduling
// by its run's own state.
func (h *harness) schedulable(issueID string) bool {
	h.t.Helper()
	return h.scalar(`
		SELECT COUNT(*) FROM app.issues i
		WHERE i.id=$1 AND i.eligible AND i.state='open' AND NOT EXISTS (
			SELECT 1 FROM app.workflow_runs w
			WHERE w.issue_id = i.id AND w.superseded_at IS NULL AND w.status IN ('failed','blocked','completed')
		)`, issueID) == 1
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
	return h.runnerWithCapacity(1)
}

// runnerWithCapacity is runner() with an explicit app.runners.capacity, for
// tests exercising capacity-aware scheduling.
func (h *harness) runnerWithCapacity(capacity int) string {
	h.t.Helper()
	runnerID := newID()
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,capacity,last_seen_at) VALUES($1,$2,'online','1','[]'::jsonb,$3,now())`, runnerID, "runner-"+runnerID[:8], capacity)
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
	if h.schedulable(issueID) {
		t.Fatal("a failed run left its issue schedulable, so the scheduler re-dispatches it forever")
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
	if !h.schedulable(issueID) {
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
	if h.schedulable(issueID) {
		t.Fatal("the abandoned run left its issue schedulable, which respawns the work automatically")
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
	if !h.schedulable(issueID) {
		t.Fatal("an unanswered offer parked the issue even though no execution ran")
	}
}

// A `completed` runner event does not land the run on `completed` directly
// any more (#267): it moves to `delivering` first, and deliverWorkflow's own
// guard (`WHERE status='delivering'`) is what carries it on to
// `waiting_github_checks` once the pull request is open. This pins that a
// second delivery attempt against the same workflow, once it has already
// moved on, is a no-op rather than a second pull request -- the guard a
// stranded-delivery re-drive (and any other retry of deliverWorkflow) leans
// on.
func TestCompletedEventDeliversThroughDeliveringStatus(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "completed", PayloadJson: "{}",
	}); err != nil {
		t.Fatalf("persistExecutionEvent: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "waiting_github_checks" {
		t.Fatalf("status = %q, want waiting_github_checks", state.status)
	}
	if err := h.deliverWorkflow(context.Background(), workflowID); err == nil {
		t.Fatal("deliverWorkflow re-ran against a run no longer at 'delivering', which would open a second pull request")
	}
	if prs := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1`, workflowID); prs != 1 {
		t.Fatalf("pull_requests rows = %d, want 1", prs)
	}
}

// Before StatusDelivering existed, the window between the runner's completion
// committing and the pull request being opened was spent at 'completed' while
// still holding a project_locks row -- the only way to tell that run apart
// from one that had already merged. This pins that the stranded-delivery
// sweep now finds and re-drives that run by status and age alone: no
// project_locks row is seeded here at all, and the age bound (not a lock
// join) is what would otherwise stop this from racing a delivery still in
// progress.
func TestRecoverySweepResumesAStrandedDeliveryByStatusAlone(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase,branch_name,updated_at) VALUES($1,$2,$3,$4,'delivering','delivering',$5,now()-interval '10 minutes')`,
		workflowID, projectID, issueID, "thread-"+workflowID, "agent/"+workflowID)

	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks`); locks != 0 {
		t.Fatal("test setup left a project lock behind; the point is proving none is needed")
	}

	if err := h.RecoverOnce(context.Background()); err != nil {
		t.Fatalf("RecoverOnce: %v", err)
	}

	if waiting := h.scalar(`SELECT COUNT(*) FROM app.workflow_runs WHERE id=$1 AND status='waiting_github_checks'`, workflowID); waiting != 1 {
		t.Fatal("a stranded delivery with no project lock was not re-driven by status and age alone")
	}
	if prs := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1`, workflowID); prs != 1 {
		t.Fatal("the re-driven delivery did not open a pull request")
	}
}

// A merge GitHub has already confirmed is irreversible no matter what this
// run's own status column raced to first. Before #281's fix, observeWorkflow's
// confirming UPDATE guarded on `status='waiting_github_checks'`, and a miss --
// an operator's cancel landing in the window between GitHub confirming the
// merge and this function's own database write -- returned nil with nothing
// recorded: app.pull_requests still said "open", nothing logged, and (because
// StatusCancelled does not exclude an issue from scheduling, unlike
// StatusBlocked/StatusFailed/StatusCompleted -- see schedulable's doc comment)
// the issue went straight back into the queue for a pull request already
// merged into main. This pins the fix: the run is forced to 'completed', the
// race itself becomes its own workflow event, the merge is still recorded,
// and the issue stays out of the queue.
func TestObserveWorkflowRecordsAConfirmedMergeEvenIfItsOwnStatusRaced(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase,branch_name) VALUES($1,$2,$3,$4,'waiting_github_checks','waiting_github_checks',$5)`,
		workflowID, projectID, issueID, "thread-"+workflowID, "agent/"+workflowID)
	h.exec(`INSERT INTO app.project_locks(project_id, workflow_run_id) VALUES($1,$2)`, projectID, workflowID)
	h.exec(`INSERT INTO app.pull_requests(id,workflow_run_id,provider,external_id,url,head_commit,state) VALUES($1,$2,'github','7','https://example.test/pull/7','abc','open')`, newID(), workflowID)

	// sequencedGitHub's zero value is the ordinary success path: Checks reports
	// green, MergeSquash succeeds, and Merged confirms it -- exactly what
	// observeWorkflow sees before it ever reaches its own confirming update.
	h.github = &sequencedGitHub{}

	// The race: an operator cancels the run (mirroring exactly what
	// controlWorkflow's "cancel" case does -- status to 'cancelled', project
	// lock released) in the window between GitHub confirming the merge above
	// and observeWorkflow's own guarded database write.
	h.exec(`UPDATE app.workflow_runs SET status='cancelled', current_phase='cancelled', terminal_reason='operator cancelled it' WHERE id=$1`, workflowID)
	h.exec(`DELETE FROM app.project_locks WHERE workflow_run_id=$1`, workflowID)

	if err := h.observeWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("observeWorkflow: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "completed" {
		t.Fatalf("status = %q, want completed: a confirmed merge must not be lost because the run's own status raced away from waiting_github_checks", state.status)
	}
	if !state.completed {
		t.Fatal("completed_at was not set on the forced completion")
	}

	if merged := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1 AND state='merged'`, workflowID); merged != 1 {
		t.Fatal("app.pull_requests still says the pull request is open even though GitHub confirmed the merge")
	}

	if raced := h.scalar(`SELECT COUNT(*) FROM app.workflow_events WHERE workflow_run_id=$1 AND event_type='delivery.completion_raced' AND severity='warning'`, workflowID); raced != 1 {
		t.Fatal("the race was not logged as its own workflow event")
	}
	if mergedEvent := h.scalar(`SELECT COUNT(*) FROM app.workflow_events WHERE workflow_run_id=$1 AND event_type='pull_request.merged'`, workflowID); mergedEvent != 1 {
		t.Fatal("pull_request.merged was not recorded even though the merge is confirmed")
	}

	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("the project lock is still held after a confirmed merge")
	}

	if h.schedulable(issueID) {
		t.Fatal("the issue is schedulable again even though its pull request is already merged into main")
	}
}

// seedDelivering inserts a workflow run already at 'delivering' with its
// project lock held, the state deliverWorkflow expects to find itself
// re-driven from -- see TestRecoverySweepResumesAStrandedDeliveryByStatusAlone
// for why a raw insert (rather than driving a runner through execution) is
// how these tests reach that state directly.
func (h *harness) seedDelivering(projectID, issueID string) (workflowID string) {
	h.t.Helper()
	workflowID = newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase,branch_name) VALUES($1,$2,$3,$4,'delivering','delivering',$5)`,
		workflowID, projectID, issueID, "thread-"+workflowID, "agent/"+workflowID)
	h.exec(`INSERT INTO app.project_locks(project_id, workflow_run_id) VALUES($1,$2)`, projectID, workflowID)
	return workflowID
}

// seedWaitingChecks inserts a workflow run at 'waiting_github_checks' with an
// open pull request and its project lock held -- the state observeWorkflow
// expects to find itself invoked against once GitHub reports checks green.
func (h *harness) seedWaitingChecks(projectID, issueID string) (workflowID string) {
	h.t.Helper()
	workflowID = newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase,branch_name) VALUES($1,$2,$3,$4,'waiting_github_checks','waiting_github_checks',$5)`,
		workflowID, projectID, issueID, "thread-"+workflowID, "agent/"+workflowID)
	h.exec(`INSERT INTO app.project_locks(project_id, workflow_run_id) VALUES($1,$2)`, projectID, workflowID)
	h.exec(`INSERT INTO app.pull_requests(id,workflow_run_id,provider,external_id,url,head_commit,state) VALUES($1,$2,'github','11','https://example.test/pull/11','abc','open')`, newID(), workflowID)
	return workflowID
}

// requireApproval flips a seeded project's configuration to opt into the
// human-approval gate (projectConfig.RequireHumanApproval), decoded fresh by
// deliveryWorkflow on every call, so this can run any time before the
// observeWorkflow/SubmitHumanDecision call under test.
func (h *harness) requireApproval(projectID string) {
	h.t.Helper()
	h.exec(`UPDATE app.projects SET configuration = configuration || '{"require_human_approval":true}'::jsonb WHERE id=$1`, projectID)
}

// Before StatusWaitingHuman existed, observeWorkflow merged a pull request
// the instant GitHub reported its checks green, with no way for a project to
// have a person look first: SubmitHumanDecision (management.go) answered
// "V1 has no approval phase" no matter what. This pins the gate end to end:
// green checks stop the run at 'waiting_human' instead of merging, the
// project lock and pull request are untouched while it waits, and the
// transition is recorded as its own event.
func TestObserveWorkflowStopsAtHumanApprovalWhenTheProjectRequiresIt(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	h.requireApproval(projectID)
	workflowID := h.seedWaitingChecks(projectID, issueID)
	h.github = &sequencedGitHub{}

	if err := h.observeWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("observeWorkflow: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "waiting_human" {
		t.Fatalf("status = %q, want waiting_human", state.status)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 1 {
		t.Fatal("the project lock was released while the run is only waiting on a human, not finished")
	}
	if merged := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1 AND state='merged'`, workflowID); merged != 0 {
		t.Fatal("the pull request was merged before a human approved it")
	}
	if events := h.scalar(`SELECT COUNT(*) FROM app.workflow_events WHERE workflow_run_id=$1 AND event_type='workflow_transition'`, workflowID); events != 1 {
		t.Fatal("the transition into waiting_human was not recorded as an event")
	}
	// h.schedulable only models the permanent failed/blocked/completed
	// exclusion (see its own doc comment); the exclusion that actually matters
	// while a run is merely waiting -- on GitHub checks or, here, on a human --
	// is ClaimSchedulableIssue's own `NOT EXISTS project_locks` clause, which
	// is exactly what the lock assertion above already pins.
}

// A project that never opted in must merge exactly as it always did: this is
// the regression guard that RequireHumanApproval's zero value (every project
// created before this field existed defaults to it) changes nothing.
func TestObserveWorkflowMergesDirectlyWhenTheProjectDoesNotRequireApproval(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := h.seedWaitingChecks(projectID, issueID)
	h.github = &sequencedGitHub{}

	if err := h.observeWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("observeWorkflow: %v", err)
	}

	if state := h.runState(workflowID); state.status != "completed" {
		t.Fatalf("status = %q, want completed: a project that did not opt in must merge exactly as before", state.status)
	}
	if h.schedulable(issueID) {
		t.Fatal("a completed run left its issue schedulable")
	}
}

// SubmitHumanDecision's "approved" branch, exercised through the actual gRPC
// entry point the console's decision panel calls: the run sitting at the
// gate is sent on to merge the already-green pull request.
func TestSubmitHumanDecisionApprovesAndMerges(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	h.requireApproval(projectID)
	workflowID := h.seedWaitingChecks(projectID, issueID)
	h.github = &sequencedGitHub{}
	if err := h.observeWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("observeWorkflow: %v", err)
	}
	if state := h.runState(workflowID); state.status != "waiting_human" {
		t.Fatalf("setup: status = %q, want waiting_human", state.status)
	}

	resp, err := h.SubmitHumanDecision(h.adminContext(), &controlv1.SubmitHumanDecisionRequest{
		WorkflowRunId: workflowID, Decision: "approved", Comment: "looks good",
	})
	if err != nil {
		t.Fatalf("SubmitHumanDecision: %v", err)
	}
	if resp.GetWorkflow().GetStatus() != "completed" {
		t.Fatalf("response status = %q, want completed", resp.GetWorkflow().GetStatus())
	}
	if state := h.runState(workflowID); state.status != "completed" {
		t.Fatalf("status = %q, want completed: approval must merge the already-green pull request", state.status)
	}
	if merged := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1 AND state='merged'`, workflowID); merged != 1 {
		t.Fatal("the pull request was not merged after approval")
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("the project lock is still held after the approved run completed")
	}
	if audits := h.scalar(`SELECT COUNT(*) FROM app.audit_events WHERE target_id=$1 AND action='workflow.approve'`, workflowID); audits != 1 {
		t.Fatal("the approval was not recorded in the audit log")
	}
}

// SubmitHumanDecision's "changes_requested" branch: the run ends the same
// terminal way any other rejection does -- blocked, with the reviewer's
// comment as the reason, project lock released so the project is schedulable
// again once an operator retries the issue.
func TestSubmitHumanDecisionRejectsAndBlocks(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	h.requireApproval(projectID)
	workflowID := h.seedWaitingChecks(projectID, issueID)
	h.github = &sequencedGitHub{}
	if err := h.observeWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("observeWorkflow: %v", err)
	}

	resp, err := h.SubmitHumanDecision(h.adminContext(), &controlv1.SubmitHumanDecisionRequest{
		WorkflowRunId: workflowID, Decision: "changes_requested", Comment: "please add tests",
	})
	if err != nil {
		t.Fatalf("SubmitHumanDecision: %v", err)
	}
	if resp.GetWorkflow().GetStatus() != "blocked" {
		t.Fatalf("response status = %q, want blocked", resp.GetWorkflow().GetStatus())
	}
	state := h.runState(workflowID)
	if state.status != "blocked" || !strings.Contains(state.blocking, "please add tests") {
		t.Fatalf("state = %+v, want blocked with the reviewer's comment as the reason", state)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("the project lock is still held after a rejected run was blocked")
	}
	if h.schedulable(issueID) {
		t.Fatal("a rejected, blocked run left its issue schedulable")
	}
	if merged := h.scalar(`SELECT COUNT(*) FROM app.pull_requests WHERE workflow_run_id=$1 AND state='merged'`, workflowID); merged != 0 {
		t.Fatal("a rejected run's pull request was merged")
	}
}

// A decision against a run that is not (or no longer) at the gate must fail
// loudly rather than silently doing nothing or acting on the wrong run.
func TestSubmitHumanDecisionRejectsADecisionAgainstARunNotAwaitingApproval(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := h.seedWaitingChecks(projectID, issueID) // still waiting_github_checks, not waiting_human

	if _, err := h.SubmitHumanDecision(h.adminContext(), &controlv1.SubmitHumanDecisionRequest{
		WorkflowRunId: workflowID, Decision: "approved",
	}); status.Code(err) != codes.FailedPrecondition {
		t.Fatalf("err = %v, want FailedPrecondition", err)
	}
}

// Before this, every GitHub error deliverWorkflow/observeWorkflow saw -- a
// rate limit, a DNS blip, a 502 -- funnelled straight to blockExternal, which
// is terminal: it releases the project lock and parks the issue. A single
// transient GitHub failure must instead leave the run exactly where it was so
// the next retry (a recovery sweep re-driving 'delivering', or the next
// observer tick for 'waiting_github_checks') can simply try again.
func TestDeliveryRetriesATransientGitHubFailureInsteadOfBlocking(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := h.seedDelivering(projectID, issueID)

	fake := &sequencedGitHub{errs: []error{errors.New("gh pr list: HTTP 502: Bad Gateway (HTTP 502)")}}
	h.github = fake

	if err := h.deliverWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("deliverWorkflow: %v", err)
	}
	if state := h.runState(workflowID); state.status != "delivering" {
		t.Fatalf("status = %q, want delivering: a transient failure must not block the run", state.status)
	}
	if attempts := h.scalar(`SELECT delivery_attempts FROM app.workflow_runs WHERE id=$1`, workflowID); attempts != 1 {
		t.Fatalf("delivery_attempts = %d, want 1", attempts)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 1 {
		t.Fatal("a transient failure released the project lock")
	}

	// The next retry succeeds, and the run should both progress and have its
	// attempt count reset -- it is no longer failing.
	if err := h.deliverWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("deliverWorkflow retry: %v", err)
	}
	if state := h.runState(workflowID); state.status != "waiting_github_checks" {
		t.Fatalf("status = %q, want waiting_github_checks once the retry succeeds", state.status)
	}
	if attempts := h.scalar(`SELECT delivery_attempts FROM app.workflow_runs WHERE id=$1`, workflowID); attempts != 0 {
		t.Fatalf("delivery_attempts = %d, want reset to 0 once delivery succeeded", attempts)
	}
}

// A terminal GitHub failure -- a 404, bad credentials, anything not
// recognised as transient -- must still block immediately, with no change
// from blockExternal's pre-existing behaviour: no retry budget spent, the
// project lock released, and the issue parked for a human.
func TestDeliveryBlocksImmediatelyOnATerminalGitHubFailure(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := h.seedDelivering(projectID, issueID)

	h.github = &sequencedGitHub{errs: []error{errors.New(`gh pr list: exit status 1: {"message":"Not Found"} gh: Not Found (HTTP 404)`)}}

	// blockExternal itself succeeds (it commits the run to 'blocked'), so
	// deliverWorkflow returns nil here just like it does for a successfully
	// delivered run -- the assertion is on the run's resulting state, not on
	// deliverWorkflow's return value.
	if err := h.deliverWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("deliverWorkflow: %v", err)
	}
	if state := h.runState(workflowID); state.status != "blocked" || state.blocking == "" {
		t.Fatalf("state = %+v, want blocked with a reason", state)
	}
	if attempts := h.scalar(`SELECT delivery_attempts FROM app.workflow_runs WHERE id=$1`, workflowID); attempts != 0 {
		t.Fatalf("delivery_attempts = %d, want 0: a terminal failure must not consume the retry budget", attempts)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("a terminal failure kept the project lock")
	}
	if h.schedulable(issueID) {
		t.Fatal("a terminal failure left the issue schedulable instead of parking it")
	}
}

// The retry budget is not unbounded: a run stuck behind an unbroken run of
// transient failures (a mis-scoped token returning 503s forever, say) must
// eventually fall through to blockExternal too, the same way abandonedChecks
// bounds a check wait GitHub never resolves.
func TestDeliveryBlocksAfterExhaustingItsRetryBudget(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := h.seedDelivering(projectID, issueID)

	h.github = &sequencedGitHub{alwaysErr: errors.New("gh pr list: HTTP 503: Service Unavailable (HTTP 503)")}

	for attempt := 1; attempt <= maxDeliveryAttempts; attempt++ {
		if err := h.deliverWorkflow(context.Background(), workflowID); err != nil {
			t.Fatalf("attempt %d: deliverWorkflow: %v", attempt, err)
		}
		if state := h.runState(workflowID); state.status != "delivering" {
			t.Fatalf("attempt %d: status = %q, want delivering (still under budget)", attempt, state.status)
		}
	}
	if attempts := h.scalar(`SELECT delivery_attempts FROM app.workflow_runs WHERE id=$1`, workflowID); attempts != maxDeliveryAttempts {
		t.Fatalf("delivery_attempts = %d, want %d", attempts, maxDeliveryAttempts)
	}

	// One more attempt exceeds the bound and must finally block. As with the
	// terminal case, blockExternal succeeding means deliverWorkflow itself
	// returns nil -- the assertion is on the resulting status.
	if err := h.deliverWorkflow(context.Background(), workflowID); err != nil {
		t.Fatalf("deliverWorkflow: %v", err)
	}
	if state := h.runState(workflowID); state.status != "blocked" {
		t.Fatalf("status = %q, want blocked once the retry budget is exhausted", state.status)
	}
}

// A run mid-delivery has done real, unfinished work: before StatusDelivering
// existed it would have been sitting at 'completed', the same value a merged
// run ends at, and GetSchedulerMetrics's `status NOT IN (terminalStatusList)`
// predicate would have wrongly read it as finished.
func TestSchedulerMetricsCountsADeliveringRunAsActive(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	workflowID := newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'delivering','delivering')`,
		workflowID, projectID, issueID, "thread-"+workflowID)

	snapshot, err := h.readSchedulerSnapshot(context.Background())
	if err != nil {
		t.Fatalf("readSchedulerSnapshot: %v", err)
	}
	if snapshot.activeWorkflows != 1 {
		t.Fatalf("activeWorkflows = %d, want 1: a run mid-delivery must count as active", snapshot.activeWorkflows)
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
			// The label-driven bit itself is untouched by either action (#268:
			// eligible is not a lifecycle flag any more) -- only whether the
			// issue is actually schedulable changes.
			if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 1 {
				t.Fatalf("%s changed the issue's label-driven eligible bit, which the tracker alone should own", action)
			}
			schedulable := h.schedulable(issueID)
			if action == "block" && schedulable {
				t.Fatal("blocking left the issue schedulable, so the scheduler restarts the work the operator stopped")
			}
			if action == "cancel" && !schedulable {
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
	// Both hang off a dedicated, already-opted-out issue rather than either of
	// the two counted above: ActiveWorkflows and ScheduledJobs read
	// app.workflow_runs/app.jobs directly with no issue join, but QueueDepth
	// now excludes an issue with a completed run of its own (#268) -- reusing
	// one of the counted issues here would make this fixture's "done" run
	// silently subtract from a count it is not testing.
	runsIssueID := newID()
	h.exec(`INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,url,state,eligible,external_created_at,external_updated_at) VALUES($1,$2,'github','44','44','Run host','https://example.test/issues/44','open',false,now(),now())`, runsIssueID, queuedProject)
	activeRun, doneRun := newID(), newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'preparing','preparing')`, activeRun, queuedProject, runsIssueID, "thread-"+activeRun)
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'completed','done')`, doneRun, queuedProject, runsIssueID, "thread-"+doneRun)
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

// GetSchedulerMetrics is where the console would read background-loop
// liveness (issue #278): it has to agree with the recorder that main.go wires
// every loop's outcome into, including reporting a loop unhealthy once it has
// gone stale and surfacing the error the loop last hit.
func TestGetSchedulerMetricsReportsLoopLiveness(t *testing.T) {
	h := newHarness(t)
	now := time.Date(2026, 8, 3, 12, 0, 0, 0, time.UTC)
	recorder := metrics.NewWithClock("", nil, func() time.Time { return now }).Recorder()
	h.Server.SetLoopRecorder(recorder)

	recorder.RecordLoopRun(metrics.LoopIssueSync, errors.New("gh: rate limited"))
	now = now.Add(time.Second)
	recorder.RecordLoopRun(metrics.LoopIssueSync, nil)
	// recovery_sweep never succeeds and its interval (30s) is well under this
	// gap, so it must come back unhealthy.
	now = now.Add(10 * time.Minute)

	resp, err := h.GetSchedulerMetrics(h.adminContext(), &controlv1.GetSchedulerMetricsRequest{})
	if err != nil {
		t.Fatalf("GetSchedulerMetrics: %v", err)
	}
	byName := make(map[string]*controlv1.LoopStatus, len(resp.GetLoopStatuses()))
	for _, status := range resp.GetLoopStatuses() {
		byName[status.GetName()] = status
	}

	issueSync, ok := byName[metrics.LoopIssueSync]
	if !ok {
		t.Fatal("GetSchedulerMetrics did not report issue_sync")
	}
	if !issueSync.GetHealthy() {
		t.Errorf("issue_sync reported unhealthy right after a fresh success: %+v", issueSync)
	}
	if issueSync.GetLastError() != "gh: rate limited" {
		t.Errorf("issue_sync LastError = %q, want the earlier failure preserved", issueSync.GetLastError())
	}
	if issueSync.GetLastSuccessAt() == "" {
		t.Error("issue_sync LastSuccessAt is empty after a recorded success")
	}

	recoverySweep, ok := byName[metrics.LoopRecoverySweep]
	if !ok {
		t.Fatal("GetSchedulerMetrics did not report recovery_sweep")
	}
	if recoverySweep.GetHealthy() {
		t.Errorf("recovery_sweep reported healthy after never succeeding for 10 minutes: %+v", recoverySweep)
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
	if h.schedulable(issueID) {
		t.Fatal("a blocked run left its issue schedulable, so the scheduler re-dispatches the same block forever")
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
	if !h.schedulable(issueID) {
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

// Agent prose reaching a text column: PostgreSQL used to reject a NUL byte
// outright, failing the whole terminal event with it -- the run would keep
// its project lock and never end (#302). sanitizeEventPayload now rewrites a
// NUL to a visible placeholder before the payload reaches either the
// event's own ::jsonb column or agentBlockReason, so it is exercised here
// beside the other hostile prose this payload already carries.
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
		// An ANSI introducer and a NUL byte, neither of which may survive
		// into an operator-facing column: jsonb stores the former happily
		// but only sanitizeEventPayload keeps the latter from failing the
		// event outright (#302).
		"summary":       "credential missing\x1b[31m and more prose\x00 trailing",
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
	if !strings.Contains(state.blocking, "credential missing") || !strings.Contains(state.blocking, "trailing") {
		t.Fatalf("blocking_reason = %q lost the agent's words around the sanitized NUL byte", state.blocking)
	}
	// The remaining-work list is the half an operator acts on, so it has to
	// survive the bound rather than be crowded out by prose, and the list it
	// opens has to close.
	if !strings.Contains(state.blocking, "\u6f22") || !strings.HasSuffix(state.blocking, ")") {
		t.Fatalf("blocking_reason = %q dropped the remaining work or left its list unclosed", state.blocking)
	}
}

// TestNULByteInALogEventPayloadIsStoredNotRejected pins #302 against a
// non-terminal event: the runner forwards payload["result"] verbatim, and an
// agent that writes a NUL anywhere into .loop/result.json used to abort the
// jsonb INSERT and fail delivery outright, before this fix. The NUL sits
// nested inside an object and an array, not just a top-level field, matching
// how an arbitrary agent result document is actually shaped.
func TestNULByteInALogEventPayloadIsStoredNotRejected(t *testing.T) {
	h := newHarness(t)
	h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	nul := literalUnicodeEscape("0000")
	payload := `{"result":{"raw":"agent output with a stray` + nul + `byte","steps":["ok",{"note":"nested` + nul + `NUL"}]}}`

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "log", PayloadJson: payload,
	}); err != nil {
		t.Fatalf("persistExecutionEvent with a NUL byte in the payload: %v", err)
	}

	var stored []byte
	if err := h.pool.QueryRow(context.Background(), `SELECT payload FROM app.workflow_events WHERE workflow_run_id=$1 AND event_type='log'`, workflowID).Scan(&stored); err != nil {
		t.Fatalf("the event was not retrievable: %v", err)
	}
	if strings.ContainsRune(string(stored), 0) {
		t.Fatalf("stored payload %s still carries a raw NUL byte", stored)
	}
	if !strings.Contains(string(stored), "agent output with a stray") || !strings.Contains(string(stored), "byte") {
		t.Fatalf("stored payload %s lost content around the sanitized NUL", stored)
	}
}

// TestNULByteInATerminalEventPayloadStillReachesTerminalStatus pins #302's
// worst case: a NUL byte anywhere in a terminal event's payload must not
// strand the run. Before this fix, the jsonb INSERT for the event row failed
// before SetWorkflowTerminalStatus and DeleteProjectLockByWorkflow ever ran,
// leaving the run without a terminal status, the project lock held forever,
// and the issue permanently unschedulable.
func TestNULByteInATerminalEventPayloadStillReachesTerminalStatus(t *testing.T) {
	h := newHarness(t)
	_, issueID := h.project()
	runnerID := h.runner()
	jobID, workflowID := h.runJob(runnerID)

	nul := literalUnicodeEscape("0000")
	payload := `{"status":"failed","exitCode":1,"error":"agent exited 1","result":{"raw":"crash trace with a stray` + nul + `byte"}}`

	if err := h.persistExecutionEvent(context.Background(), runnerID, &runnerv1.ExecutionEvent{
		JobId: jobID, LeaseGeneration: h.leaseGeneration(jobID), EventSequence: 1, Type: "failed", PayloadJson: payload,
	}); err != nil {
		t.Fatalf("persistExecutionEvent with a NUL byte in a terminal payload: %v", err)
	}

	state := h.runState(workflowID)
	if state.status != "failed" {
		t.Fatalf("status = %q, want failed: a NUL byte in the payload must not strand the run with no terminal status", state.status)
	}
	if locks := h.scalar(`SELECT COUNT(*) FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); locks != 0 {
		t.Fatal("a terminal event with a NUL byte in its payload left the project lock behind, wedging the project")
	}
	if h.schedulable(issueID) {
		t.Fatal("a terminal event with a NUL byte in its payload left the issue schedulable")
	}
}

// Only a `failed` event is read for a block declaration. The guard matters
// because `completed` is the delivery path: persistExecutionEvent moves the
// run to `delivering` and deliverWorkflow then opens the pull request under
// `WHERE id=$1 AND status='delivering'`, so a `completed` event diverted to
// `blocked` would lose a delivered branch, and a `cancelled` one reached no
// outcome of its own to declare. Neither carries the flag today — the runner
// only sets it beside a `failed` event — so this pins the guard rather than
// any current runner behaviour.
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
// job — while the run is still "offered", say — in which case there is no
// runner to notify. That must stay a no-op rather than an error or a panic.
func TestCancelWithoutAJobDoesNotErrorOrNotifyAnyone(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	runnerID := h.runner()
	outbound := h.outboundChannel(runnerID)
	workflowID := newID()
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase,branch_name) VALUES($1,$2,$3,$4,'offered','offered','agent/'||$4)`, workflowID, projectID, issueID, workflowID)

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

// 021_workflow_run_status_check.sql is the database's half of #265's typed
// Status vocabulary: every value in knownStatuses must still be accepted, and
// nothing outside it may ever reach the column again, from any writer,
// present or future.
func TestWorkflowRunStatusCheckConstraintMatchesKnownStatuses(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()

	for status := range knownStatuses {
		workflowID := newID()
		h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,$5,$5)`,
			workflowID, projectID, issueID, "thread-"+workflowID, status.String())
	}

	workflowID := newID()
	_, err := h.pool.Exec(context.Background(),
		`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'implementing','implementing')`,
		workflowID, projectID, issueID, "thread-"+workflowID)
	if err == nil {
		t.Fatal("inserting workflow_runs.status='implementing' succeeded; workflow_runs_status_is_known should have rejected it")
	}
	if !strings.Contains(err.Error(), "workflow_runs_status_is_known") {
		t.Fatalf("insert of an unknown status failed with %v, want the workflow_runs_status_is_known constraint", err)
	}
}

// labelStub is a GitHub whose ListIssues result is driven by a mutable label
// set, so a test can simulate an operator editing the tracker between sync
// passes the same way TestSyncHonoursTrackerLabelEditsAfterARunExists does.
type labelStub struct {
	stubGitHub
	labels []string
	// createdAt/updatedAt default to time.Now() on every call (the zero value
	// of each), which is what the existing label-edit tests want: each sync
	// pass is a distinct point in time regardless of the tracker row's own
	// timestamps. A test asserting on whether a pass counts as a no-op (see
	// TestUpsertIssueSkipsWritingWhenNothingChanged) sets these to a fixed
	// instant instead, matching a real tracker reporting the same
	// external_updated_at across polls when nothing actually changed.
	createdAt, updatedAt time.Time
}

func (s *labelStub) ListIssues(context.Context, string, string) ([]githubIssue, error) {
	priority, eligible := issuePriority(s.labels)
	createdAt, updatedAt := s.createdAt, s.updatedAt
	if createdAt.IsZero() {
		createdAt = time.Now()
	}
	if updatedAt.IsZero() {
		updatedAt = time.Now()
	}
	return []githubIssue{{
		ExternalID: "42", Title: "Fix scheduler", URL: "https://example.test/issues/42",
		Labels: s.labels, CreatedAt: createdAt, UpdatedAt: updatedAt,
		Priority: priority, Eligible: eligible, State: "open",
	}}, nil
}

// #268: app.issues.eligible used to be recomputed from the tracker's labels
// only until an issue had its first workflow run -- from then on the sync
// upsert's ON CONFLICT preserved whatever the orchestrator's own lifecycle
// had last written, so removing agent:ready or adding agent:blocked on the
// tracker silently stopped doing anything. This pins the fix: eligible is
// once again written unconditionally from the labels on every sync, and it
// is the scheduler's own app.workflow_runs join (not a frozen label
// snapshot) that keeps a failed run's issue from being immediately
// redispatched until RetryWorkflow supersedes it.
func TestSyncHonoursTrackerLabelEditsAfterARunExists(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	stub := &labelStub{labels: []string{"agent:ready"}}
	h.github = stub
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
	if h.schedulable(issueID) {
		t.Fatal("test setup: the failed run should already exclude the issue")
	}

	// Scenario 1: removing agent:ready after a run exists must actually take
	// effect on the tracker's own bit, not be silently swallowed.
	stub.labels = nil
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject: %v", err)
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 0 {
		t.Fatal("removing agent:ready after a run exists did not clear the label-driven eligible bit")
	}

	// Scenario 2: adding agent:blocked after a run exists must also take
	// effect.
	stub.labels = []string{"agent:ready", "agent:blocked"}
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject: %v", err)
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 0 {
		t.Fatal("adding agent:blocked after a run exists did not clear the label-driven eligible bit")
	}

	// Restoring agent:ready brings the label bit back, but must not by
	// itself reopen a failed run's issue: only RetryWorkflow does that.
	stub.labels = []string{"agent:ready"}
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject: %v", err)
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 1 {
		t.Fatal("sync did not restore the label-driven eligible bit once agent:ready came back")
	}
	if h.schedulable(issueID) {
		t.Fatal("restoring agent:ready alone reopened a failed run's issue; only RetryWorkflow should")
	}

	// Scenario 3: retry is what actually reopens it, and the issue's own
	// eligible bit (already true from the label) needs nothing further
	// written to it.
	if _, err := h.RetryWorkflow(h.adminContext(), &controlv1.RetryWorkflowRequest{WorkflowRunId: workflowID}); err != nil {
		t.Fatalf("RetryWorkflow: %v", err)
	}
	if !h.schedulable(issueID) {
		t.Fatal("retry did not make the issue schedulable again")
	}
}

// stateStub is a GitHub whose ListIssues result reports a single issue whose
// tracker state is driven by a mutable field, so a test can simulate an
// issue being closed on GitHub between two sync passes.
type stateStub struct {
	stubGitHub
	state string
}

func (s *stateStub) ListIssues(context.Context, string, string) ([]githubIssue, error) {
	return []githubIssue{{
		ExternalID: "42", Title: "Fix scheduler", URL: "https://example.test/issues/42",
		Labels: []string{"agent:ready"}, CreatedAt: time.Now(), UpdatedAt: time.Now(),
		Priority: 0, Eligible: s.state == "open", State: s.state,
	}}, nil
}

// TestSyncReconcilesAnIssueClosedOnGitHub pins the fix for the bug where
// ListIssues only ever fetched --state open: a closed issue never came back
// from GitHub at all, so app.issues.state stayed 'open' (and the issue
// stayed schedulable) forever. Now that ListIssues fetches --state all, a
// sync pass must flip the row to state='closed', clear eligible, and the
// scheduler's own predicate (h.schedulable, mirroring ListQueueEntries and
// ClaimSchedulableIssue) must actually exclude it -- not just have some
// column flipped that nothing reads.
func TestSyncReconcilesAnIssueClosedOnGitHub(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	stub := &stateStub{state: "open"}
	h.github = stub

	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject: %v", err)
	}
	if !h.schedulable(issueID) {
		t.Fatal("test setup: the open issue should be schedulable before it closes")
	}

	stub.state = "closed"
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject: %v", err)
	}
	if state := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND state='closed'`, issueID); state != 1 {
		t.Fatal("closing the issue on GitHub did not reconcile app.issues.state to 'closed'")
	}
	if eligible := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE id=$1 AND eligible`, issueID); eligible != 0 {
		t.Fatal("closing the issue on GitHub did not clear the eligible bit")
	}
	if h.schedulable(issueID) {
		t.Fatal("an issue closed on GitHub is still schedulable after sync reconciled it")
	}

	// Reopening it on GitHub must bring it back, proving this is a live
	// reconciliation and not a one-way trip.
	stub.state = "open"
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject: %v", err)
	}
	if !h.schedulable(issueID) {
		t.Fatal("reopening the issue on GitHub did not make it schedulable again")
	}
}

// TestUpsertIssueSkipsWritingWhenNothingChanged pins #290's sync-upsert cost:
// UpsertIssue's ON CONFLICT DO UPDATE used to rewrite every issue row --
// full raw_snapshot JSONB included -- on every single sync pass even when
// GitHub reported nothing new, which is the steady-state case for almost
// every pass on almost every project. Postgres does not expose "was this row
// written" directly, but xmin changes on (and only on) an actual tuple
// write, so comparing it across passes is a direct, precise proxy: unchanged
// xmin *is* zero writes, not merely "looks unchanged" from the columns a
// SELECT can see.
func TestUpsertIssueSkipsWritingWhenNothingChanged(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()
	// Fixed CreatedAt/UpdatedAt, unlike labelStub's time.Now() on every call:
	// a real tracker reports the same external_updated_at across polls when
	// nothing changed, and the guard is meant to key off exactly that, so a
	// stub whose timestamps drift on every call would make every pass look
	// like a real change regardless of the fix.
	fixedTime := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	stub := &labelStub{labels: []string{"agent:ready"}, createdAt: fixedTime, updatedAt: fixedTime}
	h.github = stub

	xmin := func() uint32 {
		t.Helper()
		var x uint32
		if err := h.pool.QueryRow(context.Background(), `SELECT xmin::text::bigint FROM app.issues WHERE id=$1`, issueID).Scan(&x); err != nil {
			t.Fatalf("read xmin: %v", err)
		}
		return x
	}

	// The seeded issue (h.project) does not match labelStub's row exactly, so
	// this first sync is expected to write once and establish a baseline xmin.
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject (baseline): %v", err)
	}
	baseline := xmin()

	// Two consecutive no-op passes: GitHub reports the exact same issue both
	// times, nothing on the tracker moved.
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject (no-op #1): %v", err)
	}
	if after := xmin(); after != baseline {
		t.Fatalf("xmin changed from %d to %d on a no-op sync pass; the DO UPDATE guard did not skip the write", baseline, after)
	}
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject (no-op #2): %v", err)
	}
	if after := xmin(); after != baseline {
		t.Fatalf("xmin changed from %d to %d on a second consecutive no-op sync pass; the DO UPDATE guard did not skip the write", baseline, after)
	}

	// A real change (a new label flipping priority/eligible) must still write.
	stub.labels = nil
	if err := h.syncProject(context.Background(), projectID, "https://github.com/acme/demo.git"); err != nil {
		t.Fatalf("syncProject (real change): %v", err)
	}
	if after := xmin(); after == baseline {
		t.Fatal("xmin did not change after a real label edit; the guard is over-suppressing writes, not just no-ops")
	}
}

// countingTracer counts every query issued through the pool it is attached
// to, so TestListWorkflowsIsBoundedAndBatched can prove the N+1 is gone by
// counting round trips rather than by reading the code and assuming.
type countingTracer struct{ queries int64 }

func (c *countingTracer) TraceQueryStart(ctx context.Context, _ *pgx.Conn, _ pgx.TraceQueryStartData) context.Context {
	atomic.AddInt64(&c.queries, 1)
	return ctx
}

func (c *countingTracer) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

// ListWorkflows used to run one query listing every workflow_runs row ever
// created, then one more per row to fetch its detail -- O(all workflow runs)
// in both rows and queries, degrading with everything that had ever run.
// This seeds well past the fixed cap and proves both halves of the fix: the
// response is capped, and the number of queries issued does not grow with
// the number of rows in the table.
func TestListWorkflowsIsBoundedAndBatched(t *testing.T) {
	h := newHarness(t)
	projectID, issueID := h.project()

	total := listWorkflowsLimit + 20
	ids := make([]string, total)
	threads := make([]string, total)
	for i := range ids {
		ids[i] = newID()
		threads[i] = "thread-" + ids[i]
	}
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase)
		SELECT unnest($1::uuid[]), $2::uuid, $3::uuid, unnest($4::text[]), 'offered', 'offered'`,
		ids, projectID, issueID, threads)

	url := os.Getenv("LOOP_TEST_DATABASE_URL")
	cfg, err := pgxpool.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	tracer := &countingTracer{}
	cfg.ConnConfig.Tracer = tracer
	tracedPool, err := pgxpool.NewWithConfig(context.Background(), cfg)
	if err != nil {
		t.Fatal(err)
	}
	defer tracedPool.Close()
	traced, err := NewWithGitHub(tracedPool, "test", stubGitHub{})
	if err != nil {
		t.Fatal(err)
	}

	before := atomic.LoadInt64(&tracer.queries)
	resp, err := traced.ListWorkflows(h.adminContext(), &controlv1.ListWorkflowsRequest{})
	if err != nil {
		t.Fatalf("ListWorkflows: %v", err)
	}
	issued := atomic.LoadInt64(&tracer.queries) - before

	if len(resp.Workflows) != listWorkflowsLimit {
		t.Fatalf("ListWorkflows returned %d workflows with %d runs seeded, want the %d cap", len(resp.Workflows), total, listWorkflowsLimit)
	}
	// requireActor's session lookup plus the single ListWorkflowsPage query is
	// 2; the old shape issued 1+total (well past 500 here), so a generous
	// constant ceiling still catches a regression back to per-row queries.
	if issued > 5 {
		t.Fatalf("ListWorkflows issued %d queries for %d workflow runs, want a small constant regardless of row count", issued, total)
	}
}

// tlsPeerContext attaches credentials.TLSInfo to the context the way a real
// TLS-terminated gRPC connection would, which is what secureContext checks
// before StoreJobSecret/ResolveJobSecret will run at all.
func tlsPeerContext(ctx context.Context) context.Context {
	return peer.NewContext(ctx, &peer.Peer{AuthInfo: credentials.TLSInfo{}})
}

// TestStoreJobSecretWritesAnAuditEntry pins #286: StoreJobSecret is the one
// credential mutation initiated by a runner rather than an admin, and it used
// to leave no trail at all in app.audit_events. The runner id is recorded as
// the actor, distinguishing this rotation from the admin-driven
// project.credential.set/clear entries credentials.go already writes.
func TestStoreJobSecretWritesAnAuditEntry(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")

	projectID, _ := h.project()
	runnerID := h.runner()
	h.exec(`INSERT INTO app.runner_credentials(id,runner_id,credential_hash) VALUES($1,$2,$3)`, newID(), runnerID, hashSecret("runner-credential"))
	// StoreJobSecret only ever rotates an existing credential (its query is an
	// UPDATE, not an upsert), so a row must already exist for the kind.
	h.exec(`INSERT INTO app.project_credentials(project_id,kind,ciphertext,nonce) VALUES($1,'agent:OPENROUTER_API_KEY','\x00'::bytea,'\x00'::bytea)`, projectID)
	jobID, _ := h.runJob(runnerID)
	var generation int64
	if err := h.pool.QueryRow(context.Background(), `SELECT lease_generation FROM app.jobs WHERE id=$1`, jobID).Scan(&generation); err != nil {
		t.Fatal(err)
	}

	resp, err := h.StoreJobSecret(tlsPeerContext(context.Background()), &runnerv1.StoreJobSecretRequest{
		RunnerId:        runnerID,
		JobId:           jobID,
		Credential:      "runner-credential",
		LeaseGeneration: generation,
		Name:            "OPENROUTER_API_KEY",
		Value:           "rotated-secret-value",
	})
	if err != nil {
		t.Fatalf("StoreJobSecret: %v", err)
	}
	if !resp.GetStored() {
		t.Fatal("StoreJobSecret reported the credential was not stored")
	}

	var actorID, action, targetType, targetID string
	if err := h.pool.QueryRow(context.Background(),
		`SELECT actor_id, action, target_type, target_id FROM app.audit_events WHERE action = 'project.credential.rotated'`,
	).Scan(&actorID, &action, &targetType, &targetID); err != nil {
		t.Fatalf("no audit entry was recorded for StoreJobSecret: %v", err)
	}
	if actorID != runnerID {
		t.Fatalf("audit actor_id = %q, want the rotating runner %q", actorID, runnerID)
	}
	if targetType != "project" || targetID != projectID {
		t.Fatalf("audit target = (%q,%q), want (project,%q)", targetType, targetID, projectID)
	}
}

// TestStoreJobSecretRecordsNoAuditEntryWhenNothingWasStored guards the audit
// entry against the same false-positive it would otherwise inherit from
// rowsAffected==0: rotating a credential kind the project never configured is
// still accepted by the RPC (Stored=false) but must not claim a rotation that
// did not happen.
func TestStoreJobSecretRecordsNoAuditEntryWhenNothingWasStored(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")

	_, _ = h.project()
	runnerID := h.runner()
	h.exec(`INSERT INTO app.runner_credentials(id,runner_id,credential_hash) VALUES($1,$2,$3)`, newID(), runnerID, hashSecret("runner-credential"))
	jobID, _ := h.runJob(runnerID)
	var generation int64
	if err := h.pool.QueryRow(context.Background(), `SELECT lease_generation FROM app.jobs WHERE id=$1`, jobID).Scan(&generation); err != nil {
		t.Fatal(err)
	}

	resp, err := h.StoreJobSecret(tlsPeerContext(context.Background()), &runnerv1.StoreJobSecretRequest{
		RunnerId:        runnerID,
		JobId:           jobID,
		Credential:      "runner-credential",
		LeaseGeneration: generation,
		Name:            "OPENROUTER_API_KEY",
		Value:           "rotated-secret-value",
	})
	if err != nil {
		t.Fatalf("StoreJobSecret: %v", err)
	}
	if resp.GetStored() {
		t.Fatal("StoreJobSecret reported the credential was stored, but no row existed to rotate")
	}

	var count int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM app.audit_events WHERE action = 'project.credential.rotated'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("audit_events has %d project.credential.rotated entries, want 0 when nothing was stored", count)
	}
}

// TestRunnerCapacityAllowsMultipleConcurrentJobs pins #272: RegisterRunner
// validated and stored app.runners.capacity, but ClaimSchedulableIssue
// excluded a runner the instant it held any in-flight job at all, so a
// runner registering capacity 3 could never be offered more than one job. A
// single project's own project_locks entry already limits it to one active
// workflow run at a time (TestOnlyOneWorkflowRunsPerProject), so this seeds
// three separate projects to isolate the runner's capacity as the only
// remaining limit.
func TestRunnerCapacityAllowsMultipleConcurrentJobs(t *testing.T) {
	h := newHarness(t)
	runnerID := h.runnerWithCapacity(3)
	for range 4 {
		h.project()
	}

	scheduled := 0
	for range 4 {
		ok, err := h.ScheduleOnce(context.Background())
		if err != nil {
			t.Fatalf("ScheduleOnce: %v", err)
		}
		if !ok {
			break
		}
		scheduled++
	}
	if scheduled != 3 {
		t.Fatalf("ScheduleOnce claimed %d jobs for a runner with capacity 3, want exactly 3", scheduled)
	}
	if jobs := h.scalar(`SELECT COUNT(*) FROM app.jobs WHERE runner_id=$1 AND status IN ('offered','preparing','running')`, runnerID); jobs != 3 {
		t.Fatalf("in-flight jobs for the runner = %d, want 3", jobs)
	}

	// A fourth eligible issue exists (on its own, unlocked project), but the
	// runner is now at capacity: nothing more should be offered to it.
	if ok, err := h.ScheduleOnce(context.Background()); err != nil {
		t.Fatalf("ScheduleOnce: %v", err)
	} else if ok {
		t.Fatal("ScheduleOnce claimed a fourth job for a runner already holding its registered capacity of 3")
	}
}

// TestDrainingScheduleRespectsCapacityAcrossMultipleRunners pins the #290
// drain-loop fix from the other side of TestRunnerCapacityAllowsMultipleConcurrentJobs:
// that test pins one runner with capacity 3 against 4 issues; this one pins N
// separate runners each capacity 1 against M > N eligible issues (one project
// per issue, so a project's own single-active-workflow lock never becomes
// the limiting factor). Calling ScheduleOnce in a tight loop -- exactly what
// main.go's drainSchedule now does within a single tick instead of waiting a
// full second per job -- must place exactly N jobs, one per runner, and never
// more: capacity is enforced by ClaimSchedulableIssue's own WHERE clause, ANDed
// fresh on every call, not by anything the caller's loop shape does.
func TestDrainingScheduleRespectsCapacityAcrossMultipleRunners(t *testing.T) {
	h := newHarness(t)
	const runnerCount = 3
	const issueCount = 7 // > runnerCount, so the queue outlives every runner's capacity
	runnerIDs := make(map[string]bool, runnerCount)
	for range runnerCount {
		runnerIDs[h.runnerWithCapacity(1)] = true
	}
	for range issueCount {
		h.project()
	}

	scheduled := 0
	for range issueCount + 1 { // one extra iteration to prove it stops, not just slows
		ok, err := h.ScheduleOnce(context.Background())
		if err != nil {
			t.Fatalf("ScheduleOnce: %v", err)
		}
		if !ok {
			break
		}
		scheduled++
	}
	if scheduled != runnerCount {
		t.Fatalf("draining scheduled %d jobs across %d runners each at capacity 1, want exactly %d (one per runner), not %d issues' worth", scheduled, runnerCount, runnerCount, issueCount)
	}

	jobsByRunner := h.scalar(`SELECT COUNT(DISTINCT runner_id) FROM app.jobs WHERE status IN ('offered','preparing','running')`)
	if jobsByRunner != runnerCount {
		t.Fatalf("in-flight jobs are spread across %d runners, want all %d runners to have exactly one each", jobsByRunner, runnerCount)
	}
	for runnerID := range runnerIDs {
		if inFlight := h.scalar(`SELECT COUNT(*) FROM app.jobs WHERE runner_id=$1 AND status IN ('offered','preparing','running')`, runnerID); inFlight != 1 {
			t.Fatalf("runner %s has %d in-flight jobs, want exactly 1 (its registered capacity)", runnerID, inFlight)
		}
	}
	if remaining := h.scalar(`SELECT COUNT(*) FROM app.issues WHERE eligible AND state='open' AND NOT EXISTS (SELECT 1 FROM app.workflow_runs w WHERE w.issue_id = app.issues.id)`); remaining != issueCount-runnerCount {
		t.Fatalf("unclaimed eligible issues = %d, want %d (the %d issues beyond runner capacity, left queued rather than dropped)", remaining, issueCount-runnerCount, issueCount-runnerCount)
	}
}

// failingGitHub makes ListIssues fail every call, for tests driving
// recordSyncFailure/backoff without touching a real `gh` process.
type failingGitHub struct {
	stubGitHub
	err error
}

func (g *failingGitHub) ListIssues(context.Context, string, string) ([]githubIssue, error) {
	return nil, g.err
}

// TestRecordSyncFailureSetsExponentialBackoff pins #272: recordSyncFailure
// used to only increment consecutive_failures, leaving next_retry_at NULL
// forever even though IssueSyncStatus reports it and the console renders it.
// The formula is 1 minute * 2^consecutive_failures, capped at 1 hour; this
// checks the first two failures land inside the expected window and that a
// long failure streak is still capped rather than overflowing.
func TestRecordSyncFailureSetsExponentialBackoff(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	ctx := context.Background()

	if err := h.recordSyncFailure(ctx, projectID, errors.New("boom")); err != nil {
		t.Fatalf("recordSyncFailure: %v", err)
	}
	var failures int
	var nextRetry time.Time
	if err := h.pool.QueryRow(ctx, `SELECT consecutive_failures, next_retry_at FROM app.issue_sync_state WHERE project_id=$1`, projectID).Scan(&failures, &nextRetry); err != nil {
		t.Fatal(err)
	}
	if failures != 1 {
		t.Fatalf("consecutive_failures = %d, want 1", failures)
	}
	wait := time.Until(nextRetry)
	if wait < 90*time.Second || wait > 150*time.Second {
		t.Fatalf("first failure's next_retry_at is %s away, want ~2 minutes (2^1 minutes)", wait)
	}

	// Drive it through many more failures than the exponent could ever
	// tolerate uncapped (2^30 minutes would overflow long before this) and
	// confirm it still lands within the 1 hour ceiling instead of erroring
	// out or producing a nonsensical timestamp.
	for range 30 {
		if err := h.recordSyncFailure(ctx, projectID, errors.New("still failing")); err != nil {
			t.Fatalf("recordSyncFailure: %v", err)
		}
	}
	if err := h.pool.QueryRow(ctx, `SELECT consecutive_failures, next_retry_at FROM app.issue_sync_state WHERE project_id=$1`, projectID).Scan(&failures, &nextRetry); err != nil {
		t.Fatal(err)
	}
	if failures != 31 {
		t.Fatalf("consecutive_failures = %d, want 31", failures)
	}
	wait = time.Until(nextRetry)
	if wait <= 0 || wait > time.Hour+time.Minute {
		t.Fatalf("next_retry_at after 31 failures is %s away, want capped at ~1 hour", wait)
	}

	// A subsequent success must clear both fields, so a recovered project is
	// not skipped by a stale future timestamp forever.
	if err := h.queries.UpsertIssueSyncStateSuccess(ctx, projectID); err != nil {
		t.Fatalf("UpsertIssueSyncStateSuccess: %v", err)
	}
	var nextRetryValid bool
	if err := h.pool.QueryRow(ctx, `SELECT consecutive_failures, next_retry_at IS NOT NULL FROM app.issue_sync_state WHERE project_id=$1`, projectID).Scan(&failures, &nextRetryValid); err != nil {
		t.Fatal(err)
	}
	if failures != 0 || nextRetryValid {
		t.Fatalf("after success: consecutive_failures=%d, next_retry_at set=%v, want 0 and NULL", failures, nextRetryValid)
	}
}

// TestSyncProjectsSkipsAProjectStillInsideItsBackoffWindow pins #272: nothing
// acted on next_retry_at, so SyncProjects (the unattended sync loop) retried
// a project with, say, a revoked token or a deleted repository at full rate
// forever. This drives a real failure through syncProject to set the
// backoff, then confirms a subsequent SyncProjects pass does not call
// ListIssues again while still inside that window, and does once the window
// is forced into the past (standing in for it elapsing).
func TestSyncProjectsSkipsAProjectStillInsideItsBackoffWindow(t *testing.T) {
	h := newHarness(t)
	projectID, _ := h.project()
	failing := &failingGitHub{err: errors.New("revoked token")}
	h.github = failing

	if err := h.SyncProjects(context.Background()); err == nil {
		t.Fatal("SyncProjects: want an error surfaced from the failing project")
	}
	var nextRetryValid bool
	if err := h.pool.QueryRow(context.Background(), `SELECT next_retry_at IS NOT NULL FROM app.issue_sync_state WHERE project_id=$1`, projectID).Scan(&nextRetryValid); err != nil {
		t.Fatal(err)
	}
	if !nextRetryValid {
		t.Fatal("test setup: the first failure should have set next_retry_at")
	}

	// Swap in a GitHub that would succeed, to prove a skip (not another
	// failure) is why ListIssues is not called again.
	succeeding := &countingGitHub{}
	h.github = succeeding
	if err := h.SyncProjects(context.Background()); err != nil {
		t.Fatalf("SyncProjects: %v", err)
	}
	if succeeding.calls != 0 {
		t.Fatalf("SyncProjects called ListIssues %d times for a project still inside its backoff window, want 0", succeeding.calls)
	}

	// Force the window into the past and confirm the project is picked back
	// up on the very next pass.
	h.exec(`UPDATE app.issue_sync_state SET next_retry_at = now() - interval '1 minute' WHERE project_id=$1`, projectID)
	if err := h.SyncProjects(context.Background()); err != nil {
		t.Fatalf("SyncProjects: %v", err)
	}
	if succeeding.calls != 1 {
		t.Fatalf("SyncProjects called ListIssues %d times once the backoff window elapsed, want 1", succeeding.calls)
	}
}

// countingGitHub counts ListIssues calls while always succeeding (with no
// issues), so a test can prove a sync pass was skipped rather than merely
// having nothing to do.
type countingGitHub struct {
	stubGitHub
	calls int
}

func (g *countingGitHub) ListIssues(context.Context, string, string) ([]githubIssue, error) {
	g.calls++
	return nil, nil
}

// TestSyncNowReportsAPlainErrorMessageNotAWrappedGRPCStatus pins #287:
// SyncNow's per-project result used to carry err.Error() verbatim, which
// leaks the gRPC status wrapper onto a console-facing field for anything
// that had already been through databaseError/configurationError, and here
// pins the plain (never-wrapped) side of that same fix end to end through
// the real RPC handler: syncProject's ListIssues failure is an ordinary Go
// error, so it must reach ProjectSyncResult.Error unchanged, never dressed up
// as "rpc error: code = ... desc = ...".
func TestSyncNowReportsAPlainErrorMessageNotAWrappedGRPCStatus(t *testing.T) {
	h := newHarness(t)
	h.project()
	h.github = &failingGitHub{err: errors.New("revoked token")}

	resp, err := h.SyncNow(h.adminContext(), &controlv1.SyncNowRequest{})
	if err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
	if len(resp.Results) != 1 {
		t.Fatalf("SyncNow results = %d, want 1", len(resp.Results))
	}
	const want = "revoked token"
	if got := resp.Results[0].Error; got != want {
		t.Fatalf("SyncNow result error = %q, want %q", got, want)
	}
	if strings.Contains(resp.Results[0].Error, "rpc error") {
		t.Fatalf("SyncNow result error leaked a gRPC status wrapper: %q", resp.Results[0].Error)
	}
}

// ===========================================================================
// Auth and authorization (#273)
// ===========================================================================

// userSession creates a user with the given role (`admin` or `viewer`) and a
// live session, returning a context carrying its session and CSRF metadata
// alongside the raw tokens themselves, so a test can also assemble a
// deliberately wrong pair from them.
func (h *harness) userSession(role string) (ctx context.Context, session, csrf string) {
	h.t.Helper()
	userID, session, csrf := newID(), newID(), newID()
	hash, err := passwordHash("Correct-Horse-1")
	if err != nil {
		h.t.Fatal(err)
	}
	h.exec(`INSERT INTO app.users(id,username,password_hash,role) VALUES($1,$2,$3,$4)`, userID, "user-"+userID[:8], hash, role)
	h.exec(`INSERT INTO app.user_sessions(id,user_id,token_hash,csrf_token_hash,expires_at,last_seen_at) VALUES($1,$2,$3,$4,now()+interval '1 hour',now())`, newID(), userID, hashSecret(session), hashSecret(csrf))
	return metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, session, csrfHeader, csrf)), session, csrf
}

func TestRequireActorRejectsNoSessionMetadataAtAll(t *testing.T) {
	h := newHarness(t)
	if _, err := h.requireActor(context.Background(), false); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("requireActor with no session metadata = %v, want Unauthenticated", err)
	}
}

func TestRequireActorRejectsAnUnknownSessionToken(t *testing.T) {
	h := newHarness(t)
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, "no-such-session-token", csrfHeader, "whatever"))
	if _, err := h.requireActor(ctx, false); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("requireActor with an unknown session token = %v, want Unauthenticated", err)
	}
}

// A session past its expires_at must be rejected exactly like one that never
// existed -- GetSessionActor's own WHERE clause is the actual enforcement,
// and this is what pins that the predicate keeps checking it.
func TestRequireActorRejectsAnExpiredSession(t *testing.T) {
	h := newHarness(t)
	userID, session, csrf := newID(), newID(), newID()
	hash, err := passwordHash("Correct-Horse-1")
	if err != nil {
		t.Fatal(err)
	}
	h.exec(`INSERT INTO app.users(id,username,password_hash,role) VALUES($1,$2,$3,'admin')`, userID, "expired-"+userID[:8], hash)
	h.exec(`INSERT INTO app.user_sessions(id,user_id,token_hash,csrf_token_hash,expires_at,last_seen_at) VALUES($1,$2,$3,$4,now()-interval '1 minute',now())`, newID(), userID, hashSecret(session), hashSecret(csrf))
	ctx := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, session, csrfHeader, csrf))
	if _, err := h.requireActor(ctx, false); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("requireActor with an expired session = %v, want Unauthenticated", err)
	}
}

// This is the highest-value test in the whole issue, called out by name: the
// admin gate inside requireActor(ctx, true) is a single `role != "admin"`
// comparison, and flipping it to `role == "admin"` passed the entire existing
// suite. Driving both an admin and a non-admin session through the same
// mutation check and requiring opposite outcomes is what actually catches
// that inversion -- a test that only ever exercises the admin path would
// still pass after the flip.
func TestRequireMutationRequiresTheAdminRoleNotJustAnyActor(t *testing.T) {
	h := newHarness(t)

	viewerCtx, _, _ := h.userSession("viewer")
	if _, err := h.requireMutation(viewerCtx); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("requireMutation for a non-admin (viewer) actor = %v, want PermissionDenied", err)
	}

	adminCtx := h.adminContext()
	if _, err := h.requireMutation(adminCtx); err != nil {
		t.Fatalf("requireMutation for an admin actor = %v, want nil", err)
	}

	// The non-mutation path must not itself gate on role: a viewer can still
	// read.
	if _, err := h.requireActor(viewerCtx, false); err != nil {
		t.Fatalf("requireActor(mutation=false) for a viewer = %v, want nil", err)
	}
}

// requireActor's CSRF check is a constant-time comparison of hashed tokens,
// not the session lookup itself, so a valid session with either a wrong or a
// missing CSRF header must still be refused.
func TestRequireMutationRejectsAWrongOrMissingCSRFToken(t *testing.T) {
	h := newHarness(t)
	_, session, _ := h.userSession("admin")

	wrongCSRF := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, session, csrfHeader, "not-the-real-csrf-token"))
	if _, err := h.requireMutation(wrongCSRF); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("requireMutation with a wrong CSRF token = %v, want PermissionDenied", err)
	}

	noCSRF := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, session))
	if _, err := h.requireMutation(noCSRF); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("requireMutation with no CSRF header at all = %v, want PermissionDenied", err)
	}
}

// requireUserMutation backs UpdateAccount, every user's own self-service
// endpoint, and is deliberately weaker than requireMutation: it still
// enforces the session/CSRF pair like any mutation, but does not gate on the
// admin role at all. A viewer must be accepted (unlike requireMutation
// above), and a wrong CSRF token must still be refused.
func TestRequireUserMutationAcceptsANonAdminButStillEnforcesCSRF(t *testing.T) {
	h := newHarness(t)
	ctx, session, _ := h.userSession("viewer")

	if _, err := h.requireUserMutation(ctx); err != nil {
		t.Fatalf("requireUserMutation for a non-admin (viewer) actor = %v, want nil", err)
	}

	wrongCSRF := metadata.NewIncomingContext(context.Background(), metadata.Pairs(sessionHeader, session, csrfHeader, "not-the-real-csrf-token"))
	if _, err := h.requireUserMutation(wrongCSRF); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("requireUserMutation with a wrong CSRF token = %v, want PermissionDenied", err)
	}
}

// ===========================================================================
// The runner Connect stream (#273)
// ===========================================================================

// fakeRunnerStream is a minimal grpc.BidiStreamingServer[RunnerToOrchestrator,
// OrchestratorToRunner] driven entirely in-process, standing in for the
// simulatedRunner the issue asked about: Connect only ever calls Recv, Send,
// and Context on the stream it is handed, so a fake need only implement
// those (plus the rest of grpc.ServerStream, unused here) to drive the real
// handler directly, with no network or TLS involved.
type fakeRunnerStream struct {
	ctx        context.Context
	fromRunner chan *runnerv1.RunnerToOrchestrator
	streamErr  chan error
	toRunner   chan *runnerv1.OrchestratorToRunner
}

func newFakeRunnerStream(ctx context.Context) *fakeRunnerStream {
	return &fakeRunnerStream{
		ctx:        ctx,
		fromRunner: make(chan *runnerv1.RunnerToOrchestrator, 8),
		streamErr:  make(chan error, 1),
		toRunner:   make(chan *runnerv1.OrchestratorToRunner, 32),
	}
}

func (f *fakeRunnerStream) Recv() (*runnerv1.RunnerToOrchestrator, error) {
	select {
	case message := <-f.fromRunner:
		return message, nil
	case err := <-f.streamErr:
		return nil, err
	}
}

func (f *fakeRunnerStream) Send(message *runnerv1.OrchestratorToRunner) error {
	f.toRunner <- message
	return nil
}

func (f *fakeRunnerStream) SetHeader(metadata.MD) error  { return nil }
func (f *fakeRunnerStream) SendHeader(metadata.MD) error { return nil }
func (f *fakeRunnerStream) SetTrailer(metadata.MD)       {}
func (f *fakeRunnerStream) Context() context.Context     { return f.ctx }
func (f *fakeRunnerStream) SendMsg(any) error            { return nil }
func (f *fakeRunnerStream) RecvMsg(any) error            { return nil }

// send queues a message as if the runner had sent it.
func (f *fakeRunnerStream) send(message *runnerv1.RunnerToOrchestrator) { f.fromRunner <- message }

// close ends the stream the way a real one ends when the runner disconnects.
func (f *fakeRunnerStream) close(err error) { f.streamErr <- err }

func heartbeatMessage(runnerID, credential string) *runnerv1.RunnerToOrchestrator {
	return &runnerv1.RunnerToOrchestrator{
		RunnerId: runnerID, Credential: credential,
		Message: &runnerv1.RunnerToOrchestrator_Heartbeat{Heartbeat: &runnerv1.Heartbeat{Version: "1"}},
	}
}

// runnerCredential seeds a runner row and a matching runner_credentials row
// with a caller-known plaintext credential, without registering a control-
// stream session -- the Connect tests below drive that themselves.
func (h *harness) runnerCredential() (runnerID, credential string) {
	h.t.Helper()
	runnerID, credential = newID(), randomSecret()
	h.exec(`INSERT INTO app.runners(id,name,status,version,labels,last_seen_at) VALUES($1,$2,'offline','1','[]'::jsonb,now())`, runnerID, "runner-"+runnerID[:8])
	h.exec(`INSERT INTO app.runner_credentials(id,runner_id,credential_hash) VALUES($1,$2,$3)`, newID(), runnerID, hashSecret(credential))
	return runnerID, credential
}

// waitForConnectedRunner polls the server's live session set for runnerID, so
// a test driving Connect in a background goroutine can synchronize on the
// session actually being registered before acting as if it were.
func waitForConnectedRunner(t *testing.T, s *Server, runnerID string) {
	t.Helper()
	deadline := time.After(2 * time.Second)
	for {
		for _, connected := range s.connectedRunners() {
			if connected == runnerID {
				return
			}
		}
		select {
		case <-deadline:
			t.Fatalf("runner %s never registered a Connect session", runnerID)
		case <-time.After(10 * time.Millisecond):
		}
	}
}

// Connect must reject a runner ID/credential pair that does not authenticate,
// exactly like RegisterRunner's own credential check, and it must do so
// before ever registering a session.
func TestConnectRejectsAnInvalidCredential(t *testing.T) {
	h := newHarness(t)
	runnerID, _ := h.runnerCredential()

	stream := newFakeRunnerStream(context.Background())
	stream.send(heartbeatMessage(runnerID, "not-the-real-credential"))

	if err := h.Connect(stream); status.Code(err) != codes.Unauthenticated {
		t.Fatalf("Connect() with a bad credential = %v, want Unauthenticated", err)
	}
	for _, connected := range h.connectedRunners() {
		if connected == runnerID {
			t.Fatal("Connect registered a session for a runner that failed to authenticate")
		}
	}
}

// A second Connect stream for a runner ID that already holds one must be
// rejected outright, rather than silently replacing the first: that is what
// stops a reconnecting runner (or an attacker who obtained its credential)
// from displacing a live stream that is mid-lease.
func TestConnectRejectsADuplicateStreamForTheSameRunner(t *testing.T) {
	h := newHarness(t)
	runnerID, credential := h.runnerCredential()

	ctx1, cancel1 := context.WithCancel(context.Background())
	defer cancel1()
	stream1 := newFakeRunnerStream(ctx1)
	stream1.send(heartbeatMessage(runnerID, credential))
	done1 := make(chan error, 1)
	go func() { done1 <- h.Connect(stream1) }()
	waitForConnectedRunner(t, h.Server, runnerID)

	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()
	stream2 := newFakeRunnerStream(ctx2)
	stream2.send(heartbeatMessage(runnerID, credential))
	done2 := make(chan error, 1)
	go func() { done2 <- h.Connect(stream2) }()
	select {
	case err := <-done2:
		if status.Code(err) != codes.AlreadyExists {
			t.Fatalf("Connect() for a runner ID already holding a stream = %v, want AlreadyExists", err)
		}
	case <-time.After(2 * time.Second):
		// A regression here would not return with a competing error at all --
		// it would let the second stream's session register and then block
		// forever in Connect's ordinary send/receive loop, exactly like a
		// second, still-open connection. Timing out is that failure, not a
		// slow pass, so it is reported as one rather than left to hang.
		t.Fatal("Connect() for a duplicate runner ID never returned; the second stream was accepted instead of rejected")
	}

	cancel1()
	select {
	case <-done1:
	case <-time.After(2 * time.Second):
		t.Fatal("the first stream's Connect() never returned after its context was cancelled")
	}
}

// Every message after the first is re-authenticated, not just pinned to the
// runner ID the stream opened with: a later message naming a different
// runner, carrying no credential, or carrying the wrong one must all end the
// stream rather than being silently accepted as if it came from whoever
// authenticated first.
func TestConnectReAuthenticatesEveryMessageNotJustTheFirst(t *testing.T) {
	h := newHarness(t)
	runnerID, credential := h.runnerCredential()
	otherRunnerID, otherCredential := h.runnerCredential()

	for name, second := range map[string]*runnerv1.RunnerToOrchestrator{
		"a different runner ID (with that runner's own valid credential)": heartbeatMessage(otherRunnerID, otherCredential),
		"an empty credential":                           {RunnerId: runnerID, Credential: "", Message: heartbeatMessage(runnerID, credential).Message},
		"the wrong credential for the pinned runner ID": heartbeatMessage(runnerID, "not-the-real-credential"),
	} {
		t.Run(name, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			stream := newFakeRunnerStream(ctx)
			stream.send(heartbeatMessage(runnerID, credential))
			stream.send(second)

			done := make(chan error, 1)
			go func() { done <- h.Connect(stream) }()
			var err error
			select {
			case err = <-done:
			case <-time.After(2 * time.Second):
				// A regression here would not surface as a different error --
				// it would accept the unauthenticated message and simply keep
				// the stream open, so a hang is exactly the failure to catch.
				t.Fatal("Connect() never returned; a later message carrying " + name + " was accepted instead of rejected")
			}
			if status.Code(err) != codes.Unauthenticated {
				t.Fatalf("Connect() = %v, want Unauthenticated once a later message carried %s", err, name)
			}
			for _, connected := range h.connectedRunners() {
				if connected == runnerID {
					t.Fatal("the session was left registered after the stream ended on a re-auth failure")
				}
			}
		})
	}
}

// The outbound buffer per runner session is exactly 16 deep: the 17th queued
// message must be dropped (enqueue reports false) rather than block the
// caller or grow without bound, and freeing one slot must free capacity for
// exactly one more.
func TestEnqueueDropsOnceTheSixteenDeepBufferIsFull(t *testing.T) {
	h := newHarness(t)
	runnerID := newID()
	outbound := make(chan *runnerv1.OrchestratorToRunner, 16)
	if !h.addSession(runnerID, outbound) {
		t.Fatal("addSession rejected a fresh runner ID")
	}

	for i := range 16 {
		if !boundedEnqueue(t, h.Server, runnerID) {
			t.Fatalf("enqueue #%d was dropped before the buffer was full", i+1)
		}
	}
	if boundedEnqueue(t, h.Server, runnerID) {
		t.Fatal("enqueue succeeded past the 16-deep buffer's capacity; the 17th message should be dropped, not queued or blocked on")
	}

	<-outbound // free exactly one slot
	if !boundedEnqueue(t, h.Server, runnerID) {
		t.Fatal("enqueue was still dropped immediately after a slot was freed")
	}
	if boundedEnqueue(t, h.Server, runnerID) {
		t.Fatal("enqueue succeeded twice after only one slot was freed")
	}
}

// boundedEnqueue calls enqueue on its own goroutine with a hard timeout: a
// dropping enqueue must never block on a full channel send in the first
// place, so a regression back to an unconditional `outbound <- message`
// would otherwise hang this test for its full -timeout rather than fail
// promptly with a clear cause.
func boundedEnqueue(t *testing.T, s *Server, runnerID string) bool {
	t.Helper()
	done := make(chan bool, 1)
	go func() { done <- s.enqueue(runnerID, &runnerv1.OrchestratorToRunner{}) }()
	select {
	case ok := <-done:
		return ok
	case <-time.After(2 * time.Second):
		t.Fatal("enqueue blocked instead of returning immediately; the drop path is gone")
		return false
	}
}

// A reconnecting runner's predecessor session must not be able to evict the
// new one out from under it. The defer in Connect runs removeSession(id,
// itsOwnChannel) unconditionally on the way out, including when it returns
// long after a successor has already taken the runnerID's slot in the map;
// removeSession's own identity check (delete only if the map still holds
// exactly this channel) is what stops that stale deferred call from deleting
// the live session.
func TestRemoveSessionDoesNotEvictAReconnectedRunnersNewSession(t *testing.T) {
	h := newHarness(t)
	runnerID := newID()

	oldSession := make(chan *runnerv1.OrchestratorToRunner, 16)
	if !h.addSession(runnerID, oldSession) {
		t.Fatal("addSession rejected a fresh runner ID")
	}
	h.removeSession(runnerID, oldSession) // the old stream ends...
	newSession := make(chan *runnerv1.OrchestratorToRunner, 16)
	if !h.addSession(runnerID, newSession) {
		t.Fatal("addSession rejected the reconnecting runner's new session")
	}

	// ...and only now does the old handler's deferred removeSession(id,
	// oldSession) actually run, late, holding a reference to a channel that
	// is no longer in the map at all.
	h.removeSession(runnerID, oldSession)

	if got := h.outboundChannel(runnerID); got != newSession {
		t.Fatal("removeSession evicted the reconnected runner's live session using its predecessor's stale channel reference")
	}
}

// ===========================================================================
// Credential storage (#273)
// ===========================================================================

// SetProjectCredential must never write the plaintext value to
// app.project_credentials: the ciphertext column must differ from the
// plaintext, and decrypting it with the configured key must recover exactly
// the value that was set. A fresh nonce must also be generated on every
// write, even for the exact same plaintext -- reusing a nonce breaks AES-GCM's
// confidentiality guarantee outright.
func TestSetProjectCredentialStoresCiphertextWithAFreshNoncePerWrite(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	const plaintext = "super-secret-github-token-value"

	if _, err := h.SetProjectCredential(h.adminContext(), &controlv1.SetProjectCredentialRequest{
		ProjectId: projectID, Kind: "github_token", Value: plaintext,
	}); err != nil {
		t.Fatalf("SetProjectCredential: %v", err)
	}
	var firstCiphertext, firstNonce []byte
	if err := h.pool.QueryRow(context.Background(), `SELECT ciphertext, nonce FROM app.project_credentials WHERE project_id=$1 AND kind='github_token'`, projectID).Scan(&firstCiphertext, &firstNonce); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(firstCiphertext), plaintext) {
		t.Fatalf("app.project_credentials.ciphertext contains the plaintext value verbatim: %x", firstCiphertext)
	}
	aead, err := configuredCipher()
	if err != nil {
		t.Fatal(err)
	}
	decrypted, err := aead.Open(nil, firstNonce, firstCiphertext, nil)
	if err != nil || string(decrypted) != plaintext {
		t.Fatalf("decrypting the stored ciphertext = (%q, %v), want (%q, nil)", decrypted, err, plaintext)
	}

	// Setting the exact same plaintext again must produce a different nonce
	// and a different ciphertext, not reuse either.
	if _, err := h.SetProjectCredential(h.adminContext(), &controlv1.SetProjectCredentialRequest{
		ProjectId: projectID, Kind: "github_token", Value: plaintext,
	}); err != nil {
		t.Fatalf("SetProjectCredential (second write): %v", err)
	}
	var secondCiphertext, secondNonce []byte
	if err := h.pool.QueryRow(context.Background(), `SELECT ciphertext, nonce FROM app.project_credentials WHERE project_id=$1 AND kind='github_token'`, projectID).Scan(&secondCiphertext, &secondNonce); err != nil {
		t.Fatal(err)
	}
	if string(secondNonce) == string(firstNonce) {
		t.Fatal("writing the same credential twice reused the same AES-GCM nonce")
	}
	if string(secondCiphertext) == string(firstCiphertext) {
		t.Fatal("writing the same plaintext twice produced identical ciphertext, which a reused nonce would also do")
	}
}

// ResolveJobSecret is the read side of the same store: it must decrypt back
// to exactly the value SetProjectCredential encrypted, over the plaintext
// name a runner asks for.
func TestResolveJobSecretRoundTripsTheEncryptedValue(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	runnerID, credential := h.runnerCredential()
	if _, err := h.SetProjectCredential(h.adminContext(), &controlv1.SetProjectCredentialRequest{
		ProjectId: projectID, Kind: "github_token", Value: "ghp_the-real-secret-value",
	}); err != nil {
		t.Fatalf("SetProjectCredential: %v", err)
	}
	jobID, generation := h.seedFencedJob(runnerID, projectID)

	resp, err := h.ResolveJobSecret(tlsPeerContext(context.Background()), &runnerv1.ResolveJobSecretRequest{
		RunnerId: runnerID, JobId: jobID, Credential: credential, LeaseGeneration: generation, Name: "GITHUB_TOKEN",
	})
	if err != nil {
		t.Fatalf("ResolveJobSecret: %v", err)
	}
	if resp.GetValue() != "ghp_the-real-secret-value" {
		t.Fatalf("ResolveJobSecret returned %q, want the original plaintext", resp.GetValue())
	}
}

// seedFencedJob puts a runner in possession of a live, fenced job on
// projectID -- exactly what StoreJobSecret/ResolveJobSecret's fencedJobProject
// lookup requires -- without going through the full scheduler/runJob path
// those tests don't otherwise need.
func (h *harness) seedFencedJob(runnerID, projectID string) (jobID string, generation int64) {
	h.t.Helper()
	workflowID, issueID := newID(), newID()
	h.exec(`INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,url,state,eligible,external_created_at,external_updated_at) VALUES($1,$2,'github',$3,$3,'issue','https://example.test/issues/x','open',true,now(),now())`, issueID, projectID, issueID[:8])
	h.exec(`INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase) VALUES($1,$2,$3,$4,'preparing','preparing')`, workflowID, projectID, issueID, "thread-"+workflowID)
	jobID = newID()
	h.exec(`INSERT INTO app.jobs(id,workflow_run_id,project_id,runner_id,status,lease_generation,lease_expires_at) VALUES($1,$2,$3,$4,'running',1,now()+interval '1 hour')`, jobID, workflowID, projectID, runnerID)
	return jobID, 1
}

// StoreJobSecret's rotation gate is the only thing standing between a
// compromised runner and overwriting a project's github_token or ssh key: it
// accepts only names that resolve to an `agent:`-prefixed kind, rejecting
// github_token and ssh_private_key outright even though ResolveJobSecret (the
// read side) serves both.
func TestStoreJobSecretRejectsNonAgentCredentialKinds(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	runnerID, credential := h.runnerCredential()
	if _, err := h.SetProjectCredential(h.adminContext(), &controlv1.SetProjectCredentialRequest{
		ProjectId: projectID, Kind: "github_token", Value: "ghp_original-value",
	}); err != nil {
		t.Fatalf("SetProjectCredential: %v", err)
	}
	jobID, generation := h.seedFencedJob(runnerID, projectID)

	for _, name := range []string{"GITHUB_TOKEN", "GIT_SSH_KEY"} {
		t.Run(name, func(t *testing.T) {
			_, err := h.StoreJobSecret(tlsPeerContext(context.Background()), &runnerv1.StoreJobSecretRequest{
				RunnerId: runnerID, JobId: jobID, Credential: credential, LeaseGeneration: generation,
				Name: name, Value: "attacker-controlled-replacement",
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("StoreJobSecret(%s) = %v, want InvalidArgument: a runner must not be able to rotate a non-agent credential", name, err)
			}
		})
	}
	var stored string
	if err := h.pool.QueryRow(context.Background(), `SELECT ciphertext::text FROM app.project_credentials WHERE project_id=$1 AND kind='github_token'`, projectID).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	resp, err := h.ResolveJobSecret(tlsPeerContext(context.Background()), &runnerv1.ResolveJobSecretRequest{
		RunnerId: runnerID, JobId: jobID, Credential: credential, LeaseGeneration: generation, Name: "GITHUB_TOKEN",
	})
	if err != nil || resp.GetValue() != "ghp_original-value" {
		t.Fatalf("github_token = (%q, %v) after the rejected rotation attempts, want the untouched original", resp.GetValue(), err)
	}
}

// The positive case beside the rejection above: an agent: credential is
// exactly what the gate is meant to allow through.
func TestStoreJobSecretAllowsAnAgentCredentialKind(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	runnerID, credential := h.runnerCredential()
	h.exec(`INSERT INTO app.project_credentials(project_id,kind,ciphertext,nonce) VALUES($1,'agent:OPENROUTER_API_KEY','\x00'::bytea,'\x00'::bytea)`, projectID)
	jobID, generation := h.seedFencedJob(runnerID, projectID)

	resp, err := h.StoreJobSecret(tlsPeerContext(context.Background()), &runnerv1.StoreJobSecretRequest{
		RunnerId: runnerID, JobId: jobID, Credential: credential, LeaseGeneration: generation,
		Name: "OPENROUTER_API_KEY", Value: "new-rotated-value",
	})
	if err != nil {
		t.Fatalf("StoreJobSecret: %v", err)
	}
	if !resp.GetStored() {
		t.Fatal("StoreJobSecret reported the agent credential was not stored")
	}
	fetched, err := h.ResolveJobSecret(tlsPeerContext(context.Background()), &runnerv1.ResolveJobSecretRequest{
		RunnerId: runnerID, JobId: jobID, Credential: credential, LeaseGeneration: generation, Name: "OPENROUTER_API_KEY",
	})
	if err != nil || fetched.GetValue() != "new-rotated-value" {
		t.Fatalf("ResolveJobSecret after rotation = (%q, %v), want the rotated value", fetched.GetValue(), err)
	}
}

// ===========================================================================
// Registration tokens (#273)
// ===========================================================================

// A registration token is single-use: RegisterRunner consumes it inside the
// same transaction that creates the runner, and SelectValidRegistrationToken
// filters on used_at IS NULL, so a second registration attempt with the exact
// same token must be refused even though the first attempt succeeded.
func TestRegistrationTokenIsSingleUse(t *testing.T) {
	h := newHarness(t)
	token := h.createRegistrationToken(t, []string{"linux"})

	if _, err := h.RegisterRunner(context.Background(), &runnerv1.RegisterRunnerRequest{
		Name: "runner-one", Token: token, ProtocolVersion: "1.0", Labels: []string{"linux"},
	}); err != nil {
		t.Fatalf("first RegisterRunner: %v", err)
	}
	if _, err := h.RegisterRunner(context.Background(), &runnerv1.RegisterRunnerRequest{
		Name: "runner-two", Token: token, ProtocolVersion: "1.0", Labels: []string{"linux"},
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("second RegisterRunner with an already-used token = %v, want PermissionDenied", err)
	}
}

// An expired token must be refused even though it was never used.
func TestRegistrationTokenExpiryIsEnforced(t *testing.T) {
	h := newHarness(t)
	token := h.createRegistrationToken(t, []string{"linux"})
	h.exec(`UPDATE app.runner_registration_tokens SET expires_at=now()-interval '1 minute' WHERE token_hash=$1`, hashSecret(token))

	if _, err := h.RegisterRunner(context.Background(), &runnerv1.RegisterRunnerRequest{
		Name: "runner-one", Token: token, ProtocolVersion: "1.0", Labels: []string{"linux"},
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RegisterRunner with an expired token = %v, want PermissionDenied", err)
	}
}

// A token's allowed_labels is the ceiling a registering runner's requested
// labels must fit inside (allowed_labels @> requested), not an exact-match
// set and not a floor: a runner asking for a subset of what the token allows
// must succeed, and one asking for even one label outside that set must be
// refused, matching the issue's "label-subset matching" description exactly.
func TestRegistrationTokenLabelSubsetMatching(t *testing.T) {
	h := newHarness(t)

	subsetToken := h.createRegistrationToken(t, []string{"linux", "docker", "gpu"})
	if _, err := h.RegisterRunner(context.Background(), &runnerv1.RegisterRunnerRequest{
		Name: "runner-subset", Token: subsetToken, ProtocolVersion: "1.0", Labels: []string{"linux", "docker"},
	}); err != nil {
		t.Fatalf("RegisterRunner requesting a subset of the token's allowed labels: %v", err)
	}

	supersetToken := h.createRegistrationToken(t, []string{"linux"})
	if _, err := h.RegisterRunner(context.Background(), &runnerv1.RegisterRunnerRequest{
		Name: "runner-superset", Token: supersetToken, ProtocolVersion: "1.0", Labels: []string{"linux", "windows"},
	}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("RegisterRunner requesting a label (windows) outside the token's allowed set = %v, want PermissionDenied", err)
	}
}

// createRegistrationToken drives the real admin-facing RPC to create a
// registration token restricted to allowedLabels, returning the raw token a
// runner would present. Using CreateRunnerRegistrationToken rather than a raw
// insert also exercises that RPC as a side effect of every test above.
func (h *harness) createRegistrationToken(t *testing.T, allowedLabels []string) string {
	t.Helper()
	resp, err := h.CreateRunnerRegistrationToken(h.adminContext(), &controlv1.CreateRunnerRegistrationTokenRequest{AllowedLabels: allowedLabels})
	if err != nil {
		t.Fatalf("CreateRunnerRegistrationToken: %v", err)
	}
	return resp.GetToken()
}
