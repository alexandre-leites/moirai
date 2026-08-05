// Task sources (#294/#345): the generic, provider-agnostic console form for
// GitHub, local_file, or any future TaskSourceType. Every field this API
// exposes here comes from the orchestrator's own descriptor
// (ListTaskSourceTypes) or from a TaskSource/TaskSourceField message it
// already returns -- this layer is a thin JSON translation of the gRPC
// contract, never a place that knows a provider by name.
package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/api/internal/auth"
	apiserver "github.com/loop-engineering/api/internal/http"
	"github.com/loop-engineering/api/internal/orchestrator"
)

type TaskSourceHandlers struct {
	client  *orchestrator.Client
	limiter *auth.RateLimiter
}

func NewTaskSourceHandlers(client *orchestrator.Client, limiter *auth.RateLimiter) *TaskSourceHandlers {
	return &TaskSourceHandlers{client: client, limiter: limiter}
}

func (h *TaskSourceHandlers) RegisterRoutes(mux *http.ServeMux) {
	mux.Handle("GET /api/v1/task-source-types", auth.RequireSession(http.HandlerFunc(h.listTypes)))
	mux.Handle("POST /api/v1/projects/{project_id}/task-sources", requireMutation(h.limiter, h.create))
	mux.Handle("PUT /api/v1/task-sources/{task_source_id}", requireMutation(h.limiter, h.update))
	mux.Handle("DELETE /api/v1/task-sources/{task_source_id}", requireMutation(h.limiter, h.delete))
}

// listTypes is the discovery endpoint the console renders every create/edit
// form from. Any signed-in session may read it (it names no project, and
// carries no secret) -- the same "read needs no admin role" rule the rest of
// this API applies (specification.md §3.2).
func (h *TaskSourceHandlers) listTypes(w http.ResponseWriter, r *http.Request) {
	resp, err := h.client.ListTaskSourceTypes(requestContext(r))
	if err != nil {
		writeClientError(w, err)
		return
	}
	types := make([]map[string]any, len(resp.GetTypes()))
	for i, t := range resp.GetTypes() {
		types[i] = taskSourceTypePayload(t)
	}
	apiserver.WriteJSON(w, http.StatusOK, map[string]any{"types": types})
}

func (h *TaskSourceHandlers) create(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Provider      string            `json:"provider"`
		Name          string            `json:"name"`
		Enabled       bool              `json:"enabled"`
		Configuration map[string]any    `json:"configuration"`
		Secrets       map[string]string `json:"secrets"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	configuration, err := encodeConfiguration(body.Configuration)
	if err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	resp, err := h.client.CreateTaskSource(requestContext(r), &controlv1.CreateTaskSourceRequest{
		ProjectId:     r.PathValue("project_id"),
		Provider:      body.Provider,
		Name:          body.Name,
		Enabled:       body.Enabled,
		Configuration: configuration,
		Secrets:       nonEmptySecrets(body.Secrets),
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusCreated, taskSourcePayload(resp.GetTaskSource()))
}

func (h *TaskSourceHandlers) update(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name          string            `json:"name"`
		Enabled       bool              `json:"enabled"`
		Configuration map[string]any    `json:"configuration"`
		Secrets       map[string]string `json:"secrets"`
		ClearSecrets  []string          `json:"clearSecrets"`
	}
	if err := decodeJSON(r, &body); err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	configuration, err := encodeConfiguration(body.Configuration)
	if err != nil {
		apiserver.WriteError(w, http.StatusBadRequest, "Invalid request body", err.Error())
		return
	}
	resp, err := h.client.UpdateTaskSource(requestContext(r), &controlv1.UpdateTaskSourceRequest{
		TaskSourceId:  r.PathValue("task_source_id"),
		Name:          body.Name,
		Enabled:       body.Enabled,
		Configuration: configuration,
		// Only a field the caller actually touched belongs here -- an
		// omitted key must reach the orchestrator as omitted, not as an
		// empty string, or an edit to an unrelated field would blank every
		// previously configured secret (see #294's UpdateTaskSource and the
		// issue's own warning about this exact failure mode).
		Secrets:      nonEmptySecrets(body.Secrets),
		ClearSecrets: body.ClearSecrets,
	})
	if err != nil {
		writeClientError(w, err)
		return
	}
	apiserver.WriteJSON(w, http.StatusOK, taskSourcePayload(resp.GetTaskSource()))
}

func (h *TaskSourceHandlers) delete(w http.ResponseWriter, r *http.Request) {
	_, err := h.client.DeleteTaskSource(requestContext(r), r.PathValue("task_source_id"))
	if err != nil {
		writeClientError(w, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// nonEmptySecrets drops a blank value before it ever reaches the request:
// the whole point of "empty means leave unchanged" is that a form field the
// operator never touched must not travel as an empty string that could be
// mistaken for an explicit (and rejected -- see CreateTaskSource) blank
// value. Never returns a nil map into a create for a provider with no
// secrets at all; that is fine, CreateTaskSourceRequest.Secrets is optional.
func nonEmptySecrets(secrets map[string]string) map[string]string {
	if len(secrets) == 0 {
		return nil
	}
	result := make(map[string]string, len(secrets))
	for key, value := range secrets {
		if strings.TrimSpace(value) == "" {
			continue
		}
		result[key] = value
	}
	return result
}

// encodeConfiguration turns the decoded JSON object the console submitted
// back into the JSON string CreateTaskSourceRequest/UpdateTaskSourceRequest
// carry. A request with no configuration key at all decodes to a nil map,
// which must still reach the orchestrator as "{}" -- an empty request body
// is never confused with "the field was omitted" the way a secret's absence
// is, because every non-secret field is always required to be present or
// explicitly defaulted by the descriptor.
func encodeConfiguration(configuration map[string]any) (string, error) {
	if configuration == nil {
		configuration = map[string]any{}
	}
	encoded, err := json.Marshal(configuration)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

func taskSourceTypePayload(t *controlv1.TaskSourceTypeDescriptor) map[string]any {
	fields := make([]map[string]any, len(t.GetFields()))
	for i, field := range t.GetFields() {
		fields[i] = taskSourceFieldPayload(field)
	}
	return map[string]any{
		"id":          t.GetId(),
		"displayName": t.GetDisplayName(),
		"fields":      fields,
	}
}

func taskSourceFieldPayload(field *controlv1.TaskSourceField) map[string]any {
	options := field.GetOptions()
	if options == nil {
		options = []string{}
	}
	payload := map[string]any{
		"key":      field.GetKey(),
		"label":    field.GetLabel(),
		"help":     field.GetHelp(),
		"kind":     field.GetKind(),
		"required": field.GetRequired(),
		"options":  options,
		"pattern":  field.GetPattern(),
	}
	// DefaultValue is JSON-encoded on the wire from the orchestrator (see
	// descriptor.go); decode it back to a real JSON value here so the
	// console never has to double-parse a string. Absent for every secret
	// field, and for any field with no declared default.
	if raw := field.GetDefaultValue(); raw != "" {
		var decoded any
		if err := json.Unmarshal([]byte(raw), &decoded); err == nil {
			payload["defaultValue"] = decoded
		}
	}
	return payload
}

func taskSourcePayload(ts *controlv1.TaskSource) map[string]any {
	if ts == nil {
		return nil
	}
	var configuration map[string]any
	if raw := ts.GetConfiguration(); raw != "" {
		_ = json.Unmarshal([]byte(raw), &configuration)
	}
	if configuration == nil {
		configuration = map[string]any{}
	}
	secrets := make([]map[string]any, len(ts.GetSecrets()))
	for i, secret := range ts.GetSecrets() {
		secrets[i] = map[string]any{"key": secret.GetKey(), "configured": secret.GetConfigured()}
	}
	return map[string]any{
		"id":            ts.GetId(),
		"provider":      ts.GetProvider(),
		"name":          ts.GetName(),
		"enabled":       ts.GetEnabled(),
		"configuration": configuration,
		"secrets":       secrets,
	}
}

func taskSourcesPayload(sources []*controlv1.TaskSource) []map[string]any {
	payload := make([]map[string]any, len(sources))
	for i, ts := range sources {
		payload[i] = taskSourcePayload(ts)
	}
	return payload
}
