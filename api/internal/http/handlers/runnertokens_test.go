package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
)

func runnerTokenMux(client runnerTokenClient) http.Handler {
	mux := http.NewServeMux()
	NewRunnerTokenHandlers(client, auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux
}

func TestListRunnerTokensReturnsTheRegistrationTokens(t *testing.T) {
	stub := &stubClient{listTokens: func(context.Context) (*controlv1.ListRunnerRegistrationTokensResponse, error) {
		return &controlv1.ListRunnerRegistrationTokensResponse{
			Tokens: []*controlv1.RunnerRegistrationToken{
				{Id: "t-1", AllowedLabels: []string{"linux"}, ExpiresAt: "2026-09-01T00:00:00Z"},
				{Id: "t-2", ExpiresAt: "2026-09-02T00:00:00Z", UsedAt: "2026-08-02T00:00:00Z", RevokedAt: "2026-08-03T00:00:00Z"},
			},
		}, nil
	}}
	rec := httptest.NewRecorder()
	runnerTokenMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/runner-tokens", "", "admin-session"))

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Tokens []struct {
			ID            string   `json:"id"`
			AllowedLabels []string `json:"allowedLabels"`
			ExpiresAt     string   `json:"expiresAt"`
			UsedAt        string   `json:"usedAt"`
			RevokedAt     string   `json:"revokedAt"`
		} `json:"tokens"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Tokens) != 2 {
		t.Fatalf("tokens = %#v, want 2", body.Tokens)
	}
	if body.Tokens[0].ID != "t-1" || body.Tokens[0].ExpiresAt != "2026-09-01T00:00:00Z" ||
		len(body.Tokens[0].AllowedLabels) != 1 || body.Tokens[0].AllowedLabels[0] != "linux" {
		t.Errorf("first token = %#v", body.Tokens[0])
	}
	if body.Tokens[1].UsedAt != "2026-08-02T00:00:00Z" || body.Tokens[1].RevokedAt != "2026-08-03T00:00:00Z" {
		t.Errorf("second token = %#v", body.Tokens[1])
	}
	// A listing must never carry the secret itself; only creation does.
	if raw := rec.Body.String(); jsonHasKey(t, raw, "token") {
		t.Errorf("listing exposed a token value: %s", raw)
	}
}

// jsonHasKey reports whether any token object in a runner-token listing
// carries a "token" key.
func jsonHasKey(t *testing.T, raw, key string) bool {
	t.Helper()
	var body struct {
		Tokens []map[string]any `json:"tokens"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	for _, token := range body.Tokens {
		if _, ok := token[key]; ok {
			return true
		}
	}
	return false
}

func TestCreateRunnerTokenReturns201WithTheSecret(t *testing.T) {
	stub := &stubClient{createToken: func(_ context.Context, labels []string) (*controlv1.CreateRunnerRegistrationTokenResponse, error) {
		return &controlv1.CreateRunnerRegistrationTokenResponse{
			Token: "secret-value", ExpiresAt: "2026-09-01T00:00:00Z",
		}, nil
	}}
	rec := httptest.NewRecorder()
	runnerTokenMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodPost, "/api/v1/runner-tokens", `{"allowedLabels":["linux","docker"]}`, "admin-session"))

	if rec.Code != http.StatusCreated {
		t.Fatalf("status = %d, want 201, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeProject(t, rec)
	if body["token"] != "secret-value" || body["expiresAt"] != "2026-09-01T00:00:00Z" {
		t.Fatalf("payload = %#v", body)
	}
	calls := stub.recorded("CreateRunnerRegistrationToken")
	if len(calls) != 1 {
		t.Fatalf("CreateRunnerRegistrationToken calls = %d, want 1", len(calls))
	}
	labels, _ := calls[0].args[0].([]string)
	if len(labels) != 2 || labels[0] != "linux" || labels[1] != "docker" {
		t.Fatalf("forwarded labels = %#v, want [linux docker]", labels)
	}
}

func TestCreateRunnerTokenRejectsAMalformedBody(t *testing.T) {
	for name, request := range map[string]func() *http.Request{
		"unknown field": func() *http.Request {
			return mutateRequest(t, http.MethodPost, "/api/v1/runner-tokens", `{"labels":["linux"]}`, "admin-session")
		},
		"not json": func() *http.Request {
			return mutateRequest(t, http.MethodPost, "/api/v1/runner-tokens", `{`, "admin-session")
		},
		"wrong content type": func() *http.Request {
			req := mutateRequest(t, http.MethodPost, "/api/v1/runner-tokens", `{}`, "admin-session")
			req.Header.Set("Content-Type", "text/plain")
			return req
		},
	} {
		t.Run(name, func(t *testing.T) {
			stub := &stubClient{}
			rec := httptest.NewRecorder()
			runnerTokenMux(stub).ServeHTTP(rec, request())
			if rec.Code != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400, body = %s", rec.Code, rec.Body.String())
			}
			if len(stub.calls) != 0 {
				t.Fatalf("a rejected request still minted a token: %#v", stub.calls)
			}
		})
	}
}

func TestRevokeRunnerTokenAnswers204AndForwardsTheID(t *testing.T) {
	stub := &stubClient{revokeToken: func(context.Context, string) (*controlv1.RevokeRunnerRegistrationTokenResponse, error) {
		return &controlv1.RevokeRunnerRegistrationTokenResponse{}, nil
	}}
	rec := httptest.NewRecorder()
	runnerTokenMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodDelete, "/api/v1/runner-tokens/t-9", "", "admin-session"))

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204, body = %s", rec.Code, rec.Body.String())
	}
	if rec.Body.Len() != 0 {
		t.Errorf("204 carried a body: %s", rec.Body.String())
	}
	calls := stub.recorded("RevokeRunnerRegistrationToken")
	if len(calls) != 1 || calls[0].args[0] != "t-9" {
		t.Fatalf("revoked %#v, want [t-9]", calls)
	}
}

func TestRevokeRunnerTokenSurfacesAnUnknownTokenAs404(t *testing.T) {
	stub := &stubClient{revokeToken: func(context.Context, string) (*controlv1.RevokeRunnerRegistrationTokenResponse, error) {
		return nil, orchestrator.ErrNotFound
	}}
	rec := httptest.NewRecorder()
	runnerTokenMux(stub).ServeHTTP(rec, mutateRequest(
		t, http.MethodDelete, "/api/v1/runner-tokens/nope", "", "admin-session"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", rec.Code)
	}
}

func TestListRunnerTokensSurfacesAForbiddenCallerAs403(t *testing.T) {
	stub := &stubClient{listTokens: func(context.Context) (*controlv1.ListRunnerRegistrationTokensResponse, error) {
		return nil, orchestrator.ErrForbidden
	}}
	rec := httptest.NewRecorder()
	runnerTokenMux(stub).ServeHTTP(rec, projectRequest(t, http.MethodGet, "/api/v1/runner-tokens", "", "viewer-session"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", rec.Code)
	}
}

func TestRunnerTokenRoutesRequireSessionAndCSRF(t *testing.T) {
	stub := &stubClient{}
	mux := runnerTokenMux(stub)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/v1/runner-tokens", nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("list without a session: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, projectRequest(t, http.MethodPost, "/api/v1/runner-tokens", `{}`, "admin-session"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("create without CSRF: status = %d, want 403", rec.Code)
	}

	if len(stub.calls) != 0 {
		t.Fatalf("an unauthenticated request reached the orchestrator: %#v", stub.calls)
	}
}
