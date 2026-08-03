package handlers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"google.golang.org/grpc"
)

// fakeSchedulerMetricsPlane returns a fixed response, so the test can check
// the REST handler translates every field the console needs rather than
// dropping the loop statuses on the floor between the gRPC response and the
// JSON one.
type fakeSchedulerMetricsPlane struct {
	controlv1.UnimplementedControlPlaneServer
	response *controlv1.GetSchedulerMetricsResponse
}

func (f *fakeSchedulerMetricsPlane) GetSchedulerMetrics(context.Context, *controlv1.GetSchedulerMetricsRequest) (*controlv1.GetSchedulerMetricsResponse, error) {
	return f.response, nil
}

func startSchedulerMetricsServer(t *testing.T, response *controlv1.GetSchedulerMetricsResponse) http.Handler {
	t.Helper()
	fake := &fakeSchedulerMetricsPlane{response: response}
	grpcServer := grpc.NewServer()
	controlv1.RegisterControlPlaneServer(grpcServer, fake)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := orchestrator.Dial(ctx, listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	mux := http.NewServeMux()
	NewProjectHandlers(client, auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux
}

// This is the console's only view of background-loop liveness (issue #278):
// if this translation drops a field, the console has nothing to show for a
// stalled loop no matter how faithfully the orchestrator tracked it.
func TestSchedulerMetricsIncludesLoopStatuses(t *testing.T) {
	mux := startSchedulerMetricsServer(t, &controlv1.GetSchedulerMetricsResponse{
		QueueDepth:      3,
		ActiveWorkflows: 2,
		ScheduledJobs:   1,
		LoopStatuses: []*controlv1.LoopStatus{
			{Name: "issue_sync", Healthy: true, LastSuccessAt: "2026-08-03T12:00:00Z"},
			{Name: "recovery_sweep", Healthy: false, LastError: "gh: rate limited", LastErrorAt: "2026-08-03T11:00:00Z"},
		},
	})
	req := httptest.NewRequest(http.MethodGet, "/api/v1/scheduler/metrics", nil)
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-1"})
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		QueueDepth int32 `json:"queueDepth"`
		Loops      []struct {
			Name          string `json:"name"`
			Healthy       bool   `json:"healthy"`
			LastSuccessAt string `json:"lastSuccessAt"`
			LastError     string `json:"lastError"`
			LastErrorAt   string `json:"lastErrorAt"`
		} `json:"loops"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("unmarshal: %v; body: %s", err, rec.Body.String())
	}
	if body.QueueDepth != 3 {
		t.Errorf("queueDepth = %d, want 3", body.QueueDepth)
	}
	if len(body.Loops) != 2 {
		t.Fatalf("loops = %#v, want 2 entries", body.Loops)
	}
	if body.Loops[0].Name != "issue_sync" || !body.Loops[0].Healthy || body.Loops[0].LastSuccessAt != "2026-08-03T12:00:00Z" {
		t.Errorf("loops[0] = %+v, want a healthy issue_sync with its last-success timestamp", body.Loops[0])
	}
	if body.Loops[1].Name != "recovery_sweep" || body.Loops[1].Healthy || body.Loops[1].LastError != "gh: rate limited" {
		t.Errorf("loops[1] = %+v, want an unhealthy recovery_sweep carrying its last error", body.Loops[1])
	}
}

func TestSchedulerMetricsRouteRequiresSession(t *testing.T) {
	h := NewProjectHandlers(nil, nil)
	mux := http.NewServeMux()
	h.RegisterRoutes(mux)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/scheduler/metrics", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("GET /api/v1/scheduler/metrics without session: got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}
