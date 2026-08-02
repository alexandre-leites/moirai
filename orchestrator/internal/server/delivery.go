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
	pr, err := s.github.FindOrCreatePR(ctx, repository, workflow.branch, workflow.defaultBranch, workflow.issueTitle, "Resolves #"+workflow.externalID)
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
	command, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status='waiting_github_checks',current_phase='waiting_github_checks',updated_at=now(),completed_at=NULL WHERE id=$1 AND status='completed'`, workflowID)
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
	rows, err := s.pool.Query(ctx, `SELECT wr.id::text FROM app.workflow_runs wr WHERE wr.status='waiting_github_checks' ORDER BY wr.updated_at,wr.id LIMIT 20`)
	if err != nil {
		return databaseError(err)
	}
	defer rows.Close()
	var workflows []string
	for rows.Next() {
		var workflowID string
		if err := rows.Scan(&workflowID); err != nil {
			return databaseError(err)
		}
		workflows = append(workflows, workflowID)
	}
	if err := rows.Err(); err != nil {
		return databaseError(err)
	}
	// Every workflow in the batch is attempted even if an earlier one fails.
	// The batch is ordered by updated_at, so returning on the first error let a
	// single persistently failing workflow sit at the head and starve every
	// other pull request waiting on its checks.
	var failures []error
	for _, workflowID := range workflows {
		if err := s.observeWorkflow(ctx, workflowID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
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
	checks, err := s.github.Checks(ctx, repository, workflow.prNumber)
	if err != nil {
		return s.blockExternal(ctx, workflowID, err)
	}
	// Merging is the exhaustive case, not the default one: a checkState this
	// switch does not recognise must never fall through into a squash merge.
	switch checks {
	case checksGreen:
	case checksFailed:
		return s.blockExternal(ctx, workflowID, errors.New("required GitHub checks failed"))
	case checksPending:
		return nil
	default:
		return nil
	}
	if err := s.github.MergeSquash(ctx, repository, workflow.prNumber); err != nil {
		return s.blockExternal(ctx, workflowID, err)
	}
	merged, err := s.github.Merged(ctx, repository, workflow.prNumber)
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
	command, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status='completed',current_phase='completed',completed_at=now(),updated_at=now() WHERE id=$1 AND status='waiting_github_checks'`, workflowID)
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
	reason := truncate("external delivery failed: "+cause.Error(), 1024)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	var projectID string
	err = tx.QueryRow(ctx, `UPDATE app.workflow_runs SET status='blocked',current_phase='blocked',blocking_reason=$2,terminal_reason=$2,completed_at=now(),updated_at=now() WHERE id=$1 AND status NOT IN ('blocked','failed','cancelled') RETURNING project_id::text`, workflowID, reason).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, projectID, workflowID); err != nil {
		return databaseError(err)
	}
	// Same reason as a failed execution: without this the scheduler re-creates
	// the run from the still-eligible issue on the next tick and delivery fails
	// again, in a loop. A manual retry is what reopens it.
	if err := parkIssue(ctx, tx, workflowID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'delivery.failed','error',$2::jsonb)`, workflowID, fmt.Sprintf(`{"reason":%q}`, reason)); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}
