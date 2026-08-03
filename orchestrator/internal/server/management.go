package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"github.com/loop-engineering/orchestrator/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func (s *Server) UpdateAccount(ctx context.Context, request *controlv1.UpdateAccountRequest) (*controlv1.UpdateAccountResponse, error) {
	actor, err := s.requireUserMutation(ctx)
	if err != nil {
		return nil, err
	}
	if request.GetNewPassword() != "" && !validPassword(request.GetNewPassword()) {
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
		matched, err := passwordMatches(request.GetCurrentPassword(), storedHash)
		if err != nil || !matched {
			return nil, status.Error(codes.Unauthenticated, "current password is incorrect")
		}
		storedHash, err = passwordHash(request.GetNewPassword())
		if err != nil {
			return nil, status.Error(codes.Internal, "hash password")
		}
		if err := queries.UpdateUserPassword(ctx, db.UpdateUserPasswordParams{ID: actor.id, PasswordHash: storedHash}); err != nil {
			return nil, databaseError(err)
		}
		if err := queries.RevokeOtherUserSessions(ctx, db.RevokeOtherUserSessionsParams{UserID: actor.id, TokenHash: hashSecret(md.Get(sessionHeader)[0])}); err != nil {
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
	if err := audit(ctx, tx, actor.id, "user.account.update", "user", actor.id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	return response, nil
}

func (s *Server) CreateRunnerRegistrationToken(ctx context.Context, request *controlv1.CreateRunnerRegistrationTokenRequest) (*controlv1.CreateRunnerRegistrationTokenResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	labels, err := normalizeLabels(request.GetAllowedLabels())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, "runner token labels are invalid")
	}
	token := randomSecret()
	expiresAt, err := s.queries.CreateRunnerRegistrationToken(ctx, db.CreateRunnerRegistrationTokenParams{
		ID:              newID(),
		TokenHash:       hashSecret(token),
		CreatedByUserID: actor.id,
		AllowedLabels:   []byte(jsonLabels(labels)),
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if err := audit(ctx, s.pool, actor.id, "runner.token.create", "runner_registration_token", hashSecret(token)); err != nil {
		return nil, err
	}
	return &controlv1.CreateRunnerRegistrationTokenResponse{Token: token, ExpiresAt: timestamp(expiresAt.Time)}, nil
}

func (s *Server) ListRunnerRegistrationTokens(ctx context.Context, _ *controlv1.ListRunnerRegistrationTokensRequest) (*controlv1.ListRunnerRegistrationTokensResponse, error) {
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

func (s *Server) RevokeRunnerRegistrationToken(ctx context.Context, request *controlv1.RevokeRunnerRegistrationTokenRequest) (*controlv1.RevokeRunnerRegistrationTokenResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(request.GetTokenId()) {
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
	if err := audit(ctx, s.pool, actor.id, "runner.token.revoke", "runner_registration_token", request.GetTokenId()); err != nil {
		return nil, err
	}
	return &controlv1.RevokeRunnerRegistrationTokenResponse{Token: token}, nil
}

func (s *Server) SubmitHumanDecision(ctx context.Context, _ *controlv1.SubmitHumanDecisionRequest) (*controlv1.SubmitHumanDecisionResponse, error) {
	if _, err := s.requireMutation(ctx); err != nil {
		return nil, err
	}
	return nil, status.Error(codes.FailedPrecondition, "V1 has no approval phase")
}

func (s *Server) requireAdmin(ctx context.Context) (actor, error) {
	current, err := s.requireActor(ctx, false)
	if err != nil {
		return actor{}, err
	}
	if current.role != "admin" {
		return actor{}, status.Error(codes.PermissionDenied, "administrator access is required")
	}
	return current, nil
}

func (s *Server) requireUserMutation(ctx context.Context) (actor, error) {
	current, err := s.requireActor(ctx, false)
	if err != nil {
		return actor{}, err
	}
	md, _ := metadata.FromIncomingContext(ctx)
	if len(md.Get(csrfHeader)) != 1 {
		return actor{}, status.Error(codes.PermissionDenied, "CSRF token is invalid")
	}
	expected, err := s.queries.GetSessionCSRFHash(ctx, hashSecret(md.Get(sessionHeader)[0]))
	if err != nil || subtle.ConstantTimeCompare([]byte(expected), []byte(hashSecret(md.Get(csrfHeader)[0]))) != 1 {
		return actor{}, status.Error(codes.PermissionDenied, "CSRF token is invalid")
	}
	return current, nil
}

func scanRegistrationToken(id, labels string, created, expires, used, revoked pgtype.Timestamptz) (*controlv1.RunnerRegistrationToken, error) {
	token := &controlv1.RunnerRegistrationToken{Id: id}
	if err := json.Unmarshal([]byte(labels), &token.AllowedLabels); err != nil {
		return nil, databaseError(err)
	}
	token.CreatedAt, token.ExpiresAt = timestamp(created.Time), timestamp(expires.Time)
	if used.Valid {
		token.UsedAt = timestamp(used.Time)
	}
	if revoked.Valid {
		token.RevokedAt = timestamp(revoked.Time)
	}
	return token, nil
}
