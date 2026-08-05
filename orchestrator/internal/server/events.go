package server

import (
	"context"
	"encoding/json"
	"strconv"
	"strings"
	"time"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/textutil"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// dashboardNotification mirrors the JSON payload the app.notify_dashboard_event()
// trigger (migration 017_dashboard_events.sql) sends over `pg_notify('moirai_dashboard_events', ...)`
// for both app.workflow_events inserts and app.runners inserts/updates. The
// trigger's `id` field is a bare JSON number for workflow events (NEW.id, a
// bigint) but a quoted string for runner events ('runner:'||NEW.id||...), so
// it's captured as json.RawMessage rather than a fixed Go type — decoding it
// straight into a string would fail on every workflow notification.
type dashboardNotification struct {
	ID            json.RawMessage `json:"id"`
	EventType     string          `json:"event_type"`
	WorkflowRunID string          `json:"workflow_run_id"`
	Runner        struct {
		ID string `json:"id"`
	} `json:"runner"`
}

// runnerEventIDPrefix marks the synthetic, non-numeric event ids the trigger
// mints for runner rows (they have no monotonic sequence to draw from). A
// client that reconnects with one of these as Last-Event-ID cannot resume
// from it — runner events are live-only, not replayable — so StreamEvents
// falls back to the default cursor instead of rejecting the request.
const runnerEventIDPrefix = "runner:"

// StreamEvents pushes app.workflow_events and app.runners changes to the
// caller as they happen. It LISTENs on the moirai_dashboard_events channel
// (populated by the trigger in 017_dashboard_events.sql) rather than polling
// the whole table, so a cold connect only pays for a single MAX(id) lookup
// plus whatever a client explicitly asks to replay via Last-Event-ID.
func (s *ControlServer) StreamEvents(request *controlv1.StreamEventsRequest, stream grpc.ServerStreamingServer[controlv1.ControlPlaneEvent]) error {
	if _, err := s.requireActor(stream.Context(), false); err != nil {
		return err
	}
	// Also cancelled when the server starts shutting down, so a WaitForNotification
	// call below that would otherwise sit until this dashboard client disconnects
	// returns promptly and lets GracefulStop finish. See Server.Shutdown.
	ctx, cancel := s.withShutdown(stream.Context())
	defer cancel()

	// Acquire a dedicated connection and start listening before computing
	// the starting cursor. If LISTEN happened after the cursor read, any
	// workflow_events row committed in between would be neither covered by
	// the cursor nor announced by a notification we're subscribed to, and
	// would be lost until the next unrelated event nudged a catch-up query.
	conn, err := s.pool.Acquire(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, "LISTEN moirai_dashboard_events"); err != nil {
		return databaseError(err)
	}

	after, err := s.startingCursor(ctx, request.GetLastEventId())
	if err != nil {
		return err
	}

	// Replay anything committed at or before the LISTEN call that the cursor
	// doesn't already cover (an explicit Last-Event-ID backlog, or events
	// that landed in the race window described above).
	if err := s.emitWorkflowEvents(ctx, stream, &after); err != nil {
		return err
	}

	for {
		notification, err := conn.Conn().WaitForNotification(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			return databaseError(err)
		}
		var payload dashboardNotification
		if err := json.Unmarshal([]byte(notification.Payload), &payload); err != nil {
			// A malformed payload should never take the whole stream down;
			// skip it and keep listening.
			continue
		}
		switch payload.EventType {
		case "workflow":
			if err := s.emitWorkflowEvents(ctx, stream, &after); err != nil {
				return err
			}
		case "runner":
			if err := s.emitRunnerEvent(ctx, stream, payload); err != nil {
				return err
			}
		}
	}
}

// startingCursor resolves the Last-Event-ID header to a workflow_events
// cursor. An empty header (cold connect) or a non-numeric, runner-shaped id
// (reconnecting right after a runner event) both default to the current
// maximum event id, so a client never pays for a full-table replay — history
// beyond that remains available through ListWorkflowEvents. Any other
// unparsable value is treated as caller error.
func (s *Core) startingCursor(ctx context.Context, lastEventID string) (int64, error) {
	if lastEventID != "" && !strings.HasPrefix(lastEventID, runnerEventIDPrefix) {
		value, err := strconv.ParseInt(lastEventID, 10, 64)
		if err != nil || value < 0 {
			return 0, status.Error(codes.InvalidArgument, "event cursor is invalid")
		}
		return value, nil
	}
	var after int64
	if err := s.pool.QueryRow(ctx, `SELECT COALESCE(MAX(id),0) FROM app.workflow_events`).Scan(&after); err != nil {
		return 0, databaseError(err)
	}
	return after, nil
}

// emitWorkflowEvents sends every app.workflow_events row after *cursor, in
// batches of 100, advancing *cursor as it goes. The workflow lookups for a
// batch are done with a single ANY($1) query instead of one query per row.
func (s *Core) emitWorkflowEvents(ctx context.Context, stream grpc.ServerStreamingServer[controlv1.ControlPlaneEvent], cursor *int64) error {
	for {
		type eventRow struct {
			id         int64
			workflowID string
		}
		rows, err := s.pool.Query(ctx, `SELECT id,workflow_run_id::text FROM app.workflow_events WHERE id>$1 ORDER BY id LIMIT 100`, *cursor)
		if err != nil {
			return databaseError(err)
		}
		var batch []eventRow
		ids := make([]string, 0, 100)
		seen := make(map[string]bool, 100)
		for rows.Next() {
			var row eventRow
			if err := rows.Scan(&row.id, &row.workflowID); err != nil {
				rows.Close()
				return databaseError(err)
			}
			batch = append(batch, row)
			if !seen[row.workflowID] {
				seen[row.workflowID] = true
				ids = append(ids, row.workflowID)
			}
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return databaseError(err)
		}
		rows.Close()
		if len(batch) == 0 {
			return nil
		}
		workflows, err := s.workflowsByIDs(ctx, ids)
		if err != nil {
			return err
		}
		for _, row := range batch {
			// The workflow can legitimately be gone by the time we look it
			// up (e.g. cascaded delete) — advance the cursor past it either
			// way so the stream doesn't get stuck replaying it forever, but
			// only send an event when there's a workflow to attach.
			*cursor = row.id
			workflow, ok := workflows[row.workflowID]
			if !ok {
				continue
			}
			if err := stream.Send(&controlv1.ControlPlaneEvent{Id: strconv.FormatInt(row.id, 10), EventType: "workflow", Workflow: workflow}); err != nil {
				return err
			}
		}
		if len(batch) < 100 {
			return nil
		}
	}
}

// emitRunnerEvent re-reads the runner named in a NOTIFY payload and sends it
// as a "runner" event. Re-reading (rather than trusting the JSON payload's
// snapshot of the row) guarantees the event reflects the runner's current
// state even if several updates coalesced before this notification was
// processed, and picks up fields the trigger doesn't emit (e.g. version).
func (s *Core) emitRunnerEvent(ctx context.Context, stream grpc.ServerStreamingServer[controlv1.ControlPlaneEvent], payload dashboardNotification) error {
	if !idgen.ValidID(payload.Runner.ID) {
		return nil
	}
	runner, err := s.runner(ctx, payload.Runner.ID)
	if status.Code(err) == codes.NotFound {
		// Revoked/deleted between the notification firing and us reading it.
		return nil
	}
	if err != nil {
		return err
	}
	var eventID string
	if err := json.Unmarshal(payload.ID, &eventID); err != nil {
		// The trigger always quotes runner ids as JSON strings; a raw
		// notification this malformed shouldn't take the stream down.
		return nil
	}
	return stream.Send(&controlv1.ControlPlaneEvent{Id: eventID, EventType: "runner", Runner: runner})
}

// workflowsByIDs batch-loads workflows for a StreamEvents page, keyed by id,
// replacing the per-row s.workflow() lookup StreamEvents used to make.
func (s *Core) workflowsByIDs(ctx context.Context, ids []string) (map[string]*controlv1.Workflow, error) {
	rows, err := s.pool.Query(ctx, `SELECT wr.id::text,wr.project_id::text,wr.status,wr.current_phase,i.external_id,i.title,wr.branch_name,COALESCE(pr.external_id,wr.pull_request_external_id),COALESCE(pr.url,wr.pull_request_url),pr.state,wr.blocking_reason,wr.planning_attempts,wr.implementation_attempts,wr.pipeline_repair_attempts,wr.ci_repair_attempts,wr.review_cycles,wr.total_agent_executions,wr.created_at,wr.updated_at FROM app.workflow_runs wr JOIN app.issues i ON i.id=wr.issue_id LEFT JOIN app.pull_requests pr ON pr.workflow_run_id=wr.id WHERE wr.id=ANY($1)`, ids)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	workflows := make(map[string]*controlv1.Workflow, len(ids))
	for rows.Next() {
		workflow := &controlv1.Workflow{}
		var branch, externalID, url, state, reason *string
		var created, updated time.Time
		if err := rows.Scan(&workflow.Id, &workflow.ProjectId, &workflow.Status, &workflow.Phase, &workflow.IssueExternalId, &workflow.IssueTitle, &branch, &externalID, &url, &state, &reason, &workflow.PlanningAttempts, &workflow.ImplementationAttempts, &workflow.PipelineRepairAttempts, &workflow.CiRepairAttempts, &workflow.ReviewCycles, &workflow.TotalAgentExecutions, &created, &updated); err != nil {
			return nil, databaseError(err)
		}
		workflow.BranchName, workflow.PullRequestExternalId, workflow.PullRequestUrl, workflow.PullRequestState, workflow.BlockingReason = textutil.StringValue(branch), textutil.StringValue(externalID), textutil.StringValue(url), textutil.StringValue(state), textutil.StringValue(reason)
		workflow.CreatedAt, workflow.UpdatedAt = textutil.Timestamp(created), textutil.Timestamp(updated)
		workflows[workflow.Id] = workflow
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
	}
	return workflows, nil
}
