package server

import (
	"context"
	"crypto/rand"
	"encoding/json"
	"errors"
	"strings"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// taskSourceSecretKindPrefix namespaces a task-source secret field's
// generated credential kind away from the project-level kinds
// validateCredential accepts ("github_token", "ssh_private_key", "agent:*"):
// a task source's secret field and a project's own credential store share
// one table (app.project_credentials, see migration 026), and this prefix is
// what keeps the two kind spaces from ever colliding on the same string. A
// field can override this via Field.CredentialKind (see descriptor.go) to
// land on a pre-existing kind an adapter already reads directly, as GitHub's
// "token" field does with "github_token".
const taskSourceSecretKindPrefix = "source_field:"

// credentialKind is the app.project_credentials "kind" field's value is
// stored/read under: its own CredentialKind override when it declares one,
// or the generated default otherwise.
func credentialKind(field Field) string {
	if field.CredentialKind != "" {
		return field.CredentialKind
	}
	return taskSourceSecretKindPrefix + field.Key
}

// secretFields returns the subset of fields that are secret, keyed by Key,
// for quick membership checks against a request's secrets map.
func secretFields(fields []Field) map[string]Field {
	secrets := make(map[string]Field)
	for _, field := range fields {
		if field.IsSecret() {
			secrets[field.Key] = field
		}
	}
	return secrets
}

// credentialLister is the one method taskSourceProto/configuredSecretKeys
// need, satisfied by both *db.Queries and the narrower projectQuerier
// interface (server.go's project() builds a TaskSource list the same way,
// inside and outside of a transaction).
type credentialLister interface {
	ListProjectCredentials(context.Context, string) ([]db.ListProjectCredentialsRow, error)
}

// configuredSecretKeys reports which of fields' secret fields already have a
// value stored, by listing every credential this project has and matching
// each row's kind back to a field via credentialKind, scoped to sourceID. It
// never decrypts anything -- exactly the same "configured, never a value"
// shape ProjectCredential already commits to (see credentials.go's
// projectCredentials).
func configuredSecretKeys(ctx context.Context, queries credentialLister, projectID, sourceID string, fields []Field) (map[string]bool, error) {
	rows, err := queries.ListProjectCredentials(ctx, projectID)
	if err != nil {
		return nil, databaseError(err)
	}
	kindToKey := make(map[string]string, len(fields))
	for _, field := range fields {
		if field.IsSecret() {
			kindToKey[credentialKind(field)] = field.Key
		}
	}
	configured := make(map[string]bool)
	for _, row := range rows {
		scope, _ := row.TaskSourceID.(string)
		if scope != sourceID {
			continue
		}
		if key, ok := kindToKey[row.Kind]; ok {
			configured[key] = true
		}
	}
	return configured, nil
}

// taskSourceProto builds the controlv1.TaskSource a create/update/list/get
// response returns: configuration is exactly what is stored (never a
// secret, enforced at write time by validateConfiguration rejecting a
// secret key inside it), and secrets reports only which of this provider's
// declared secret fields are configured -- never a value, mirroring
// ProjectCredential's own convention.
func taskSourceProto(ctx context.Context, queries credentialLister, id, projectID, provider, name string, enabled bool, configuration string) (*controlv1.TaskSource, error) {
	proto := &controlv1.TaskSource{Id: id, Provider: provider, Name: name, Enabled: enabled, Configuration: configuration}
	taskType, ok := lookupTaskSourceType(provider)
	if !ok {
		// A source whose provider predates #294's descriptors (or was
		// migrated straight into the database) simply reports no secret
		// fields rather than erroring a read: see resolveTaskSource's own
		// "unrecognised provider falls back" precedent for why a legacy row
		// must never become unreadable.
		return proto, nil
	}
	secrets := secretFields(taskType.Fields())
	if len(secrets) == 0 {
		return proto, nil
	}
	configured, err := configuredSecretKeys(ctx, queries, projectID, id, taskType.Fields())
	if err != nil {
		return nil, err
	}
	for _, field := range taskType.Fields() {
		if !field.IsSecret() {
			continue
		}
		proto.Secrets = append(proto.Secrets, &controlv1.TaskSourceSecretField{Key: field.Key, Configured: configured[field.Key]})
	}
	return proto, nil
}

// ListTaskSourceTypes is the discovery RPC: every TaskSourceType descriptor
// this orchestrator knows, field by field, so the console can render a
// create/edit form with zero provider-specific code (see descriptor.go).
func (s *ControlServer) ListTaskSourceTypes(ctx context.Context, _ *controlv1.ListTaskSourceTypesRequest) (*controlv1.ListTaskSourceTypesResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	response := &controlv1.ListTaskSourceTypesResponse{}
	for _, taskType := range taskSourceTypes() {
		descriptor := &controlv1.TaskSourceTypeDescriptor{Id: taskType.ID(), DisplayName: taskType.DisplayName()}
		for _, field := range taskType.Fields() {
			protoField := &controlv1.TaskSourceField{
				Key: field.Key, Label: field.Label, Help: field.Help,
				Kind: field.Kind.String(), Required: field.Required,
				Options: field.Options, Pattern: field.Pattern,
			}
			if field.Default != nil {
				encoded, err := json.Marshal(field.Default)
				if err != nil {
					return nil, status.Error(codes.Internal, "encode field default")
				}
				protoField.DefaultValue = string(encoded)
			}
			descriptor.Fields = append(descriptor.Fields, protoField)
		}
		response.Types = append(response.Types, descriptor)
	}
	return response, nil
}

// CreateTaskSource validates request against its provider's descriptor
// (rejecting a missing required field, a secret smuggled into
// `configuration`, a bad enum value, ...) before ever writing a row, then
// routes every declared secret field's value to the encrypted credential
// store scoped to the new source's id rather than to `configuration`.
func (s *ControlServer) CreateTaskSource(ctx context.Context, request *controlv1.CreateTaskSourceRequest) (*controlv1.CreateTaskSourceResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetProjectId()) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	name := strings.TrimSpace(request.GetName())
	if name == "" || len(name) > 128 {
		return nil, status.Error(codes.InvalidArgument, "task source name is invalid")
	}
	taskType, ok := lookupTaskSourceType(request.GetProvider())
	if !ok {
		return nil, status.Errorf(codes.InvalidArgument, "unsupported task source provider %q", request.GetProvider())
	}
	config, err := decodeConfiguration(request.GetConfiguration())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	declaredSecrets := secretFields(taskType.Fields())
	providedSecrets := map[string]bool{}
	for key, value := range request.GetSecrets() {
		if strings.TrimSpace(value) == "" {
			continue
		}
		if _, declared := declaredSecrets[key]; !declared {
			return nil, status.Errorf(codes.InvalidArgument, "%q is not a secret field this provider accepts", key)
		}
		providedSecrets[key] = true
	}
	if err := validateConfiguration(taskType.Fields(), config, providedSecrets); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, status.Error(codes.Internal, "encode task source configuration")
	}
	id := idgen.NewID()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.CreateProjectTaskSource(ctx, db.CreateProjectTaskSourceParams{
		ID: id, ProjectID: request.GetProjectId(), Provider: taskType.ID(), Name: name, Enabled: request.GetEnabled(), Column6: encoded,
	}); err != nil {
		return nil, databaseError(err)
	}
	if err := writeTaskSourceSecrets(ctx, queries, request.GetProjectId(), id, request.GetSecrets(), declaredSecrets); err != nil {
		return nil, err
	}
	if err := audit(ctx, queries, actor.id, "task_source.create", "task_source", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	proto, err := taskSourceProto(ctx, s.queries, id, request.GetProjectId(), taskType.ID(), name, request.GetEnabled(), string(encoded))
	if err != nil {
		return nil, err
	}
	return &controlv1.CreateTaskSourceResponse{TaskSource: proto}, nil
}

// UpdateTaskSource never touches a secret field this request's `secrets` map
// omits: the whole reason it exists rather than round-tripping the create
// form is that an edit to name/enabled/an unrelated configuration field must
// not blank an already-configured secret (see the issue body's warning about
// getting this "wrong in the obvious way"). Only two things can change a
// secret: naming it in `secrets` with a new value, or naming its key in
// `clear_secrets`.
func (s *ControlServer) UpdateTaskSource(ctx context.Context, request *controlv1.UpdateTaskSourceRequest) (*controlv1.UpdateTaskSourceResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetTaskSourceId()) {
		return nil, status.Error(codes.InvalidArgument, "task source ID is invalid")
	}
	name := strings.TrimSpace(request.GetName())
	if name == "" || len(name) > 128 {
		return nil, status.Error(codes.InvalidArgument, "task source name is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	existing, err := existingTaskSource(ctx, queries, request.GetTaskSourceId())
	if err != nil {
		return nil, err
	}
	taskType, ok := lookupTaskSourceType(existing.Provider)
	if !ok {
		return nil, status.Errorf(codes.FailedPrecondition, "task source provider %q has no descriptor to validate against", existing.Provider)
	}
	config, err := decodeConfiguration(request.GetConfiguration())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	declaredSecrets := secretFields(taskType.Fields())
	clearing := make(map[string]bool, len(request.GetClearSecrets()))
	for _, key := range request.GetClearSecrets() {
		if _, declared := declaredSecrets[key]; !declared {
			return nil, status.Errorf(codes.InvalidArgument, "%q is not a secret field this provider accepts", key)
		}
		clearing[key] = true
	}
	replacing := map[string]bool{}
	for key, value := range request.GetSecrets() {
		if strings.TrimSpace(value) == "" {
			// Empty means "leave unchanged", not "clear it" -- clearing is
			// only ever explicit, via clear_secrets.
			continue
		}
		if _, declared := declaredSecrets[key]; !declared {
			return nil, status.Errorf(codes.InvalidArgument, "%q is not a secret field this provider accepts", key)
		}
		if clearing[key] {
			return nil, status.Errorf(codes.InvalidArgument, "%q cannot be both replaced and cleared in the same request", key)
		}
		replacing[key] = true
	}
	alreadyConfigured, err := configuredSecretKeys(ctx, queries, existing.ProjectID, request.GetTaskSourceId(), taskType.Fields())
	if err != nil {
		return nil, err
	}
	// The set validateConfiguration checks against is: still-configured
	// (untouched) OR newly replaced, MINUS explicitly cleared. This is the
	// one place an edit that never mentions a secret at all resolves to
	// "unchanged" rather than "absent".
	effectiveSecrets := map[string]bool{}
	for key := range alreadyConfigured {
		effectiveSecrets[key] = true
	}
	for key := range replacing {
		effectiveSecrets[key] = true
	}
	for key := range clearing {
		delete(effectiveSecrets, key)
	}
	if err := validateConfiguration(taskType.Fields(), config, effectiveSecrets); err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	encoded, err := json.Marshal(config)
	if err != nil {
		return nil, status.Error(codes.Internal, "encode task source configuration")
	}
	affected, err := queries.UpdateProjectTaskSource(ctx, db.UpdateProjectTaskSourceParams{
		ID: request.GetTaskSourceId(), ProjectID: existing.ProjectID, Name: name, Enabled: request.GetEnabled(), Column5: encoded,
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if affected != 1 {
		return nil, status.Error(codes.NotFound, "task source is unknown")
	}
	if err := writeTaskSourceSecrets(ctx, queries, existing.ProjectID, request.GetTaskSourceId(), request.GetSecrets(), declaredSecrets); err != nil {
		return nil, err
	}
	for key := range clearing {
		if _, err := queries.DeleteProjectCredentialForSource(ctx, db.DeleteProjectCredentialForSourceParams{
			ProjectID: existing.ProjectID, Kind: credentialKind(declaredSecrets[key]), TaskSourceID: uuidValue(request.GetTaskSourceId()),
		}); err != nil {
			return nil, databaseError(err)
		}
	}
	if err := audit(ctx, queries, actor.id, "task_source.update", "task_source", request.GetTaskSourceId()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	proto, err := taskSourceProto(ctx, s.queries, request.GetTaskSourceId(), existing.ProjectID, existing.Provider, name, request.GetEnabled(), string(encoded))
	if err != nil {
		return nil, err
	}
	return &controlv1.UpdateTaskSourceResponse{TaskSource: proto}, nil
}

// DeleteTaskSource removes a task source; app.project_credentials'
// task_source_id foreign key (ON DELETE CASCADE, migration 026) removes its
// secrets along with it, and app.issues/app.issue_sync_state cascade the
// same way, so no separate secret cleanup is needed here.
func (s *ControlServer) DeleteTaskSource(ctx context.Context, request *controlv1.DeleteTaskSourceRequest) (*controlv1.DeleteTaskSourceResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetTaskSourceId()) {
		return nil, status.Error(codes.InvalidArgument, "task source ID is invalid")
	}
	existing, err := existingTaskSource(ctx, s.queries, request.GetTaskSourceId())
	if err != nil {
		return nil, err
	}
	affected, err := s.queries.DeleteProjectTaskSource(ctx, db.DeleteProjectTaskSourceParams{ID: request.GetTaskSourceId(), ProjectID: existing.ProjectID})
	if err != nil {
		return nil, databaseError(err)
	}
	if affected != 1 {
		return nil, status.Error(codes.NotFound, "task source is unknown")
	}
	if err := audit(ctx, s.queries, actor.id, "task_source.delete", "task_source", request.GetTaskSourceId()); err != nil {
		return nil, err
	}
	return &controlv1.DeleteTaskSourceResponse{}, nil
}

// existingTaskSource looks up a task source by id alone (its project isn't
// known to the caller yet, unlike GetProjectTaskSource's two-argument
// lookup), for the update/delete handlers that only receive a
// task_source_id. app.project_task_sources.id is a global primary key, so
// this is unambiguous; every subsequent query still scopes by the project_id
// this returns.
func existingTaskSource(ctx context.Context, queries *db.Queries, id string) (db.GetProjectTaskSourceByIDRow, error) {
	row, err := queries.GetProjectTaskSourceByID(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return db.GetProjectTaskSourceByIDRow{}, status.Error(codes.NotFound, "task source is unknown")
	}
	if err != nil {
		return db.GetProjectTaskSourceByIDRow{}, databaseError(err)
	}
	return row, nil
}

// uuidValue converts an already-validated id string (idgen.ValidID) into the
// pgtype.UUID the source-scoped credential queries take for task_source_id.
// Scan on pgtype.UUID cannot fail for a string of that exact shape.
func uuidValue(id string) pgtype.UUID {
	var value pgtype.UUID
	_ = value.Scan(id)
	return value
}

// writeTaskSourceSecrets encrypts and upserts every non-empty, declared
// secret value in secrets, scoped to sourceID. Called by both
// CreateTaskSource and UpdateTaskSource so the two share one encryption
// path; an empty value is always "leave unchanged" here (both callers
// already filtered it out of what they pass as "declared", but the check is
// repeated defensively since this function has no other way to know a
// caller upheld that contract).
func writeTaskSourceSecrets(ctx context.Context, queries *db.Queries, projectID, sourceID string, secrets map[string]string, declared map[string]Field) error {
	if len(secrets) == 0 {
		return nil
	}
	aead, err := configuredCipher()
	if err != nil {
		return status.Error(codes.FailedPrecondition, err.Error())
	}
	for key, value := range secrets {
		if strings.TrimSpace(value) == "" {
			continue
		}
		field, ok := declared[key]
		if !ok {
			continue
		}
		nonce := make([]byte, aead.NonceSize())
		if _, err := rand.Read(nonce); err != nil {
			return status.Error(codes.Internal, "generate credential nonce")
		}
		ciphertext := aead.Seal(nil, nonce, []byte(value), nil)
		if _, err := queries.UpsertProjectCredentialForSource(ctx, db.UpsertProjectCredentialForSourceParams{
			ProjectID: projectID, Kind: credentialKind(field), TaskSourceID: uuidValue(sourceID),
			Ciphertext: ciphertext, Nonce: nonce,
		}); err != nil {
			return databaseError(err)
		}
	}
	return nil
}
