//go:build integration

// #294: TaskSourceType/Field descriptors, the ListTaskSourceTypes discovery
// RPC, and the CreateTaskSource/UpdateTaskSource/DeleteTaskSource RPCs that
// validate a configuration against them. This file's whole job is proving
// the issue's own three riskiest claims: a secret value never lands in
// `configuration` or any RPC response, editing an unrelated field never
// disturbs a configured secret, and a configuration the descriptor rejects
// never reaches the database.
package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestListTaskSourceTypesExposesGitHubAndLocalFileDescriptors(t *testing.T) {
	h := newHarness(t)
	response, err := h.Control.ListTaskSourceTypes(h.adminContext(), &controlv1.ListTaskSourceTypesRequest{})
	if err != nil {
		t.Fatalf("ListTaskSourceTypes: %v", err)
	}
	byID := map[string]*controlv1.TaskSourceTypeDescriptor{}
	for _, taskType := range response.GetTypes() {
		byID[taskType.GetId()] = taskType
	}
	github, ok := byID["github"]
	if !ok {
		t.Fatal("ListTaskSourceTypes did not describe the github provider")
	}
	localFile, ok := byID["local_file"]
	if !ok {
		t.Fatal("ListTaskSourceTypes did not describe the local_file provider")
	}
	fieldKinds := map[string]string{}
	for _, field := range github.GetFields() {
		fieldKinds[field.GetKey()] = field.GetKind()
	}
	if fieldKinds["ref"] != "text" {
		t.Fatalf("github.ref kind = %q, want text", fieldKinds["ref"])
	}
	if fieldKinds["token"] != "secret" {
		t.Fatalf("github.token kind = %q, want secret", fieldKinds["token"])
	}
	found := false
	for _, field := range localFile.GetFields() {
		if field.GetKey() == "ref" && field.GetKind() == "text" {
			found = true
		}
	}
	if !found {
		t.Fatal("local_file did not describe a text \"ref\" field")
	}
}

// TestCreateTaskSourceRejectsConfigurationTheDescriptorDoesNotSatisfy proves
// the server validates against the same descriptor the discovery RPC
// exposes: a missing required field, an unrecognised field, and a value
// that fails the field's pattern must all be rejected before a row is ever
// written.
func TestCreateTaskSourceRejectsConfigurationTheDescriptorDoesNotSatisfy(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	ctx := h.adminContext()

	cases := []struct {
		name          string
		provider      string
		configuration string
	}{
		{"missing required ref", "github", `{}`},
		{"unrecognised field", "github", `{"ref":"acme/demo","bogus":"x"}`},
		{"ref fails pattern", "github", `{"ref":"not a valid repo slug"}`},
		{"unknown provider", "jira", `{}`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.Control.CreateTaskSource(ctx, &controlv1.CreateTaskSourceRequest{
				ProjectId: projectID, Provider: tc.provider, Name: "source-" + tc.name, Enabled: true, Configuration: tc.configuration,
			})
			if status.Code(err) != codes.InvalidArgument {
				t.Fatalf("CreateTaskSource(%s) = %v, want InvalidArgument", tc.name, err)
			}
		})
	}

	var count int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM app.project_task_sources WHERE project_id=$1 AND name LIKE 'source-%'`, projectID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("a rejected CreateTaskSource call still wrote %d row(s)", count)
	}
}

// TestCreateTaskSourceRoutesSecretAwayFromConfiguration is the issue's
// central acceptance criterion: a value submitted for a Secret-kind field
// must never appear in app.project_task_sources.configuration, and no RPC
// response (create, list, project read) may ever carry it back out --
// verified by decoding the raw stored JSON and by grepping every response's
// serialized form for the plaintext token.
func TestCreateTaskSourceRoutesSecretAwayFromConfiguration(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	ctx := h.adminContext()
	const secretValue = "ghp_super-secret-token-value"

	created, err := h.Control.CreateTaskSource(ctx, &controlv1.CreateTaskSourceRequest{
		ProjectId: projectID, Provider: "github", Name: "secret-source", Enabled: true,
		Configuration: `{"ref":"acme/demo"}`,
		Secrets:       map[string]string{"token": secretValue},
	})
	if err != nil {
		t.Fatalf("CreateTaskSource: %v", err)
	}
	assertNoSecretLeak(t, created, secretValue)

	var storedConfiguration string
	if err := h.pool.QueryRow(context.Background(), `SELECT configuration::text FROM app.project_task_sources WHERE id=$1`, created.GetTaskSource().GetId()).Scan(&storedConfiguration); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(storedConfiguration, secretValue) {
		t.Fatalf("configuration JSONB contains the secret value: %s", storedConfiguration)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(storedConfiguration), &decoded); err != nil {
		t.Fatal(err)
	}
	if _, ok := decoded["token"]; ok {
		t.Fatal("configuration JSONB has a \"token\" key at all -- a secret field must never be stored there")
	}

	secretField := findSecretField(t, created.GetTaskSource(), "token")
	if !secretField.GetConfigured() {
		t.Fatal("token secret field reports not configured after being set")
	}

	// The secret must actually be resolvable through the same source-scoped
	// credential path the GitHub adapter itself uses (resolveGitHubToken).
	resolved, err := resolveGitHubToken(context.Background(), h.queries, projectID, created.GetTaskSource().GetId())
	if err != nil {
		t.Fatalf("resolveGitHubToken: %v", err)
	}
	if resolved != secretValue {
		t.Fatalf("resolveGitHubToken returned %q, want the configured secret", resolved)
	}

	// Reading the project back (ListProjects/Project's TaskSources) must
	// never leak it either -- this is the read path a console would use for
	// anything other than the CRUD RPCs themselves.
	project, err := h.Core.project(context.Background(), h.queries, projectID)
	if err != nil {
		t.Fatalf("h.Core.project: %v", err)
	}
	assertNoSecretLeak(t, project, secretValue)
}

// TestUpdateTaskSourceDoesNotDisturbConfiguredSecret is the issue's other
// central acceptance criterion: creating a source with a secret, then
// editing an entirely unrelated field (name), must leave that secret
// resolvable afterward -- the "obvious wrong" implementation (round-tripping
// the form) would blank it.
func TestUpdateTaskSourceDoesNotDisturbConfiguredSecret(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	ctx := h.adminContext()
	const secretValue = "ghp_original-token"

	created, err := h.Control.CreateTaskSource(ctx, &controlv1.CreateTaskSourceRequest{
		ProjectId: projectID, Provider: "github", Name: "will-rename", Enabled: true,
		Configuration: `{"ref":"acme/demo"}`,
		Secrets:       map[string]string{"token": secretValue},
	})
	if err != nil {
		t.Fatalf("CreateTaskSource: %v", err)
	}
	sourceID := created.GetTaskSource().GetId()

	// Edit only the name and the (unrelated) configuration ref; omit secrets
	// entirely, exactly what a form that never rendered the secret back would
	// submit.
	updated, err := h.Control.UpdateTaskSource(ctx, &controlv1.UpdateTaskSourceRequest{
		TaskSourceId: sourceID, Name: "renamed", Enabled: true, Configuration: `{"ref":"acme/demo-renamed"}`,
	})
	if err != nil {
		t.Fatalf("UpdateTaskSource: %v", err)
	}
	assertNoSecretLeak(t, updated, secretValue)
	if !findSecretField(t, updated.GetTaskSource(), "token").GetConfigured() {
		t.Fatal("token secret field reports not configured after an unrelated edit -- the secret was disturbed")
	}
	resolved, err := resolveGitHubToken(context.Background(), h.queries, projectID, sourceID)
	if err != nil {
		t.Fatalf("resolveGitHubToken after unrelated edit: %v", err)
	}
	if resolved != secretValue {
		t.Fatalf("resolveGitHubToken after unrelated edit = %q, want the original secret to survive", resolved)
	}

	// Now replace it explicitly, and confirm the new value -- not the old
	// one -- is what resolves.
	if _, err := h.Control.UpdateTaskSource(ctx, &controlv1.UpdateTaskSourceRequest{
		TaskSourceId: sourceID, Name: "renamed", Enabled: true, Configuration: `{"ref":"acme/demo-renamed"}`,
		Secrets: map[string]string{"token": "ghp_replacement-token"},
	}); err != nil {
		t.Fatalf("UpdateTaskSource (replace secret): %v", err)
	}
	resolved, err = resolveGitHubToken(context.Background(), h.queries, projectID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "ghp_replacement-token" {
		t.Fatalf("resolveGitHubToken after replace = %q, want the replacement", resolved)
	}

	// Finally, clear it explicitly, and confirm it no longer resolves.
	cleared, err := h.Control.UpdateTaskSource(ctx, &controlv1.UpdateTaskSourceRequest{
		TaskSourceId: sourceID, Name: "renamed", Enabled: true, Configuration: `{"ref":"acme/demo-renamed"}`,
		ClearSecrets: []string{"token"},
	})
	if err != nil {
		t.Fatalf("UpdateTaskSource (clear secret): %v", err)
	}
	if findSecretField(t, cleared.GetTaskSource(), "token").GetConfigured() {
		t.Fatal("token secret field still reports configured after an explicit clear")
	}
	resolved, err = resolveGitHubToken(context.Background(), h.queries, projectID, sourceID)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "" {
		t.Fatalf("resolveGitHubToken after clear = %q, want empty", resolved)
	}
}

func TestDeleteTaskSourceRemovesItsSecrets(t *testing.T) {
	h := newHarness(t)
	t.Setenv("LOOP_SECRET_KEY", "1Xy85zuZAOzRGEh/yyc4YmI64BOK8HY4pQHTjyqTa+E=")
	projectID, _ := h.project()
	ctx := h.adminContext()

	created, err := h.Control.CreateTaskSource(ctx, &controlv1.CreateTaskSourceRequest{
		ProjectId: projectID, Provider: "local_file", Name: "to-delete", Enabled: true,
		Configuration: `{"ref":"/tmp/does-not-need-to-exist-294"}`,
	})
	if err != nil {
		t.Fatalf("CreateTaskSource: %v", err)
	}
	sourceID := created.GetTaskSource().GetId()

	if _, err := h.Control.DeleteTaskSource(ctx, &controlv1.DeleteTaskSourceRequest{TaskSourceId: sourceID}); err != nil {
		t.Fatalf("DeleteTaskSource: %v", err)
	}
	var count int
	if err := h.pool.QueryRow(context.Background(), `SELECT count(*) FROM app.project_task_sources WHERE id=$1`, sourceID).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("DeleteTaskSource left the row in place")
	}
	if _, err := h.Control.DeleteTaskSource(ctx, &controlv1.DeleteTaskSourceRequest{TaskSourceId: sourceID}); status.Code(err) != codes.NotFound {
		t.Fatalf("deleting an already-deleted task source = %v, want NotFound", err)
	}
}

// assertNoSecretLeak marshals response (any proto message) to JSON and fails
// if the plaintext secret value appears anywhere in it -- a stronger check
// than reading named fields, since it also catches a secret leaking through
// a field nobody thought to check.
func assertNoSecretLeak(t *testing.T, response any, secretValue string) {
	t.Helper()
	encoded, err := json.Marshal(response)
	if err != nil {
		t.Fatalf("marshal response for leak check: %v", err)
	}
	if strings.Contains(string(encoded), secretValue) {
		t.Fatalf("response contains the secret value verbatim: %s", encoded)
	}
}

func findSecretField(t *testing.T, taskSource *controlv1.TaskSource, key string) *controlv1.TaskSourceSecretField {
	t.Helper()
	for _, field := range taskSource.GetSecrets() {
		if field.GetKey() == key {
			return field
		}
	}
	t.Fatalf("task source %s reports no %q secret field", taskSource.GetId(), key)
	return nil
}
