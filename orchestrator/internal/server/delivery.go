package server

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5"

	"github.com/loop-engineering/orchestrator/internal/db"
)

type deliveryWorkflow struct {
	id            string
	projectID     string
	issueID       string
	externalID    string
	issueTitle    string
	issueBody     string
	repositoryURL string
	defaultBranch string
	branch        string
	prNumber      string
}

func (s *Server) deliverWorkflow(ctx context.Context, workflowID string) error {
	workflow, err := s.deliveryWorkflow(ctx, workflowID, false)
	if err != nil {
		return s.blockExternal(ctx, workflowID, err)
	}
	repository, err := repositoryRef(workflow.repositoryURL)
	if err != nil {
		return s.blockExternal(ctx, workflowID, err)
	}
	pr, err := s.github.FindOrCreatePR(ctx, workflow.projectID, repository, workflow.branch, workflow.defaultBranch, workflow.issueTitle, "Resolves #"+workflow.externalID)
	if err != nil {
		return s.blockOrRetryExternal(ctx, workflowID, err)
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.UpsertPullRequest(ctx, db.UpsertPullRequestParams{
		ID:            newID(),
		WorkflowRunID: workflowID,
		ExternalID:    pr.Number,
		Url:           pr.URL,
		HeadCommit:    pr.HeadSHA,
		State:         pr.State,
	}); err != nil {
		return databaseError(err)
	}
	rowsAffected, err := queries.MarkWorkflowDelivered(ctx, workflowID)
	if err != nil {
		return databaseError(err)
	}
	if rowsAffected != 1 {
		return errors.New("workflow delivery is no longer available")
	}
	if err := queries.InsertPullRequestCreatedEvent(ctx, db.InsertPullRequestCreatedEventParams{
		WorkflowRunID: workflowID,
		Payload:       []byte(fmt.Sprintf(`{"number":%q,"url":%q}`, pr.Number, pr.URL)),
	}); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) ObserveWorkflows(ctx context.Context) error {
	workflowIDs, err := s.queries.SelectWaitingGithubChecksWorkflows(ctx)
	if err != nil {
		return databaseError(err)
	}
	return s.eachWorkflowID(ctx, workflowIDs, s.observeWorkflow)
}

// terminateWorkflow moves a run to a terminal state, releases the project lock
// it was holding and parks its issue, recording the cause in the same
// transaction. Delivery failures and abandoned leases are the same event as far
// as the run is concerned — the work stopped and the project has to be freed —
// and writing them separately had already produced two different sets of
// columns for the same outcome.
//
// A run that is already terminal is left alone and reported as success: it has
// nothing left to release.
//
// park says whether the issue should stop being scheduled. It is false only
// when nothing was spent — an offer nobody answered ran no execution, so that
// work should simply be offered again rather than waiting for a human.
func (s *Server) terminateWorkflow(ctx context.Context, workflowID string, state Status, eventType, cause string, park bool) error {
	reason := truncate(cause, 1024)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	projectID, err := queries.TerminateWorkflowRun(ctx, db.TerminateWorkflowRunParams{
		Status: state.String(),
		Reason: pgText(reason),
		ID:     workflowID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return databaseError(err)
	}
	if err := queries.DeleteProjectLock(ctx, db.DeleteProjectLockParams{ProjectID: projectID, WorkflowRunID: workflowID}); err != nil {
		return databaseError(err)
	}
	// Without this the scheduler re-creates the run from the still-eligible
	// issue on the next tick and it fails again, in a loop. A manual retry is
	// what reopens it.
	if park {
		if err := parkIssue(ctx, tx, workflowID); err != nil {
			return err
		}
	}
	if err := queries.InsertWorkflowTerminationEvent(ctx, db.InsertWorkflowTerminationEventParams{
		WorkflowRunID: workflowID,
		EventType:     eventType,
		Payload:       []byte(fmt.Sprintf(`{"reason":%q}`, reason)),
	}); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) observeWorkflow(ctx context.Context, workflowID string) error {
	workflow, err := s.deliveryWorkflow(ctx, workflowID, true)
	if err != nil {
		return s.blockExternal(ctx, workflowID, err)
	}
	repository, err := repositoryRef(workflow.repositoryURL)
	if err != nil {
		return s.blockExternal(ctx, workflowID, err)
	}
	checks, err := s.github.Checks(ctx, workflow.projectID, repository, workflow.prNumber)
	if err != nil {
		return s.blockOrRetryExternal(ctx, workflowID, err)
	}
	if checks == checksFailed {
		return s.blockExternal(ctx, workflowID, errors.New("required GitHub checks failed"))
	}
	if checks != checksGreen {
		return nil // pending, or a state this code does not recognise: never merge
	}
	if err := s.github.MergeSquash(ctx, workflow.projectID, repository, workflow.prNumber); err != nil {
		return s.blockOrRetryExternal(ctx, workflowID, err)
	}
	merged, err := s.github.Merged(ctx, workflow.projectID, repository, workflow.prNumber)
	if err != nil {
		return s.blockOrRetryExternal(ctx, workflowID, err)
	}
	if !merged {
		return s.blockExternal(ctx, workflowID, errors.New("GitHub did not confirm pull request merge"))
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	rowsAffected, err := queries.MarkWorkflowCompleted(ctx, workflowID)
	if err != nil {
		return databaseError(err)
	}
	if rowsAffected != 1 {
		return nil
	}
	if err := queries.MarkPullRequestMerged(ctx, workflowID); err != nil {
		return databaseError(err)
	}
	if err := queries.MarkIssueIneligible(ctx, workflow.issueID); err != nil {
		return databaseError(err)
	}
	if err := queries.DeleteProjectLock(ctx, db.DeleteProjectLockParams{ProjectID: workflow.projectID, WorkflowRunID: workflowID}); err != nil {
		return databaseError(err)
	}
	if err := queries.InsertPullRequestMergedEvent(ctx, workflowID); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) deliveryWorkflow(ctx context.Context, workflowID string, requirePR bool) (deliveryWorkflow, error) {
	row, err := s.queries.GetDeliveryWorkflow(ctx, workflowID)
	if errors.Is(err, pgx.ErrNoRows) {
		return deliveryWorkflow{}, errors.New("workflow is unknown")
	}
	if err != nil {
		return deliveryWorkflow{}, databaseError(err)
	}
	workflow := deliveryWorkflow{
		id:            workflowID,
		projectID:     row.ProjectID,
		issueID:       row.IssueID,
		externalID:    row.ExternalID,
		issueTitle:    row.Title,
		issueBody:     row.Body,
		repositoryURL: row.RepositoryUrl,
		defaultBranch: row.DefaultBranch,
		branch:        row.BranchName,
	}
	if requirePR && !row.PrExternalID.Valid {
		return deliveryWorkflow{}, errors.New("workflow pull request is missing")
	}
	if row.PrExternalID.Valid {
		workflow.prNumber = row.PrExternalID.String
	}
	if workflow.repositoryURL == "" || workflow.defaultBranch == "" || workflow.branch == "" {
		return deliveryWorkflow{}, errors.New("workflow delivery configuration is invalid")
	}
	return workflow, nil
}

func (s *Server) blockExternal(ctx context.Context, workflowID string, cause error) error {
	return s.terminateWorkflow(ctx, workflowID, StatusBlocked, "delivery.failed", "external delivery failed: "+cause.Error(), true)
}

// maxDeliveryAttempts bounds how many consecutive transient GitHub failures
// blockOrRetryExternal will absorb for a single run before giving up and
// falling through to blockExternal. abandonedChecks (recovery.go) already
// bounds the case where GitHub simply never reports a result at all; this is
// the analogous ceiling for the case where every call to GitHub is itself
// failing. Without it a run stuck behind (say) a mis-scoped token would retry
// forever: RecordTransientDeliveryFailure advances updated_at on every
// attempt, which would keep it from ever aging into abandonedChecks' own
// window.
const maxDeliveryAttempts = 10

// blockOrRetryExternal is what deliverWorkflow and observeWorkflow call for
// every error the GitHub interface itself returns. A transient failure --
// a network blip, a 5xx, a rate limit, gh's own invocation timing out -- says
// nothing about whether the run's actual work (opening or merging the pull
// request) is bad, and retrying it a little later plausibly succeeds with
// nothing changed on GitHub's side or this run's. blockExternal is terminal:
// it parks the run's issue and waits for a human, so it stays reserved for a
// real 404/permission/merge-conflict failure, or for a transient failure that
// has already exhausted its attempt budget.
//
// The run's status is left untouched on every attempt below the bound: the
// next observer tick (for observeWorkflow, still 'waiting_github_checks') or
// recovery sweep (for deliverWorkflow, still 'delivering') calls back into the
// same code path and simply retries the same GitHub call.
func (s *Server) blockOrRetryExternal(ctx context.Context, workflowID string, cause error) error {
	if !isTransientGitHubError(cause) {
		return s.blockExternal(ctx, workflowID, cause)
	}
	attempts, err := s.queries.RecordTransientDeliveryFailure(ctx, workflowID)
	if err != nil {
		return databaseError(err)
	}
	if attempts <= maxDeliveryAttempts {
		return nil
	}
	return s.blockExternal(ctx, workflowID, fmt.Errorf("%d consecutive transient GitHub failures, giving up: %w", attempts, cause))
}

// httpStatusPattern picks the HTTP status code out of gh's own error text --
// see TestIsTransientGitHubError for the exact shape, verified against a real
// `gh` invocation rather than guessed: a 404 renders as "gh: Not Found
// (HTTP 404)" and a 401 as "gh: Bad credentials (HTTP 401)".
var httpStatusPattern = regexp.MustCompile(`http (\d{3})`)

// isTransientGitHubError classifies an error returned by the GitHub interface
// (github.go) as transient -- worth leaving deliverWorkflow/observeWorkflow's
// run alone to retry -- or terminal, meaning blockExternal should run
// immediately. It only recognises shapes actually observed from the
// gh-CLI-backed implementation:
//
//   - context.DeadlineExceeded/context.Canceled: githubTimeout expired on a gh
//     invocation, or the caller's own context was canceled -- see Run's
//     ctx.Err() substitution in github.go for why this is reachable via
//     errors.Is instead of a bare "signal: killed" exec error.
//   - "error connecting to ... check your internet connection": gh's own
//     message for a DNS/network failure reaching api.github.com, confirmed by
//     running `gh api` against an unresolvable host.
//   - "rate limit" / "abuse detection" / "secondary rate limit": GitHub's
//     primary and secondary rate-limit responses, which gh passes through in
//     the message text alongside their HTTP status.
//   - an "HTTP 5xx" or "HTTP 429" in the message: gh renders every REST/API
//     error as "<message> (HTTP <code>)" -- confirmed against real 404 and 401
//     responses below -- and 5xx/429 are GitHub's own server-side/throttling
//     codes, as opposed to 404/401/403 (not-found, bad credentials, permission
//     denied), which stay terminal.
//
// Anything else -- a merge conflict, an unrecognised gh failure -- is left
// terminal, matching blockExternal's pre-existing behaviour for those cases.
func isTransientGitHubError(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled) {
		return true
	}
	message := strings.ToLower(err.Error())
	for _, phrase := range []string{
		"rate limit",
		"abuse detection",
		"secondary rate limit",
		"error connecting to",
		"dial tcp",
		"no such host",
		"connection reset",
		"connection refused",
		"i/o timeout",
		"tls handshake timeout",
		"temporary failure in name resolution",
	} {
		if strings.Contains(message, phrase) {
			return true
		}
	}
	if match := httpStatusPattern.FindStringSubmatch(message); match != nil {
		if code, convErr := strconv.Atoi(match[1]); convErr == nil && (code == 429 || code >= 500) {
			return true
		}
	}
	return false
}
