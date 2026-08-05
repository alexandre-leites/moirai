package handlers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	"google.golang.org/grpc"
)

// fakeSyncPlane records what project id the handler forwarded, which is the
// only thing the request body controls.
type fakeSyncPlane struct {
	controlv1.UnimplementedControlPlaneServer
	requested []string
}

func (f *fakeSyncPlane) SyncNow(_ context.Context, in *controlv1.SyncNowRequest) (*controlv1.SyncNowResponse, error) {
	f.requested = append(f.requested, in.GetProjectId())
	return &controlv1.SyncNowResponse{
		Results: []*controlv1.ProjectSyncResult{{ProjectId: "project-1", SyncedIssues: 3}},
	}, nil
}

func startSyncServer(t *testing.T) (http.Handler, *fakeSyncPlane) {
	t.Helper()
	fake := &fakeSyncPlane{}
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
	NewSyncHandlers(client, auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux, fake
}

func syncRequest(t *testing.T, body string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-1"})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "csrf-token"})
	req.Header.Set(auth.CSRFHeaderName, "csrf-token")
	return req
}

// The console sends `{}` for "sync everything". Reading the body and then
// decoding from r.Body made that EOF, so every Sync now click returned
// 400 "Invalid request body: EOF".
func TestSyncNowAcceptsTheEmptyObjectTheConsoleSends(t *testing.T) {
	mux, fake := startSyncServer(t)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, syncRequest(t, "{}"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fake.requested) != 1 || fake.requested[0] != "" {
		t.Fatalf("forwarded project ids = %#v, want one empty (sync everything)", fake.requested)
	}
}

func TestSyncNowForwardsTheRequestedProject(t *testing.T) {
	mux, fake := startSyncServer(t)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, syncRequest(t, `{"projectId":"project-7"}`))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fake.requested) != 1 || fake.requested[0] != "project-7" {
		t.Fatalf("forwarded project ids = %#v, want [project-7]", fake.requested)
	}
}

func TestSyncNowAcceptsNoBodyAtAll(t *testing.T) {
	mux, fake := startSyncServer(t)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, syncRequest(t, ""))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if len(fake.requested) != 1 || fake.requested[0] != "" {
		t.Fatalf("forwarded project ids = %#v, want one empty", fake.requested)
	}
}

func TestSyncNowReturnsTheResults(t *testing.T) {
	mux, _ := startSyncServer(t)
	rec := httptest.NewRecorder()

	mux.ServeHTTP(rec, syncRequest(t, "{}"))

	var body struct {
		Results []struct {
			ProjectID    string `json:"projectId"`
			SyncedIssues int    `json:"syncedIssues"`
		} `json:"results"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Results) != 1 || body.Results[0].SyncedIssues != 3 {
		t.Fatalf("results = %#v", body.Results)
	}
}

func TestSyncNowRejectsAMalformedBodyRatherThanSyncingEverything(t *testing.T) {
	// The dangerous failure is not the 400 -- it is treating an unreadable
	// request as "no project specified" and syncing every project instead.
	mux, fake := startSyncServer(t)
	for _, body := range []string{`{"projectId":`, `not json`, `{"unknownField":"x"}`} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, syncRequest(t, body))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body %q gave status %d, want 400", body, rec.Code)
		}
	}
	if len(fake.requested) != 0 {
		t.Fatalf("a rejected request still reached the orchestrator: %#v", fake.requested)
	}
}

func TestSyncNowRequiresSessionAndCSRF(t *testing.T) {
	mux, fake := startSyncServer(t)

	noSession := httptest.NewRequest(http.MethodPost, "/api/v1/sync", strings.NewReader("{}"))
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, noSession)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("without a session: status = %d, want 401", rec.Code)
	}

	noCSRF := httptest.NewRequest(http.MethodPost, "/api/v1/sync", strings.NewReader("{}"))
	noCSRF.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session-1"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, noCSRF)
	if rec.Code != http.StatusForbidden {
		t.Fatalf("without CSRF: status = %d, want 403", rec.Code)
	}

	if len(fake.requested) != 0 {
		t.Fatalf("an unauthenticated request reached the orchestrator: %#v", fake.requested)
	}
}

func syncMux(client syncClient) http.Handler {
	mux := http.NewServeMux()
	NewSyncHandlers(client, auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux
}

// The status view is how an operator sees that a registered project is (or is
// not) being picked up, so every field the console renders has to survive the
// translation -- including the back-off fields that explain a stalled project.
func TestSyncStatusReportsEveryProjectEntry(t *testing.T) {
	stub := &stubClient{issueSyncStatus: func(context.Context) (*controlv1.IssueSyncStatusResponse, error) {
		return &controlv1.IssueSyncStatusResponse{Entries: []*controlv1.IssueSyncStatusEntry{
			{
				ProjectId: "p-1", ProjectName: "billing", Enabled: true,
				IssueCount: 12, EligibleCount: 3, LastSyncedAt: "2026-08-01T00:00:00Z",
			},
			{
				ProjectId: "p-2", ProjectName: "secure", Enabled: false,
				ConsecutiveFailures: 4, NextRetryAt: "2026-08-01T01:00:00Z",
				LastError: "credentials rejected", BackingOff: true,
			},
		}}, nil
	}}
	rec := httptest.NewRecorder()
	syncMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/sync/status", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Entries []struct {
			ProjectID           string `json:"projectId"`
			ProjectName         string `json:"projectName"`
			Enabled             bool   `json:"enabled"`
			IssueCount          int32  `json:"issueCount"`
			EligibleCount       int32  `json:"eligibleCount"`
			LastSyncedAt        string `json:"lastSyncedAt"`
			ConsecutiveFailures int32  `json:"consecutiveFailures"`
			NextRetryAt         string `json:"nextRetryAt"`
			LastError           string `json:"lastError"`
			BackingOff          bool   `json:"backingOff"`
		} `json:"entries"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Entries) != 2 {
		t.Fatalf("entries = %#v, want 2", body.Entries)
	}
	healthy := body.Entries[0]
	if healthy.ProjectID != "p-1" || healthy.ProjectName != "billing" || !healthy.Enabled ||
		healthy.IssueCount != 12 || healthy.EligibleCount != 3 ||
		healthy.LastSyncedAt != "2026-08-01T00:00:00Z" {
		t.Errorf("healthy entry = %#v", healthy)
	}
	stalled := body.Entries[1]
	if stalled.ConsecutiveFailures != 4 || !stalled.BackingOff ||
		stalled.LastError != "credentials rejected" || stalled.NextRetryAt != "2026-08-01T01:00:00Z" {
		t.Errorf("stalled entry = %#v", stalled)
	}
}

func TestSyncStatusAnswersAnEmptyListRatherThanNull(t *testing.T) {
	stub := &stubClient{issueSyncStatus: func(context.Context) (*controlv1.IssueSyncStatusResponse, error) {
		return &controlv1.IssueSyncStatusResponse{}, nil
	}}
	rec := httptest.NewRecorder()
	syncMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/sync/status", "", "admin-session"))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d", rec.Code)
	}
	if got := strings.TrimSpace(rec.Body.String()); got != `{"entries":[]}` {
		t.Fatalf("body = %s, want an empty entries array", got)
	}
}

func TestSyncStatusMapsOrchestratorErrors(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{orchestrator.ErrUnauthorized, http.StatusUnauthorized},
		{orchestrator.ErrForbidden, http.StatusForbidden},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
	} {
		stub := &stubClient{issueSyncStatus: func(context.Context) (*controlv1.IssueSyncStatusResponse, error) {
			return nil, tc.err
		}}
		rec := httptest.NewRecorder()
		syncMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/sync/status", "", "admin-session"))
		if rec.Code != tc.want {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}

func TestSyncStatusRequiresASession(t *testing.T) {
	stub := &stubClient{}
	rec := httptest.NewRecorder()
	syncMux(stub).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/sync/status", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the orchestrator: %#v", stub.calls)
	}
}

// A body larger than the cap is refused rather than buffered, and must not be
// mistaken for "no project specified" and sync every project.
func TestSyncNowRejectsAnOversizedBody(t *testing.T) {
	stub := &stubClient{syncNow: func(context.Context, string) (*controlv1.SyncNowResponse, error) {
		return &controlv1.SyncNowResponse{}, nil
	}}
	oversized := `{"projectId":"` + strings.Repeat("x", maxSyncRequestBytes) + `"}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/sync", strings.NewReader(oversized))
	req.Header.Set("Content-Type", "application/json")
	req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "admin-session"})
	req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "csrf-token"})
	req.Header.Set(auth.CSRFHeaderName, "csrf-token")

	rec := httptest.NewRecorder()
	syncMux(stub).ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
	}
	if len(stub.calls) != 0 {
		t.Fatalf("an oversized request reached the orchestrator: %#v", stub.calls)
	}
}

func TestSyncNowSurfacesOrchestratorRejection(t *testing.T) {
	stub := &stubClient{syncNow: func(context.Context, string) (*controlv1.SyncNowResponse, error) {
		return nil, orchestrator.ErrNotFound
	}}
	rec := httptest.NewRecorder()
	syncMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodPost, "/api/v1/sync", `{"projectId":"nope"}`, "admin-session"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}
