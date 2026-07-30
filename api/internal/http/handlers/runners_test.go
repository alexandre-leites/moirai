package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
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
