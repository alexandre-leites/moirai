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
}

func (s *labelStub) ListIssues(context.Context, string, string) ([]githubIssue, error) {
	priority, eligible := issuePriority(s.labels)
	return []githubIssue{{
		ExternalID: "42", Title: "Fix scheduler", URL: "https://example.test/issues/42",
		Labels: s.labels, CreatedAt: time.Now(), UpdatedAt: time.Now(),
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
