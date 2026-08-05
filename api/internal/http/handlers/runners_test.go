package handlers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
)

func TestRunnerControlRoutesRequireSessionAndCSRF(t *testing.T) {
	limiter := auth.NewRateLimiter(time.Minute, 60)
	mux := http.NewServeMux()
	NewRunnerHandlers(nil, limiter).RegisterRoutes(mux)

	for _, path := range []string{
		"/api/v1/runners/runner-1/drain",
		"/api/v1/runners/runner-1/undrain",
		"/api/v1/runners/runner-1/revoke",
	} {
		req := httptest.NewRequest(http.MethodPost, path, nil)
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without session: got %d, want %d", path, rec.Code, http.StatusUnauthorized)
		}

		req = httptest.NewRequest(http.MethodPost, path, nil)
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: "session"})
		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF: got %d, want %d", path, rec.Code, http.StatusForbidden)
		}
	}
}

func TestRunnerPayloadNormalizesEmptyLabels(t *testing.T) {
	payload := runnerPayload(&controlv1.Runner{})
	labels, ok := payload["labels"].([]string)
	if !ok || labels == nil {
		t.Fatalf("labels = %#v, want empty []string", payload["labels"])
	}
}

func runnerMux(client runnerClient) http.Handler {
	mux := http.NewServeMux()
	NewRunnerHandlers(client, auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux
}

func TestListRunnersReturnsEveryRunner(t *testing.T) {
	stub := &stubClient{listRunners: func(context.Context) (*controlv1.ListRunnersResponse, error) {
		return &controlv1.ListRunnersResponse{Runners: []*controlv1.Runner{
			{
				Id: "r-1", Name: "builder", Enabled: true, Draining: false,
				Status: "online", Version: "1.2.3", Labels: []string{"linux"},
				LastSeenAt: "2026-08-01T00:00:00Z",
			},
			{Id: "r-2", Name: "idle"},
		}}, nil
	}}
	rec := httptest.NewRecorder()
	runnerMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/runners", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Runners []struct {
			ID         string   `json:"id"`
			Name       string   `json:"name"`
			Enabled    bool     `json:"enabled"`
			Status     string   `json:"status"`
			Version    string   `json:"version"`
			Labels     []string `json:"labels"`
			LastSeenAt string   `json:"lastSeenAt"`
		} `json:"runners"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Runners) != 2 {
		t.Fatalf("runners = %#v, want 2", body.Runners)
	}
	first := body.Runners[0]
	if first.ID != "r-1" || first.Name != "builder" || !first.Enabled ||
		first.Status != "online" || first.Version != "1.2.3" ||
		first.LastSeenAt != "2026-08-01T00:00:00Z" {
		t.Errorf("first runner = %#v", first)
	}
	if len(first.Labels) != 1 || first.Labels[0] != "linux" {
		t.Errorf("labels = %#v, want [linux]", first.Labels)
	}
	// A runner with no labels must still serialize as [], never null: the
	// console iterates the field without a guard.
	if body.Runners[1].Labels == nil || len(body.Runners[1].Labels) != 0 {
		t.Errorf("unlabelled runner labels = %#v, want []", body.Runners[1].Labels)
	}
}

func TestListRunnersMapsOrchestratorErrorsToStatusCodes(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want int
	}{
		{orchestrator.ErrUnauthorized, http.StatusUnauthorized},
		{orchestrator.ErrForbidden, http.StatusForbidden},
		{orchestrator.ErrNotFound, http.StatusNotFound},
		{orchestrator.ErrInvalidInput, http.StatusUnprocessableEntity},
		{context.DeadlineExceeded, http.StatusGatewayTimeout},
		{errors.New("boom"), http.StatusServiceUnavailable},
	} {
		stub := &stubClient{listRunners: func(context.Context) (*controlv1.ListRunnersResponse, error) {
			return nil, tc.err
		}}
		rec := httptest.NewRecorder()
		runnerMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/runners", "", "admin-session"))
		if rec.Code != tc.want {
			t.Errorf("%v: status = %d, want %d", tc.err, rec.Code, tc.want)
		}
	}
}

func TestRunnerControlRoutesForwardTheRequestedState(t *testing.T) {
	for _, tc := range []struct{ path, state string }{
		{"drain", "drain"},
		{"undrain", "enable"},
		{"revoke", "revoke"},
	} {
		t.Run(tc.path, func(t *testing.T) {
			stub := &stubClient{setRunnerState: func(_ context.Context, runnerID, state string) (*controlv1.SetRunnerStateResponse, error) {
				return &controlv1.SetRunnerStateResponse{Runner: &controlv1.Runner{
					Id: runnerID, Name: "builder", Draining: state == "drain",
				}}, nil
			}}
			rec := httptest.NewRecorder()
			runnerMux(stub).ServeHTTP(rec, mutateRequest(
				t, http.MethodPost, "/api/v1/runners/runner-7/"+tc.path, "", "admin-session"))

			if rec.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
			}
			calls := stub.recorded("SetRunnerState")
			if len(calls) != 1 {
				t.Fatalf("SetRunnerState calls = %d, want 1", len(calls))
			}
			if calls[0].args[0] != "runner-7" || calls[0].args[1] != tc.state {
				t.Fatalf("forwarded %v, want [runner-7 %s]", calls[0].args, tc.state)
			}
			body := decodeProject(t, rec)
			if body["id"] != "runner-7" || body["name"] != "builder" {
				t.Fatalf("payload = %#v", body)
			}
		})
	}
}

// A response with no runner in it is the orchestrator answering something the
// handler cannot turn into a payload; answering 200 with a null body would
// leave the console showing a runner row with no fields.
func TestRunnerControlRejectsAResponseWithNoRunner(t *testing.T) {
	stub := &stubClient{setRunnerState: func(context.Context, string, string) (*controlv1.SetRunnerStateResponse, error) {
		return &controlv1.SetRunnerStateResponse{}, nil
	}}
	rec := httptest.NewRecorder()
	runnerMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodPost, "/api/v1/runners/runner-7/drain", "", "admin-session"))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503", rec.Code)
	}
}

func TestRunnerControlSurfacesAnUnknownRunnerAs404(t *testing.T) {
	stub := &stubClient{setRunnerState: func(context.Context, string, string) (*controlv1.SetRunnerStateResponse, error) {
		return nil, orchestrator.ErrNotFound
	}}
	rec := httptest.NewRecorder()
	runnerMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodPost, "/api/v1/runners/nope/drain", "", "admin-session"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestListRunnersRequiresASession(t *testing.T) {
	stub := &stubClient{}
	rec := httptest.NewRecorder()
	runnerMux(stub).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runners", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401", rec.Code)
	}
	if len(stub.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the orchestrator: %#v", stub.calls)
	}
}
