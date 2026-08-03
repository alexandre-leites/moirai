package server

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"log/slog"
	"path"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/textutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
)

var agentCredentialName = regexp.MustCompile(`^[A-Z_][A-Z0-9_]{0,127}$`)

func (s *Server) SetProjectCredential(ctx context.Context, request *controlv1.SetProjectCredentialRequest) (*controlv1.SetProjectCredentialResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	kind, err := validateCredential(request.GetKind(), request.GetFilePath())
	if err != nil || !idgen.ValidID(request.GetProjectId()) || request.GetValue() == "" || len(request.GetValue()) > 64*1024 {
		return nil, status.Error(codes.InvalidArgument, "project credential is invalid")
	}
	aead, err := configuredCipher()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, status.Error(codes.Internal, "generate credential nonce")
	}
	ciphertext := aead.Seal(nil, nonce, []byte(request.GetValue()), nil)
	rowsAffected, err := s.queries.UpsertProjectCredential(ctx, db.UpsertProjectCredentialParams{
		ProjectID:  request.GetProjectId(),
		Kind:       kind,
		Ciphertext: ciphertext,
		Nonce:      nonce,
		FilePath:   request.GetFilePath(),
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if rowsAffected != 1 {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	if err := audit(ctx, s.queries, actor.id, "project.credential.set", "project", request.GetProjectId()); err != nil {
		return nil, err
	}
	credentials, err := s.projectCredentials(ctx, request.GetProjectId())
	if err != nil {
		return nil, err
	}
	return &controlv1.SetProjectCredentialResponse{Credentials: credentials}, nil
}

func (s *Server) ClearProjectCredential(ctx context.Context, request *controlv1.ClearProjectCredentialRequest) (*controlv1.ClearProjectCredentialResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	kind, err := validateCredential(request.GetKind(), "")
	if err != nil || !idgen.ValidID(request.GetProjectId()) {
		return nil, status.Error(codes.InvalidArgument, "project credential is invalid")
	}
	rowsAffected, err := s.queries.DeleteProjectCredential(ctx, db.DeleteProjectCredentialParams{
		ProjectID: request.GetProjectId(),
		Kind:      kind,
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if rowsAffected > 0 {
		if err := audit(ctx, s.queries, actor.id, "project.credential.clear", "project", request.GetProjectId()); err != nil {
			return nil, err
		}
	}
	credentials, err := s.projectCredentials(ctx, request.GetProjectId())
	if err != nil {
		return nil, err
	}
	return &controlv1.ClearProjectCredentialResponse{Credentials: credentials}, nil
}

func (s *Server) ListProjectCredentials(ctx context.Context, request *controlv1.ListProjectCredentialsRequest) (*controlv1.ListProjectCredentialsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	credentials, err := s.projectCredentials(ctx, request.GetProjectId())
	if err != nil {
		return nil, err
	}
	return &controlv1.ListProjectCredentialsResponse{Credentials: credentials}, nil
}

func (s *Server) ResolveJobSecret(ctx context.Context, request *runnerv1.ResolveJobSecretRequest) (*runnerv1.ResolveJobSecretResponse, error) {
	if !secureContext(ctx) {
		return nil, status.Error(codes.FailedPrecondition, "the control plane will not serve a secret over an insecure channel")
	}
	if !idgen.ValidID(request.GetRunnerId()) || !idgen.ValidID(request.GetJobId()) || request.GetCredential() == "" || request.GetLeaseGeneration() < 1 {
		return nil, status.Error(codes.InvalidArgument, "secret resolution request is invalid")
	}
	if err := s.authenticateRunner(ctx, request.GetRunnerId(), request.GetCredential()); err != nil {
		return nil, err
	}
	kind, err := environmentCredentialKind(request.GetName())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}
	projectID, err := s.fencedJobProject(ctx, request.GetJobId(), request.GetRunnerId(), request.GetLeaseGeneration())
	if err != nil {
		return nil, err
	}
	secret, err := s.queries.GetProjectCredentialSecret(ctx, db.GetProjectCredentialSecretParams{
		ProjectID: projectID,
		Kind:      kind,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "no credential is configured for this project")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	aead, err := configuredCipher()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	plaintext, err := aead.Open(nil, secret.Nonce, secret.Ciphertext, nil)
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, "stored secret could not be opened")
	}
	delivery := "environment"
	if kind == "ssh_private_key" || secret.FilePath != "" {
		delivery = "file"
	}
	return &runnerv1.ResolveJobSecretResponse{Value: string(plaintext), Delivery: delivery}, nil
}

func (s *Server) StoreJobSecret(ctx context.Context, request *runnerv1.StoreJobSecretRequest) (*runnerv1.StoreJobSecretResponse, error) {
	if !secureContext(ctx) {
		return nil, status.Error(codes.FailedPrecondition, "the control plane will not accept a secret over an insecure channel")
	}
	if !idgen.ValidID(request.GetRunnerId()) || !idgen.ValidID(request.GetJobId()) || request.GetCredential() == "" || request.GetLeaseGeneration() < 1 || request.GetValue() == "" || len(request.GetValue()) > 64*1024 {
		return nil, status.Error(codes.InvalidArgument, "secret storage request is invalid")
	}
	if err := s.authenticateRunner(ctx, request.GetRunnerId(), request.GetCredential()); err != nil {
		return nil, err
	}
	kind, err := environmentCredentialKind(request.GetName())
	if err != nil || !strings.HasPrefix(kind, "agent:") {
		return nil, status.Error(codes.InvalidArgument, "secret is not a rotatable agent credential")
	}
	projectID, err := s.fencedJobProject(ctx, request.GetJobId(), request.GetRunnerId(), request.GetLeaseGeneration())
	if err != nil {
		return nil, err
	}
	aead, err := configuredCipher()
	if err != nil {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, status.Error(codes.Internal, "generate credential nonce")
	}
	ciphertext := aead.Seal(nil, nonce, []byte(request.GetValue()), nil)
	rowsAffected, err := s.queries.UpdateProjectCredentialSecret(ctx, db.UpdateProjectCredentialSecretParams{
		ProjectID:  projectID,
		Kind:       kind,
		Ciphertext: ciphertext,
		Nonce:      nonce,
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if rowsAffected == 1 {
		// Unlike SetProjectCredential/ClearProjectCredential, this mutation is
		// initiated by a runner rotating its own agent credential rather than
		// by an admin, so the runner (not a user) is recorded as the actor --
		// it is otherwise the one credential write that leaves no audit trail.
		if err := audit(ctx, s.queries, request.GetRunnerId(), "project.credential.rotated", "project", projectID); err != nil {
			return nil, err
		}
	}
	return &runnerv1.StoreJobSecretResponse{Stored: rowsAffected == 1}, nil
}

func (s *Server) fencedJobProject(ctx context.Context, jobID, runnerID string, generation int64) (string, error) {
	projectID, err := s.queries.GetFencedJobProject(ctx, db.GetFencedJobProjectParams{
		JobID:           jobID,
		RunnerID:        runnerID,
		LeaseGeneration: generation,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", status.Error(codes.FailedPrecondition, "runner does not hold this job")
	}
	if err != nil {
		return "", databaseError(err)
	}
	return projectID, nil
}

func (s *Server) projectCredentials(ctx context.Context, projectID string) ([]*controlv1.ProjectCredential, error) {
	if !idgen.ValidID(projectID) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	exists, err := s.queries.ProjectExists(ctx, projectID)
	if err != nil {
		return nil, databaseError(err)
	}
	if !exists {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	rows, err := s.queries.ListProjectCredentials(ctx, projectID)
	if err != nil {
		return nil, databaseError(err)
	}
	credentials := []*controlv1.ProjectCredential{}
	for _, row := range rows {
		credentials = append(credentials, &controlv1.ProjectCredential{
			Kind:      row.Kind,
			CreatedAt: textutil.Timestamp(row.CreatedAt.Time),
			UpdatedAt: textutil.Timestamp(row.UpdatedAt.Time),
			FilePath:  row.FilePath,
		})
	}
	return credentials, nil
}

func configuredCipher() (cipher.AEAD, error) {
	value, configured, err := optionalSecret("LOOP_SECRET_KEY")
	if err != nil {
		// Distinct from the "not configured" case below: this means the
		// operator did set LOOP_SECRET_KEY (or _FILE), but it's unusable
		// (both set, unreadable file, wrong file mode, oversized). Collapsing
		// this into "is required" tells an operator who mounted the secret
		// with the wrong ownership that the variable is unset, when it isn't.
		slog.Error("LOOP_SECRET_KEY is misconfigured", "error", err)
		return nil, errors.New("LOOP_SECRET_KEY is misconfigured")
	}
	if !configured {
		return nil, errors.New("LOOP_SECRET_KEY is required for project credentials")
	}
	// Try both encodings rather than first-parse-wins: a hex-encoded 32-byte
	// key is 64 characters, a multiple of 4, and so also parses as valid
	// (but wrong-length) base64. Accept whichever decoding yields exactly 32
	// bytes; if a value happens to decode to 32 bytes under both encodings,
	// prefer base64 since it was the historically accepted encoding.
	var key []byte
	if decoded, err := base64.StdEncoding.DecodeString(value); err == nil && len(decoded) == 32 {
		key = decoded
	} else if decoded, err := hex.DecodeString(value); err == nil && len(decoded) == 32 {
		key = decoded
	}
	if len(key) != 32 {
		return nil, errors.New("secret key must decode to 32 bytes from base64 or hex")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}

// reservedCredentialNames are environment variables that change how a process
// or its loader behaves rather than carrying a secret. An agent credential is
// injected into the execution environment by name, so allowing these would turn
// the credential store into a way to point the agent's toolchain at a different
// binary or preload a library into everything it runs.
var reservedCredentialNames = map[string]bool{
	"HOME": true, "PATH": true, "TMPDIR": true, "SHELL": true, "IFS": true,
	"LD_PRELOAD": true, "LD_LIBRARY_PATH": true, "LD_AUDIT": true, "DYLD_INSERT_LIBRARIES": true, "DYLD_LIBRARY_PATH": true,
	"GIT_SSH_COMMAND": true, "GIT_EXTERNAL_DIFF": true, "BASH_ENV": true, "ENV": true, "NODE_OPTIONS": true,
}

func validCredentialName(name string) bool {
	return agentCredentialName.MatchString(name) && !reservedCredentialNames[name]
}

func validateCredential(kind, filePath string) (string, error) {
	kind = strings.TrimSpace(kind)
	if kind != "github_token" && kind != "ssh_private_key" && !(strings.HasPrefix(kind, "agent:") && validCredentialName(strings.TrimPrefix(kind, "agent:"))) {
		return "", errors.New("credential kind is invalid")
	}
	if filePath == "" {
		return kind, nil
	}
	if !strings.HasPrefix(kind, "agent:") || len(filePath) > 256 || strings.TrimSpace(filePath) != filePath || strings.ContainsAny(filePath, " \t\r\n") || path.IsAbs(filePath) || path.Clean(filePath) != filePath || strings.HasPrefix(filePath, "~") || strings.Contains(filePath, "\\") || strings.Contains(filePath, "//") || strings.HasSuffix(filePath, "/") || strings.HasPrefix(filePath, "../") || filePath == "." || filePath == ".." {
		return "", errors.New("credential file path is invalid")
	}
	return kind, nil
}

func environmentCredentialKind(name string) (string, error) {
	name = strings.TrimSpace(name)
	switch name {
	case "GITHUB_TOKEN":
		return "github_token", nil
	case "GIT_SSH_KEY":
		return "ssh_private_key", nil
	}
	if !validCredentialName(name) {
		return "", errors.New("credential environment reference is invalid")
	}
	return "agent:" + name, nil
}

func secureContext(ctx context.Context) bool {
	peer, ok := peer.FromContext(ctx)
	if !ok {
		return false
	}
	_, ok = peer.AuthInfo.(credentials.TLSInfo)
	return ok
}
