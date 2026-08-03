package server

import (
	"context"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
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
	if _, err := tx.Exec(ctx, `INSERT INTO app.pull_requests(id,workflow_run_id,provider,external_id,url,head_commit,state) VALUES($1,$2,'github',$3,$4,$5,$6) ON CONFLICT(workflow_run_id) DO UPDATE SET external_id=EXCLUDED.external_id,url=EXCLUDED.url,head_commit=EXCLUDED.head_commit,state=EXCLUDED.state`, newID(), workflowID, pr.Number, pr.URL, pr.HeadSHA, pr.State); err != nil {
		return databaseError(err)
	}
	command, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status=`+qWaitingGithubChecks+`,current_phase=`+qWaitingGithubChecks+`,updated_at=now(),completed_at=NULL WHERE id=$1 AND status=`+qCompleted, workflowID)
	if err != nil {
		return databaseError(err)
	}
	if command.RowsAffected() != 1 {
		return errors.New("workflow delivery is no longer available")
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'pull_request.created','info',$2::jsonb)`, workflowID, fmt.Sprintf(`{"number":%q,"url":%q}`, pr.Number, pr.URL)); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) ObserveWorkflows(ctx context.Context) error {
	return s.eachWorkflow(ctx, `SELECT wr.id::text FROM app.workflow_runs wr WHERE wr.status=`+qWaitingGithubChecks+` ORDER BY wr.updated_at,wr.id LIMIT 20`, s.observeWorkflow)
}

// eachWorkflow drains a batch of workflow identifiers and runs `do` against
// every one of them, collecting failures rather than stopping at the first.
// These batches are ordered oldest-first, so returning early would let one
// persistently failing workflow sit at the head and starve the rest — and in
// the recovery sweep that head-of-line row is one holding a project lock, so a
// single bad row would block lock release for every other project.
//
// The cursor is drained before any work starts: `do` makes its own database
// calls and shells out to GitHub, and holding the cursor's pooled connection
// across that is how a small pool starves itself.
func (s *Server) eachWorkflow(ctx context.Context, query string, do func(context.Context, string) error) error {
	rows, err := s.pool.Query(ctx, query)
	if err != nil {
		return databaseError(err)
	}
	workflows, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		return databaseError(err)
	}
	var failures []error
	for _, workflowID := range workflows {
		if err := do(ctx, workflowID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
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
	var projectID string
	err = tx.QueryRow(ctx, `UPDATE app.workflow_runs SET status=$2,current_phase=$2,blocking_reason=$3,terminal_reason=$3,completed_at=now(),updated_at=now() WHERE id=$1 AND status NOT IN (`+genuinelyTerminalStatusList+`) RETURNING project_id::text`, workflowID, state.String(), reason).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, projectID, workflowID); err != nil {
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
	if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,$2,'error',$3::jsonb)`, workflowID, eventType, fmt.Sprintf(`{"reason":%q}`, reason)); err != nil {
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
	command, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status=`+qCompleted+`,current_phase=`+qCompleted+`,completed_at=now(),updated_at=now() WHERE id=$1 AND status=`+qWaitingGithubChecks, workflowID)
	if err != nil {
		return databaseError(err)
	}
	if command.RowsAffected() != 1 {
		return nil
	}
	if _, err := tx.Exec(ctx, `UPDATE app.pull_requests SET state='merged',merged_at=now() WHERE workflow_run_id=$1`, workflowID); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.issues SET eligible=false WHERE id=$1`, workflow.issueID); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, workflow.projectID, workflowID); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'pull_request.merged','info','{}'::jsonb)`, workflowID); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) deliveryWorkflow(ctx context.Context, workflowID string, requirePR bool) (deliveryWorkflow, error) {
	workflow := deliveryWorkflow{id: workflowID}
	var prNumber *string
	err := s.pool.QueryRow(ctx, `SELECT wr.project_id::text,wr.issue_id::text,i.external_id,i.title,i.body,COALESCE(p.repository_url,''),p.default_branch,wr.branch_name,pr.external_id FROM app.workflow_runs wr JOIN app.issues i ON i.id=wr.issue_id JOIN app.projects p ON p.id=wr.project_id LEFT JOIN app.pull_requests pr ON pr.workflow_run_id=wr.id WHERE wr.id=$1`, workflowID).Scan(&workflow.projectID, &workflow.issueID, &workflow.externalID, &workflow.issueTitle, &workflow.issueBody, &workflow.repositoryURL, &workflow.defaultBranch, &workflow.branch, &prNumber)
	if errors.Is(err, pgx.ErrNoRows) {
		return deliveryWorkflow{}, errors.New("workflow is unknown")
	}
	if err != nil {
		return deliveryWorkflow{}, databaseError(err)
	}
	if requirePR && prNumber == nil {
		return deliveryWorkflow{}, errors.New("workflow pull request is missing")
	}
	if prNumber != nil {
		workflow.prNumber = *prNumber
	}
	if workflow.repositoryURL == "" || workflow.defaultBranch == "" || workflow.branch == "" {
		return deliveryWorkflow{}, errors.New("workflow delivery configuration is invalid")
	}
	return workflow, nil
}

func (s *Server) blockExternal(ctx context.Context, workflowID string, cause error) error {
	return s.terminateWorkflow(ctx, workflowID, StatusBlocked, "delivery.failed", "external delivery failed: "+cause.Error(), true)
}
