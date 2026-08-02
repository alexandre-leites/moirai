package server

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

// RecoverOnce reconciles state that no live request will ever come back to
// finish. Every guard in this package is written as `lease_expires_at>now()` or
// `status='waiting_github_checks'`, which correctly refuses stale work but
// leaves the stale rows themselves untouched. A project lock is held by exactly
// one workflow run at a time, so any run that stops making progress while
// holding one takes its whole project down with it until something clears it —
// and a runner that is killed mid-job, or an orchestrator restarted between
// committing a completion and delivering it, both do exactly that.
//
// It runs on a timer and at startup, and is written so that running it twice
// changes nothing the first pass already settled.
func (s *Server) RecoverOnce(ctx context.Context) error {
	if err := s.markDisconnectedRunnersOffline(ctx); err != nil {
		return err
	}
	if err := s.reclaimExpiredLeases(ctx); err != nil {
		return err
	}
	return s.resumeStrandedDeliveries(ctx)
}

// markDisconnectedRunnersOffline clears the online flag for runners this
// process is not holding a stream for. Nothing else ever writes 'offline'
// outside an operator revoke, so after a restart the console otherwise reports
// every runner that has ever connected as online forever.
func (s *Server) markDisconnectedRunnersOffline(ctx context.Context) error {
	connected := s.connectedRunners()
	if connected == nil {
		connected = []string{}
	}
	_, err := s.pool.Exec(ctx, `UPDATE app.runners SET status='offline' WHERE status='online' AND NOT (id::text=ANY($1))`, connected)
	return databaseError(err)
}

// reclaimExpiredLeases fails jobs whose runner stopped renewing. The runner
// cannot rescue them itself: every write path it has is fenced on an unexpired
// lease, so once the lease lapses a reconnecting runner can neither renew it
// nor report the outcome, and the job would stay 'running' forever.
func (s *Server) reclaimExpiredLeases(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `UPDATE app.jobs SET status='cancelled',finished_at=now(),lease_generation=lease_generation+1,recovery_reason='runner lease expired' WHERE status IN ('preparing','running') AND lease_expires_at<now() RETURNING workflow_run_id::text`)
	if err != nil {
		return databaseError(err)
	}
	var workflows []string
	for rows.Next() {
		var workflowID string
		if err := rows.Scan(&workflowID); err != nil {
			rows.Close()
			return databaseError(err)
		}
		workflows = append(workflows, workflowID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return databaseError(err)
	}
	for _, workflowID := range workflows {
		if err := s.failStrandedWorkflow(ctx, workflowID); err != nil {
			return err
		}
	}
	return nil
}

func (s *Server) failStrandedWorkflow(ctx context.Context, workflowID string) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	const reason = "runner stopped renewing its lease before reporting an outcome"
	var projectID string
	err = tx.QueryRow(ctx, `UPDATE app.workflow_runs SET status='failed',current_phase='failed',terminal_reason=$2,completed_at=now(),updated_at=now() WHERE id=$1 AND status NOT IN ('completed','failed','blocked','cancelled') RETURNING project_id::text`, workflowID, reason).Scan(&projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil
	}
	if err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, projectID, workflowID); err != nil {
		return databaseError(err)
	}
	if err := parkIssue(ctx, tx, workflowID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'failed','error','{"reason":"runner lease expired"}'::jsonb)`, workflowID); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

// resumeStrandedDeliveries re-drives workflows left at 'completed' while still
// holding their project lock. A run reaches that state for the short window
// between the runner's completion being committed and the pull request being
// opened, and the lock is deliberately retained across it. If the process dies
// in that window nothing else looks at the run again: the check observer only
// selects 'waiting_github_checks', and both retry and cancel refuse a run whose
// status is already terminal.
func (s *Server) resumeStrandedDeliveries(ctx context.Context) error {
	rows, err := s.pool.Query(ctx, `SELECT wr.id::text FROM app.workflow_runs wr JOIN app.project_locks l ON l.workflow_run_id=wr.id WHERE wr.status='completed' ORDER BY wr.updated_at,wr.id LIMIT 20`)
	if err != nil {
		return databaseError(err)
	}
	var workflows []string
	for rows.Next() {
		var workflowID string
		if err := rows.Scan(&workflowID); err != nil {
			rows.Close()
			return databaseError(err)
		}
		workflows = append(workflows, workflowID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return databaseError(err)
	}
	var failures []error
	for _, workflowID := range workflows {
		// deliverWorkflow finds an existing pull request before creating one,
		// so re-driving a delivery that already got as far as GitHub reuses it.
		if err := s.deliverWorkflow(ctx, workflowID); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}
