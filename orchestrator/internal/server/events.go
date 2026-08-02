package server

import (
	"strconv"
	"time"

	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func (s *Server) StreamEvents(request *controlv1.StreamEventsRequest, stream grpc.ServerStreamingServer[controlv1.ControlPlaneEvent]) error {
	if _, err := s.requireActor(stream.Context(), false); err != nil {
		return err
	}
	after := int64(0)
	if request.GetLastEventId() != "" {
		value, err := strconv.ParseInt(request.GetLastEventId(), 10, 64)
		if err != nil || value < 0 {
			return status.Error(codes.InvalidArgument, "event cursor is invalid")
		}
		after = value
	}
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		rows, err := s.pool.Query(stream.Context(), `SELECT id,workflow_run_id::text FROM app.workflow_events WHERE id>$1 ORDER BY id LIMIT 100`, after)
		if err != nil {
			return databaseError(err)
		}
		for rows.Next() {
			var id int64
			var workflowID string
			if err := rows.Scan(&id, &workflowID); err != nil {
				rows.Close()
				return databaseError(err)
			}
			workflow, err := s.workflow(stream.Context(), workflowID)
			if err != nil {
				rows.Close()
				return err
			}
			if err := stream.Send(&controlv1.ControlPlaneEvent{Id: strconv.FormatInt(id, 10), EventType: "workflow", Workflow: workflow}); err != nil {
				rows.Close()
				return err
			}
			after = id
		}
		if err := rows.Err(); err != nil {
			rows.Close()
			return databaseError(err)
		}
		rows.Close()
		select {
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-ticker.C:
		}
	}
}
