package server_test

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	apiserver "github.com/loop-engineering/api/internal/http"
)

func TestServerValidatesConfig(t *testing.T) {
	_, err := apiserver.New(apiserver.Config{BindAddress: ""}, nil)
	if err == nil {
		t.Fatal("expected error for empty bind address")
	}
	_, err = apiserver.New(apiserver.Config{BindAddress: "not-valid"}, nil)
	if err == nil {
		t.Fatal("expected error for invalid bind address")
	}
}

func TestServerHealthRoutes(t *testing.T) {
	s, err := apiserver.New(apiserver.DefaultConfig(), nil)
	if err != nil {
		t.Fatalf("new server: %v", err)
	}
	for _, path := range []string{"/live", "/ready"} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		s.Mux().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Errorf("GET %s: got %d, want %d", path, rec.Code, http.StatusOK)
		}
	}
}

func TestWriteErrorSetsCorrectStatusAndContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	apiserver.WriteError(rec, http.StatusUnauthorized, "Unauthorized", "session required")
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
	ct := rec.Header().Get("Content-Type")
	if ct != "application/problem+json" {
		t.Errorf("got content-type %q, want application/problem+json", ct)
	}
}

func TestDefaultConfigIsValid(t *testing.T) {
	if err := apiserver.DefaultConfig().Validate(); err != nil {
		t.Errorf("default config invalid: %v", err)
	}
}

func TestWriteJSONSetsContentType(t *testing.T) {
	rec := httptest.NewRecorder()
	apiserver.WriteJSON(rec, http.StatusOK, map[string]string{"ok": "true"})
	if rec.Code != http.StatusOK {
		t.Errorf("got %d, want %d", rec.Code, http.StatusOK)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("content-type: got %q, want application/json", ct)
	}
}

var _ = errors.New
