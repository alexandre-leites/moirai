package server

import (
	"context"
	"errors"
	"fmt"

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
		return s.blockExternal(ctx, workflowID, err)
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

// terminateWorkflow moves a run to a terminal state and releases the project
// lock it was holding, recording the cause in the same transaction. Delivery
// failures and abandoned leases are the same event as far as the run is
// concerned — the work stopped and the project has to be freed — and writing
// them separately had already produced two different sets of columns for the
// same outcome.
//
// A run that is already terminal is left alone and reported as success: it has
// nothing left to release.
//
// Nothing here writes to the issue any more (see #268): whether its work
// stays excluded from scheduling is entirely a function of the state this run
// itself lands on, via the app.workflow_runs join ListQueueEntries and
// ClaimSchedulableIssue both run. StatusFailed and StatusBlocked exclude it
// until RetryWorkflow supersedes this run; StatusCancelled does not exclude
// it at all, which is why reclaimUnansweredOffers uses that state for an
// offer nobody answered — nothing was spent, so that work should simply be
// offered again rather than waiting for a human.
func (s *Server) terminateWorkflow(ctx context.Context, workflowID string, state Status, eventType, cause string) error {
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
		return s.blockExternal(ctx, workflowID, err)
	}
	if checks == checksFailed {
		return s.blockExternal(ctx, workflowID, errors.New("required GitHub checks failed"))
	}
	if checks != checksGreen {
		return nil // pending, or a state this code does not recognise: never merge
	}
	if err := s.github.MergeSquash(ctx, workflow.projectID, repository, workflow.prNumber); err != nil {
		return s.blockExternal(ctx, workflowID, err)
	}
	merged, err := s.github.Merged(ctx, workflow.projectID, repository, workflow.prNumber)
	if err != nil {
		return s.blockExternal(ctx, workflowID, err)
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
	// Nothing needs writing to the issue itself: MarkWorkflowCompleted above
	// already landed this run on 'completed', which the scheduler's
	// app.workflow_runs join (ListQueueEntries, ClaimSchedulableIssue)
	// excludes permanently — there is no retry path off StatusCompleted (see
	// genuinelyTerminalStatuses), so nothing ever supersedes it. See #268.
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
	return s.terminateWorkflow(ctx, workflowID, StatusBlocked, "delivery.failed", "external delivery failed: "+cause.Error())
}
