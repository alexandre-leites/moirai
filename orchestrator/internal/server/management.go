package server

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
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
	response := &controlv1.UpdateAccountResponse{UserId: actor.id, Role: actor.role}
	var storedHash string
	if err := tx.QueryRow(ctx, `SELECT username,password_hash,email,display_name FROM app.users WHERE id=$1 FOR UPDATE`, actor.id).Scan(&response.Username, &storedHash, &response.Email, &response.DisplayName); err != nil {
		return nil, databaseError(err)
	}
	if request.GetNewPassword() != "" {
		matched, err := passwordMatches(request.GetCurrentPassword(), storedHash)
		if err != nil || !matched {
			return nil, status.Error(codes.Unauthenticated, "current password is incorrect")
		}
		storedHash, err = passwordHash(request.GetNewPassword())
		if err != nil {
			return nil, status.Error(codes.Internal, "hash password")
		}
		if _, err := tx.Exec(ctx, `UPDATE app.users SET password_hash=$2,updated_at=now() WHERE id=$1`, actor.id, storedHash); err != nil {
			return nil, databaseError(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE app.user_sessions SET revoked_at=now() WHERE user_id=$1 AND token_hash<>$2 AND revoked_at IS NULL`, actor.id, hashSecret(md.Get(sessionHeader)[0])); err != nil {
			return nil, databaseError(err)
		}
	}
	if request.GetNewEmail() != "" {
		response.Email = strings.TrimSpace(request.GetNewEmail())
	}
	if request.GetDisplayName() != "" {
		response.DisplayName = strings.TrimSpace(request.GetDisplayName())
	}
	if _, err := tx.Exec(ctx, `UPDATE app.users SET email=$2,display_name=$3,updated_at=now() WHERE id=$1`, actor.id, response.Email, response.DisplayName); err != nil {
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
	var expiresAt time.Time
	err = s.pool.QueryRow(ctx, `INSERT INTO app.runner_registration_tokens(id,token_hash,created_by_user_id,allowed_labels,expires_at) VALUES($1,$2,$3,$4::jsonb,now()+interval '15 minutes') RETURNING expires_at`, newID(), hashSecret(token), actor.id, jsonLabels(labels)).Scan(&expiresAt)
	if err != nil {
		return nil, databaseError(err)
	}
	if err := audit(ctx, s.pool, actor.id, "runner.token.create", "runner_registration_token", hashSecret(token)); err != nil {
		return nil, err
	}
	return &controlv1.CreateRunnerRegistrationTokenResponse{Token: token, ExpiresAt: timestamp(expiresAt)}, nil
}

func (s *Server) ListRunnerRegistrationTokens(ctx context.Context, _ *controlv1.ListRunnerRegistrationTokensRequest) (*controlv1.ListRunnerRegistrationTokensResponse, error) {
	if _, err := s.requireAdmin(ctx); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text,allowed_labels::text,created_at,expires_at,used_at,revoked_at FROM app.runner_registration_tokens ORDER BY created_at DESC,id`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.ListRunnerRegistrationTokensResponse{}
	for rows.Next() {
		token, err := scanRegistrationToken(rows)
		if err != nil {
			return nil, err
		}
		response.Tokens = append(response.Tokens, token)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	row := s.pool.QueryRow(ctx, `UPDATE app.runner_registration_tokens SET revoked_at=COALESCE(revoked_at,now()) WHERE id=$1 RETURNING id::text,allowed_labels::text,created_at,expires_at,used_at,revoked_at`, request.GetTokenId())
	token, err := scanRegistrationTokenRow(row)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "runner token is unknown")
	}
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
	var expected string
	err = s.pool.QueryRow(ctx, `SELECT csrf_token_hash FROM app.user_sessions WHERE token_hash=$1 AND revoked_at IS NULL AND expires_at>now()`, hashSecret(md.Get(sessionHeader)[0])).Scan(&expected)
	if err != nil || subtle.ConstantTimeCompare([]byte(expected), []byte(hashSecret(md.Get(csrfHeader)[0]))) != 1 {
		return actor{}, status.Error(codes.PermissionDenied, "CSRF token is invalid")
	}
	return current, nil
}

func scanRegistrationToken(rows pgx.Rows) (*controlv1.RunnerRegistrationToken, error) {
	token, err := scanRegistrationTokenValues(rows)
	if err != nil {
		return nil, err
	}
	return token, nil
}

func scanRegistrationTokenRow(row pgx.Row) (*controlv1.RunnerRegistrationToken, error) {
	return scanRegistrationTokenValues(row)
}

func scanRegistrationTokenValues(row interface{ Scan(...any) error }) (*controlv1.RunnerRegistrationToken, error) {
	token := &controlv1.RunnerRegistrationToken{}
	var labels []byte
	var created, expires time.Time
	var used, revoked *time.Time
	if err := row.Scan(&token.Id, &labels, &created, &expires, &used, &revoked); err != nil {
		return nil, err
	}
	if err := json.Unmarshal(labels, &token.AllowedLabels); err != nil {
		return nil, databaseError(err)
	}
	token.CreatedAt, token.ExpiresAt = timestamp(created), timestamp(expires)
	if used != nil {
		token.UsedAt = timestamp(*used)
	}
	if revoked != nil {
		token.RevokedAt = timestamp(*revoked)
	}
	return token, nil
}
