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

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
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
