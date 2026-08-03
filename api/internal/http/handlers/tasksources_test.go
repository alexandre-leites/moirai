package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestListTaskSourceTypesRendersFieldsGenerically(t *testing.T) {
	mux, _ := startProjectServer(t)
	req := projectRequest(t, http.MethodGet, "/api/v1/task-source-types", "", "viewer-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", rec.Code, rec.Body.String())
	}
	var body struct {
		Types []struct {
			ID          string `json:"id"`
			DisplayName string `json:"displayName"`
			Fields      []struct {
				Key      string   `json:"key"`
				Label    string   `json:"label"`
				Kind     string   `json:"kind"`
				Required bool     `json:"required"`
				Options  []string `json:"options"`
			} `json:"fields"`
		} `json:"types"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Types) != 1 || body.Types[0].ID != "github" {
		t.Fatalf("types = %+v, want the fake github descriptor", body.Types)
	}
	fields := body.Types[0].Fields
	if len(fields) != 2 || fields[0].Key != "ref" || fields[1].Key != "token" {
		t.Fatalf("fields = %+v, want ref then token", fields)
	}
	if fields[1].Kind != "secret" {
		t.Fatalf("token field kind = %q, want secret", fields[1].Kind)
	}
}

func TestListTaskSourceTypesRequiresSession(t *testing.T) {
	mux, _ := startProjectServer(t)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/task-source-types", nil)
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("got %d, want %d", rec.Code, http.StatusUnauthorized)
	}
}

func TestCreateTaskSourceForwardsConfigurationAndSecrets(t *testing.T) {
	mux, fake := startProjectServer(t)
	req := mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/task-sources",
		`{"provider":"github","name":"primary","enabled":true,"configuration":{"ref":"acme/billing"},"secrets":{"token":"ghp_secret"}}`,
		"admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want 201: %s", rec.Code, rec.Body.String())
	}
	if strings.Contains(rec.Body.String(), "ghp_secret") {
		t.Fatalf("response echoed the secret: %s", rec.Body.String())
	}
	var created struct {
		ID            string         `json:"id"`
		Configuration map[string]any `json:"configuration"`
		Secrets       []struct {
			Key        string `json:"key"`
			Configured bool   `json:"configured"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if created.Configuration["ref"] != "acme/billing" {
		t.Fatalf("configuration = %+v, want ref acme/billing", created.Configuration)
	}
	if len(created.Secrets) != 1 || created.Secrets[0].Key != "token" || !created.Secrets[0].Configured {
		t.Fatalf("secrets = %+v, want token configured=true, never a value", created.Secrets)
	}

	fake.mu.Lock()
	stored := fake.taskSources[created.ID]
	fake.mu.Unlock()
	if stored == nil || stored.Configuration != `{"ref":"acme/billing"}` {
		t.Fatalf("stored task source = %#v, want the submitted configuration forwarded verbatim", stored)
	}
}

func TestCreateTaskSourceRequiresAdmin(t *testing.T) {
	mux, _ := startProjectServer(t)
	req := mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/task-sources",
		`{"provider":"github","name":"primary","enabled":true,"configuration":{"ref":"acme/billing"}}`,
		"viewer-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusForbidden {
		t.Errorf("got %d, want %d", rec.Code, http.StatusForbidden)
	}
}

// Editing a task source's name (or any unrelated field) must not blank a
// previously configured secret. This is the same property #294's own
// integration test guards server-side; this test guards the API layer's own
// part of the contract -- that an omitted `secrets` key travels to the
// orchestrator as omitted, never as an empty string.
func TestUpdateTaskSourceOmittingSecretsLeavesThemConfigured(t *testing.T) {
	mux, fake := startProjectServer(t)
	createReq := mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/task-sources",
		`{"provider":"github","name":"primary","enabled":true,"configuration":{"ref":"acme/billing"},"secrets":{"token":"ghp_secret"}}`,
		"admin-session")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(createRec.Body.Bytes(), &created); err != nil {
		t.Fatalf("decode create: %v", err)
	}

	// Edit the name only; the request body carries no "secrets" key at all.
	updateReq := mutateRequest(t, http.MethodPut, "/api/v1/task-sources/"+created.ID,
		`{"name":"renamed","enabled":true,"configuration":{"ref":"acme/billing"}}`,
		"admin-session")
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", updateRec.Code, updateRec.Body.String())
	}

	fake.mu.Lock()
	lastSecrets := fake.lastUpdateSecrets
	fake.mu.Unlock()
	if len(lastSecrets) != 0 {
		t.Fatalf("orchestrator received secrets=%v, want an empty/omitted map for an untouched field", lastSecrets)
	}

	var updated struct {
		Name    string `json:"name"`
		Secrets []struct {
			Key        string `json:"key"`
			Configured bool   `json:"configured"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode update: %v", err)
	}
	if updated.Name != "renamed" {
		t.Fatalf("name = %q, want renamed", updated.Name)
	}
	if len(updated.Secrets) != 1 || !updated.Secrets[0].Configured {
		t.Fatalf("secrets after unrelated edit = %+v, want token still configured", updated.Secrets)
	}
}

func TestUpdateTaskSourceEmptySecretValueIsFilteredNotForwarded(t *testing.T) {
	mux, fake := startProjectServer(t)
	createReq := mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/task-sources",
		`{"provider":"github","name":"primary","enabled":true,"configuration":{"ref":"acme/billing"},"secrets":{"token":"ghp_secret"}}`,
		"admin-session")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	// A form that round-tripped an empty secret input would send "" here; the
	// API must drop it before it ever reaches the orchestrator.
	updateReq := mutateRequest(t, http.MethodPut, "/api/v1/task-sources/"+created.ID,
		`{"name":"primary","enabled":true,"configuration":{"ref":"acme/billing"},"secrets":{"token":""}}`,
		"admin-session")
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", updateRec.Code, updateRec.Body.String())
	}

	fake.mu.Lock()
	lastSecrets := fake.lastUpdateSecrets
	fake.mu.Unlock()
	if _, present := lastSecrets["token"]; present {
		t.Fatalf("orchestrator received an empty token value in secrets=%v, want it filtered out", lastSecrets)
	}
}

func TestUpdateTaskSourceClearSecretsRemovesConfigured(t *testing.T) {
	mux, _ := startProjectServer(t)
	createReq := mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/task-sources",
		`{"provider":"github","name":"primary","enabled":true,"configuration":{"ref":"acme/billing"},"secrets":{"token":"ghp_secret"}}`,
		"admin-session")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	updateReq := mutateRequest(t, http.MethodPut, "/api/v1/task-sources/"+created.ID,
		`{"name":"primary","enabled":true,"configuration":{"ref":"acme/billing"},"clearSecrets":["token"]}`,
		"admin-session")
	updateRec := httptest.NewRecorder()
	mux.ServeHTTP(updateRec, updateReq)
	if updateRec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200: %s", updateRec.Code, updateRec.Body.String())
	}
	var updated struct {
		Secrets []struct {
			Key        string `json:"key"`
			Configured bool   `json:"configured"`
		} `json:"secrets"`
	}
	if err := json.Unmarshal(updateRec.Body.Bytes(), &updated); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(updated.Secrets) != 1 || updated.Secrets[0].Configured {
		t.Fatalf("secrets after clear = %+v, want token configured=false", updated.Secrets)
	}
}

func TestDeleteTaskSourceRemovesIt(t *testing.T) {
	mux, fake := startProjectServer(t)
	createReq := mutateRequest(t, http.MethodPost, "/api/v1/projects/p-1/task-sources",
		`{"provider":"github","name":"primary","enabled":true,"configuration":{"ref":"acme/billing"}}`,
		"admin-session")
	createRec := httptest.NewRecorder()
	mux.ServeHTTP(createRec, createReq)
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(createRec.Body.Bytes(), &created)

	req := mutateRequest(t, http.MethodDelete, "/api/v1/task-sources/"+created.ID, "", "admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)
	if rec.Code != http.StatusNoContent {
		t.Fatalf("got %d, want 204: %s", rec.Code, rec.Body.String())
	}
	fake.mu.Lock()
	_, ok := fake.taskSources[created.ID]
	fake.mu.Unlock()
	if ok {
		t.Fatal("task source still present after delete")
	}
}
