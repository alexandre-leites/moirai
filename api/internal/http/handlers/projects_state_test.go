package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// The enable/disable pair and the credential delete are the three project
// endpoints the existing suite never reached. They run against the same
// fakeControlPlane gRPC server as the rest of projects_test.go, so they also
// cover the session and CSRF metadata the real client attaches.

func TestDisableAndEnableProjectFlipTheStoredFlag(t *testing.T) {
	mux, fake := startProjectServer(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/disable", "", "admin-session"))
	if rec.Code != http.StatusOK {
		t.Fatalf("disable: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := decodeProject(t, rec); body["enabled"] != false || body["id"] != "p-1" {
		t.Fatalf("disable payload = %#v", body)
	}
	if fake.projects["p-1"].Enabled {
		t.Fatal("disable did not reach the orchestrator")
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/enable", "", "admin-session"))
	if rec.Code != http.StatusOK {
		t.Fatalf("enable: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	if body := decodeProject(t, rec); body["enabled"] != true {
		t.Fatalf("enable payload = %#v", body)
	}
	if !fake.projects["p-1"].Enabled {
		t.Fatal("enable did not reach the orchestrator")
	}
}

func TestEnableUnknownProjectAnswers404(t *testing.T) {
	mux, _ := startProjectServer(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, mutateRequest(t, http.MethodPost, "/api/v1/projects/missing/enable", "", "admin-session"))
	if rec.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}
}

// A viewer may read a project but must not toggle it, and the orchestrator's
// permission denial has to arrive as 403 rather than a generic failure.
func TestEnableProjectRejectsANonAdministrator(t *testing.T) {
	mux, fake := startProjectServer(t)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, mutateRequest(t, http.MethodPost, "/api/v1/projects/p-2/enable", "", "viewer-session"))
	if rec.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403, body = %s", rec.Code, rec.Body.String())
	}
	if fake.projects["p-2"].Enabled {
		t.Fatal("a rejected request still enabled the project")
	}
}

func TestProjectStateRoutesRequireSessionAndCSRF(t *testing.T) {
	mux, fake := startProjectServer(t)
	for _, path := range []string{
		"/api/v1/projects/p-1/enable",
		"/api/v1/projects/p-1/disable",
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, path, nil))
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s without a session: status = %d, want 401", path, rec.Code)
		}

		rec = httptest.NewRecorder()
		mux.ServeHTTP(rec, projectRequest(t, http.MethodPost, path, "", "admin-session"))
		if rec.Code != http.StatusForbidden {
			t.Errorf("%s without CSRF: status = %d, want 403", path, rec.Code)
		}
	}
	if !fake.projects["p-1"].Enabled {
		t.Fatal("an unauthenticated request changed the project")
	}
}

// Deleting a credential answers with the remaining summary, so the console can
// redraw the credential list from the delete response alone -- and that summary
// still never carries a value.
func TestClearCredentialReturnsTheRemainingSummary(t *testing.T) {
	mux, fake := startProjectServer(t)

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, mutateRequest(t, http.MethodPut,
		"/api/v1/projects/p-1/credentials/agent:claude", `{"value":"sk-secret","filePath":".config/token"}`, "admin-session"))
	if rec.Code != http.StatusOK {
		t.Fatalf("set: status = %d, body = %s", rec.Code, rec.Body.String())
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, mutateRequest(t, http.MethodDelete,
		"/api/v1/projects/p-1/credentials/agent:claude", "", "admin-session"))
	if rec.Code != http.StatusOK {
		t.Fatalf("delete: status = %d, body = %s", rec.Code, rec.Body.String())
	}
	body := decodeProject(t, rec)
	credentials, ok := body["credentials"].([]any)
	if !ok || len(credentials) != 0 {
		t.Fatalf("credentials after delete = %#v, want an empty list", body["credentials"])
	}
	if fake.credentials != nil {
		t.Fatalf("the delete never reached the orchestrator: %#v", fake.credentials)
	}
}

func TestClearCredentialRequiresSessionAndCSRF(t *testing.T) {
	mux, _ := startProjectServer(t)
	const path = "/api/v1/projects/p-1/credentials/agent:claude"

	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, httptest.NewRequest(http.MethodDelete, path, nil))
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("without a session: status = %d, want 401", rec.Code)
	}

	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, projectRequest(t, http.MethodDelete, path, "", "admin-session"))
	if rec.Code != http.StatusForbidden {
		t.Errorf("without CSRF: status = %d, want 403", rec.Code)
	}
}
