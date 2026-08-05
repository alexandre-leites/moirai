package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/secrethash"
	"github.com/loop-engineering/orchestrator/internal/textutil"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func (s *ControlServer) UpdateAccount(ctx context.Context, request *controlv1.UpdateAccountRequest) (*controlv1.UpdateAccountResponse, error) {
	actor, err := s.requireUserMutation(ctx)
	if err != nil {
		return nil, err
	}
	if request.GetNewPassword() != "" && !secrethash.ValidPassword(request.GetNewPassword()) {
		return nil, status.Error(codes.InvalidArgument, "new password is invalid")
	}
	if request.GetNewEmail() != "" && (len(request.GetNewEmail()) > 254 || !strings.Contains(request.GetNewEmail(), "@")) {
		return nil, status.Error(codes.InvalidArgument, "email is invalid")
	}
	if len(request.GetDisplayName()) > 128 {
		return nil, status.Error(codes.InvalidArgument, "display name is invalid")
	}
	md, _ := metadata.FromIncomingContext(ctx)
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	response := &controlv1.UpdateAccountResponse{UserId: actor.id, Role: actor.role}
	row, err := queries.GetUserForUpdate(ctx, actor.id)
	if err != nil {
		return nil, databaseError(err)
	}
	response.Username, response.Email, response.DisplayName = row.Username, row.Email, row.DisplayName
	storedHash := row.PasswordHash
	if request.GetNewPassword() != "" {
		matched, err := secrethash.PasswordMatches(request.GetCurrentPassword(), storedHash)
		if err != nil || !matched {
			return nil, status.Error(codes.Unauthenticated, "current password is incorrect")
		}
		storedHash, err = secrethash.PasswordHash(request.GetNewPassword())
		if err != nil {
			return nil, status.Error(codes.Internal, "hash password")
		}
		if err := queries.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{ID: actor.id, PasswordHash: storedHash}); err != nil {
			return nil, databaseError(err)
		}
		if err := queries.RevokeOtherUserSessions(ctx, db.RevokeOtherUserSessionsParams{UserID: actor.id, TokenHash: secrethash.HashSecret(md.Get(sessionHeader)[0])}); err != nil {
			return nil, databaseError(err)
		}
	}
	if request.GetNewEmail() != "" {
		response.Email = strings.TrimSpace(request.GetNewEmail())
	}
	if request.GetDisplayName() != "" {
		response.DisplayName = strings.TrimSpace(request.GetDisplayName())
	}
	if err := queries.UpdateUserProfile(ctx, db.UpdateUserProfileParams{ID: actor.id, Email: response.Email, DisplayName: response.DisplayName}); err != nil {
		return nil, databaseError(err)
	}
	if err := audit(ctx, queries, actor.id, "user.account.update", "user", actor.id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	return response, nil
}

func (s *ControlServer) CreateRunnerRegistrationToken(ctx context.Context, request *controlv1.CreateRunnerRegistrationTokenRequest) (*controlv1.CreateRunnerRegistrationTokenResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := normalizeLabels(request.GetAllowedLabels())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "runner token labels are invalid")
	}
	encodedLabels, err := jsonLabels(labels)
	if err != nil {
		return nil, status.Error(codes.Internal, "encode runner token labels")
	}
	token := idgen.RandomSecret()
	expiresAt, err := s.queries.CreateRunnerRegistrationToken(ctx, db.CreateRunnerRegistrationTokenParams{
		ID:              idgen.NewID(),
		TokenHash:       secrethash.HashSecret(token),
		CreatedByUserID: actor.id,
		AllowedLabels:   []byte(encodedLabels),
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if err := audit(ctx, s.queries, actor.id, "runner.token.create", "runner_registration_token", secrethash.HashSecret(token)); err != nil {
		return nil, err
	}
	return &controlv1.CreateRunnerRegistrationTokenResponse{Token: token, ExpiresAt: textutil.Timestamp(expiresAt.Time)}, nil
}

func (s *ControlServer) ListRunnerRegistrationTokens(ctx context.Context, _ *controlv1.ListRunnerRegistrationTokensRequest) (*controlv1.ListRunnerRegistrationTokensResponse, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListRunnerRegistrationTokens(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	response := &controlv1.ListRunnerRegistrationTokensResponse{}
	for _, row := range rows {
		token, err := scanRegistrationToken(row.ID, row.AllowedLabels, row.CreatedAt, row.ExpiresAt, row.UsedAt, row.RevokedAt)
		if err != nil {
			return nil, err
		}
		response.Tokens = append(response.Tokens, token)
	}
	return response, nil
}

func (s *ControlServer) RevokeRunnerRegistrationToken(ctx context.Context, request *controlv1.RevokeRunnerRegistrationTokenRequest) (*controlv1.RevokeRunnerRegistrationTokenResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetTokenId()) {
		return nil, status.Error(codes.InvalidArgument, "runner token ID is invalid")
	}
	row, err := s.queries.RevokeRunnerRegistrationToken(ctx, request.GetTokenId())
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "runner token is unknown")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	token, err := scanRegistrationToken(row.ID, row.AllowedLabels, row.CreatedAt, row.ExpiresAt, row.UsedAt, row.RevokedAt)
	if err != nil {
		return nil, err
	}
	if err := audit(ctx, s.queries, actor.id, "runner.token.revoke", "runner_registration_token", request.GetTokenId()); err != nil {
		return nil, err
	}
	return &controlv1.RevokeRunnerRegistrationTokenResponse{Token: token}, nil
}

// SubmitHumanDecision resolves a run sitting at StatusWaitingHuman: the
// console's decision panel (web/src/workflow-detail.tsx) calls this with
// decision "approved" or "changes_requested" once every automated gate
// (implementation, GitHub checks) has passed and the project opted into the
// human-approval gate (projectConfig.RequireHumanApproval). Any actor with a
// mutating session may call it, the same requirement RetryWorkflow/
// CancelWorkflow/BlockWorkflow use -- the decision panel itself is rendered
// admin-only (useIsAdmin in the console), so this mirrors rather than
// tightens that boundary.
func (s *ControlServer) SubmitHumanDecision(ctx context.Context, request *controlv1.SubmitHumanDecisionRequest) (*controlv1.SubmitHumanDecisionResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	id := request.GetWorkflowRunId()
	comment := strings.TrimSpace(request.GetComment())
	if !idgen.ValidID(id) || len(comment) > 1024 {
		return nil, status.Error(codes.InvalidArgument, "human decision request is invalid")
	}
	var action string
	switch request.GetDecision() {
	case "approved":
		action, err = "workflow.approve", s.approveWorkflow(ctx, id, comment)
	case "changes_requested":
		action, err = "workflow.reject", s.rejectWorkflow(ctx, id, comment)
	default:
		return nil, status.Error(codes.InvalidArgument, `decision must be "approved" or "changes_requested"`)
	}
	if errors.Is(err, errApprovalNotPending) {
		return nil, status.Error(codes.FailedPrecondition, err.Error())
	}
	if err != nil {
		return nil, err
	}
	if err := audit(ctx, s.queries, actor.id, action, "workflow_run", id); err != nil {
		return nil, err
	}
	workflow, err := s.workflow(ctx, id)
	if err != nil {
		return nil, err
	}
	return &controlv1.SubmitHumanDecisionResponse{Workflow: workflow}, nil
}

func (s *Core) requireAdmin(ctx context.Context) (actor, error) {
	current, err := s.requireActor(ctx, false)
	if err != nil {
		return actor{}, err
	}
	if current.role != "admin" {
		return actor{}, status.Error(codes.PermissionDenied, "administrator access is required")
	}
	return current, nil
}

func (s *Core) requireUserMutation(ctx context.Context) (actor, error) {
	current, err := s.requireActor(ctx, false)
	if err != nil {
		return actor{}, err
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get(csrfHeader)) != 1 {
		return actor{}, status.Error(codes.PermissionDenied, "CSRF token is invalid")
	}
	expected, err := s.queries.GetSessionCSRFHash(ctx, secrethash.HashSecret(md.Get(sessionHeader)[0]))
	if err != nil || subtle.ConstantTimeCompare([]byte(expected), []byte(secrethash.HashSecret(md.Get(csrfHeader)[0]))) != 1 {
		return actor{}, status.Error(codes.PermissionDenied, "CSRF token is invalid")
	}
	return current, nil
}

func scanRegistrationToken(id, labels string, created, expires, used, revoked pgtype.Timestamptz) (*controlv1.RunnerRegistrationToken, error) {
	token := &controlv1.RunnerRegistrationToken{Id: id}
	if err := json.Unmarshal([]byte(labels), &token.AllowedLabels); err != nil {
		return nil, configurationError(err)
	}
	token.CreatedAt, token.ExpiresAt = textutil.Timestamp(created.Time), textutil.Timestamp(expires.Time)
	if used.Valid {
		token.UsedAt = textutil.Timestamp(used.Time)
	}
	if revoked.Valid {
		token.RevokedAt = textutil.Timestamp(revoked.Time)
	}
	return token, nil
}
