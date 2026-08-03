package server

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/metrics"
	"golang.org/x/crypto/scrypt"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

const (
	sessionHeader = "x-loop-session"
	csrfHeader    = "x-loop-csrf"
	leaseDuration = 10 * time.Minute
)

type Server struct {
	controlv1.UnimplementedControlPlaneServer
	runnerv1.UnimplementedRunnerControlServer
	pool     *pgxpool.Pool
	queries  *db.Queries
	version  string
	github   GitHub
	sessions map[string]chan *runnerv1.OrchestratorToRunner
	mu       sync.Mutex
	// shutdown is closed exactly once, by Shutdown, to tell every long-lived
	// stream handler (Connect, StreamEvents) to return promptly instead of
	// blocking on its own stream context forever. gRPC's GracefulStop waits
	// for in-flight RPCs to finish and never cancels a server-stream's
	// context itself, so without this signal a connected runner — which
	// holds Connect open indefinitely by design — would keep GracefulStop
	// from ever returning.
	shutdown     chan struct{}
	shutdownOnce sync.Once
}

type actor struct {
	id   string
	role string
}

type projectConfig struct {
	Labels         []string `json:"required_runner_labels"`
	ExecutionImage string   `json:"execution_image"`
}

func New(pool *pgxpool.Pool, version string) (*Server, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	queries := db.New(pool)
	github := NewGitHubCLI(nil, func(ctx context.Context, projectID string) (string, error) {
		return resolveGitHubToken(ctx, queries, projectID)
	})
	return NewWithGitHub(pool, version, github)
}

func NewWithGitHub(pool *pgxpool.Pool, version string, github GitHub) (*Server, error) {
	if pool == nil || github == nil {
		return nil, errors.New("server dependencies are required")
	}
	return &Server{pool: pool, queries: db.New(pool), version: version, github: github, sessions: make(map[string]chan *runnerv1.OrchestratorToRunner), shutdown: make(chan struct{})}, nil
}

// Shutdown tells every stream handler currently blocked on Connect or
// StreamEvents to return. Safe to call more than once and from any
// goroutine; only the first call has an effect. It must run before (or
// concurrently with, never after) grpc.Server.GracefulStop — GracefulStop
// blocks until in-flight RPCs finish, and both stream handlers only finish
// early because they observe this signal, so calling it after GracefulStop
// has already started blocking would deadlock the very shutdown it exists to
// unblock.
func (s *Server) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

// withShutdown returns a context derived from parent that is also cancelled
// when the server starts shutting down, plus its cancel func. Callers must
// still call parent's own cancellation/Done handling as before; this only
// adds the extra trigger. The goroutine it starts exits as soon as either
// signal fires, so it never outlives the returned context.
func (s *Server) withShutdown(parent context.Context) (context.Context, context.CancelFunc) {
	ctx, cancel := context.WithCancel(parent)
	go func() {
		select {
		case <-s.shutdown:
			cancel()
		case <-ctx.Done():
		}
	}()
	return ctx, cancel
}

func (s *Server) Bootstrap(ctx context.Context) error {
	userCount, err := s.queries.CountUsers(ctx)
	if err != nil {
		return databaseError(err)
	}
	if userCount == 0 {
		password, configured, err := optionalSecret("LOOP_INITIAL_ADMIN_PASSWORD")
		if err != nil {
			return err
		}
		if configured {
			username := strings.TrimSpace(os.Getenv("LOOP_INITIAL_ADMIN_USERNAME"))
			if username == "" {
				username = "admin"
			}
			if len(username) > 128 || !validPassword(password) {
				return errors.New("initial administrator configuration is invalid")
			}
			hash, err := passwordHash(password)
			if err != nil {
				return err
			}
			if err := s.queries.CreateAdminUser(ctx, db.CreateAdminUserParams{ID: newID(), Username: username, PasswordHash: hash}); err != nil {
				return databaseError(err)
			}
		}
	}
	token, configured, err := optionalSecret("RUNNER_REGISTRATION_TOKEN")
	if err != nil {
		return err
	}
	if !configured {
		return nil
	}
	labels := []string{}
	for _, label := range strings.Split(os.Getenv("LOOP_SEED_TOKEN_LABELS"), ",") {
		if label = strings.TrimSpace(label); label != "" {
			labels = append(labels, label)
		}
	}
	// An explicitly set but empty list means the operator supplied separators
	// and nothing else. Storing [] would make the token match no runner at all,
	// and the resulting rejection names neither the token nor the labels.
	if len(labels) == 0 {
		labels = []string{"linux"}
	}
	if _, err := normalizeLabels(labels); err != nil {
		return errors.New("seed runner token labels are invalid")
	}
	// Re-arm the expiry on every start, rather than DO NOTHING. The token hash
	// is derived from a configured value and so is stable across restarts, so
	// the row survives while its 15-minute expiry does not: with DO NOTHING the
	// second boot leaves an expired row in place and every runner registration
	// is refused with a message that suggests a wrong token. A token that has
	// already been redeemed keeps its used_at and stays redeemed.
	err = s.queries.UpsertSeedRunnerRegistrationToken(ctx, db.UpsertSeedRunnerRegistrationTokenParams{ID: newID(), TokenHash: hashSecret(token), Column3: []byte(jsonLabels(labels))})
	return databaseError(err)
}

func (s *Server) Login(ctx context.Context, request *controlv1.LoginRequest) (*controlv1.LoginResponse, error) {
	username := strings.TrimSpace(request.GetUsername())
	if username == "" || len(username) > 128 || request.GetPassword() == "" {
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	row, err := s.queries.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = passwordMatches(request.GetPassword(), "scrypt$16384$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	userID, encoded, enabled := row.ID, row.PasswordHash, row.Enabled
	matches, err := passwordMatches(request.GetPassword(), encoded)
	if err != nil {
		// A genuine scrypt failure, not a wrong password: log it so it isn't
		// silently indistinguishable from bad credentials.
		slog.Error("password verification failed", "error", err, "user_id", userID)
	}
	if err != nil || !enabled || !matches {
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	sessionToken, csrfToken := randomSecret(), randomSecret()
	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	if err := s.queries.CreateUserSession(ctx, db.CreateUserSessionParams{ID: newID(), UserID: userID, TokenHash: hashSecret(sessionToken), CsrfTokenHash: hashSecret(csrfToken), ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true}}); err != nil {
		return nil, databaseError(err)
	}
	return &controlv1.LoginResponse{SessionToken: sessionToken, UserId: userID, CsrfToken: csrfToken}, nil
}

func (s *Server) WhoAmI(ctx context.Context, _ *controlv1.WhoAmIRequest) (*controlv1.WhoAmIResponse, error) {
	current, err := s.requireActor(ctx, false)
	if err != nil {
		return nil, err
	}
	response := &controlv1.WhoAmIResponse{UserId: current.id, Role: current.role}
	row, err := s.queries.GetUserProfile(ctx, current.id)
	if err != nil {
		return nil, databaseError(err)
	}
	response.Username, response.Email, response.DisplayName = row.Username, row.Email, row.DisplayName
	return response, nil
}

func (s *Server) Logout(ctx context.Context, _ *controlv1.LogoutRequest) (*controlv1.LogoutResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get(sessionHeader)) != 1 || len(md.Get(csrfHeader)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "session is required")
	}
	affected, err := s.queries.RevokeSessionByTokens(ctx, db.RevokeSessionByTokensParams{TokenHash: hashSecret(md.Get(sessionHeader)[0]), CsrfTokenHash: hashSecret(md.Get(csrfHeader)[0])})
	if err != nil {
		return nil, databaseError(err)
	}
	if affected != 1 {
		return nil, status.Error(codes.Unauthenticated, "session is invalid")
	}
	return &controlv1.LogoutResponse{}, nil
}

func (s *Server) ListProjects(ctx context.Context, _ *controlv1.ListProjectsRequest) (*controlv1.ListProjectsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	ids, err := s.queries.ListProjectIDs(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	response := &controlv1.ListProjectsResponse{}
	for _, id := range ids {
		project, err := s.project(ctx, s.queries, id)
		if err != nil {
			return nil, err
		}
		response.Projects = append(response.Projects, project)
	}
	return response, nil
}

func (s *Server) CreateProject(ctx context.Context, request *controlv1.CreateProjectRequest) (*controlv1.CreateProjectResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	cfg, steps, err := validateProject(request.GetProject())
	if err != nil {
		return nil, err
	}
	id := newID()
	encoded, _ := json.Marshal(projectConfig{Labels: cfg.GetRequiredRunnerLabels(), ExecutionImage: cfg.GetExecutionImage()})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.CreateProject(ctx, db.CreateProjectParams{
		ID: id, Name: cfg.GetName(), RepositoryMode: cfg.GetRepositoryMode(),
		Column4: cfg.GetRepositoryUrl(), Column5: cfg.GetLocalRepositoryPath(),
		DefaultBranch: cfg.GetDefaultBranch(), Column7: encoded,
	}); err != nil {
		return nil, databaseError(err)
	}
	if err := replacePipelineSteps(ctx, queries, id, steps); err != nil {
		return nil, err
	}
	if err := audit(ctx, queries, actor.id, "project.create", "project", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	project, err := s.project(ctx, s.queries, id)
	if err != nil {
		return nil, err
	}
	return &controlv1.CreateProjectResponse{Project: project}, nil
}

func (s *Server) UpdateProject(ctx context.Context, request *controlv1.UpdateProjectRequest) (*controlv1.UpdateProjectResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(request.GetProjectId()) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	cfg, steps, err := validateProject(request.GetProject())
	if err != nil {
		return nil, err
	}
	encoded, _ := json.Marshal(projectConfig{Labels: cfg.GetRequiredRunnerLabels(), ExecutionImage: cfg.GetExecutionImage()})
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	affected, err := queries.UpdateProject(ctx, db.UpdateProjectParams{
		ID: request.GetProjectId(), Name: cfg.GetName(), RepositoryMode: cfg.GetRepositoryMode(),
		Column4: cfg.GetRepositoryUrl(), Column5: cfg.GetLocalRepositoryPath(),
		DefaultBranch: cfg.GetDefaultBranch(), Column7: encoded,
	})
	if err != nil {
		return nil, databaseError(err)
	}
	if affected != 1 {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	if err := replacePipelineSteps(ctx, queries, request.GetProjectId(), steps); err != nil {
		return nil, err
	}
	if err := audit(ctx, queries, actor.id, "project.update", "project", request.GetProjectId()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	project, err := s.project(ctx, s.queries, request.GetProjectId())
	if err != nil {
		return nil, err
	}
	return &controlv1.UpdateProjectResponse{Project: project}, nil
}

func (s *Server) SetProjectEnabled(ctx context.Context, request *controlv1.SetProjectEnabledRequest) (*controlv1.SetProjectEnabledResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(request.GetProjectId()) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	affected, err := s.queries.SetProjectEnabled(ctx, db.SetProjectEnabledParams{ID: request.GetProjectId(), Enabled: request.GetEnabled()})
	if err != nil {
		return nil, databaseError(err)
	}
	if affected != 1 {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	if err := audit(ctx, s.queries, actor.id, "project.enabled", "project", request.GetProjectId()); err != nil {
		return nil, err
	}
	project, err := s.project(ctx, s.queries, request.GetProjectId())
	if err != nil {
		return nil, err
	}
	return &controlv1.SetProjectEnabledResponse{Project: project}, nil
}

// listWorkflowsLimit caps ListWorkflows to the most recently created runs.
// The endpoint had no LIMIT at all, so it grew with every workflow ever run,
// running one join query per row (see workflowFromDetailRow). The console
// only ever renders this list unpaginated (web/src/console-data.tsx polls it
// whole, web/src/workflows.tsx filters/searches client-side over whatever
// comes back) so a fixed cap -- rather than cursor pagination, which would
// need a new proto field plus client changes to be useful -- is the
// low-risk fix: 500 comfortably covers what an operator would actually
// filter or search through in one sitting, matching the existing 500-row
// ceiling ListWorkflowEvents already uses for the same reason.
const listWorkflowsLimit = 500

func (s *Server) ListWorkflows(ctx context.Context, _ *controlv1.ListWorkflowsRequest) (*controlv1.ListWorkflowsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListWorkflowsPage(ctx, listWorkflowsLimit)
	if err != nil {
		return nil, databaseError(err)
	}
	response := &controlv1.ListWorkflowsResponse{}
	for _, row := range rows {
		response.Workflows = append(response.Workflows, workflowFromDetailRow(db.GetWorkflowDetailRow(row)))
	}
	return response, nil
}

func (s *Server) GetWorkflow(ctx context.Context, request *controlv1.GetWorkflowRequest) (*controlv1.GetWorkflowResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	workflow, err := s.workflow(ctx, request.GetWorkflowRunId())
	if err != nil {
		return nil, err
	}
	return &controlv1.GetWorkflowResponse{Workflow: workflow}, nil
}

func (s *Server) ListWorkflowEvents(ctx context.Context, request *controlv1.ListWorkflowEventsRequest) (*controlv1.ListWorkflowEventsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	if !validID(request.GetWorkflowRunId()) || request.GetAfterId() < 0 {
		return nil, status.Error(codes.InvalidArgument, "workflow event request is invalid")
	}
	limit := request.GetLimit()
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 500 {
		return nil, status.Error(codes.InvalidArgument, "event limit must be between 1 and 500")
	}
	rows, err := s.queries.ListWorkflowEvents(ctx, db.ListWorkflowEventsParams{WorkflowRunID: request.GetWorkflowRunId(), ID: request.GetAfterId(), Limit: limit})
	if err != nil {
		return nil, databaseError(err)
	}
	response := &controlv1.ListWorkflowEventsResponse{}
	var last int64
	for _, row := range rows {
		event := controlv1.WorkflowEvent{
			Id:          row.ID,
			EventType:   row.EventType,
			PayloadJson: row.Payload,
			CreatedAt:   timestamp(row.CreatedAt.Time),
		}
		response.Events = append(response.Events, &event)
		last, _ = parseInt(event.Id)
	}
	if len(response.Events) == int(limit) {
		response.NextCursor = fmt.Sprintf("%d", last)
	}
	return response, nil
}

func (s *Server) RetryWorkflow(ctx context.Context, request *controlv1.RetryWorkflowRequest) (*controlv1.RetryWorkflowResponse, error) {
	workflow, err := s.controlWorkflow(ctx, request.GetWorkflowRunId(), request.GetReason(), "retry")
	if err != nil {
		return nil, err
	}
	return &controlv1.RetryWorkflowResponse{Workflow: workflow}, nil
}

func (s *Server) CancelWorkflow(ctx context.Context, request *controlv1.CancelWorkflowRequest) (*controlv1.CancelWorkflowResponse, error) {
	workflow, err := s.controlWorkflow(ctx, request.GetWorkflowRunId(), request.GetReason(), "cancel")
	if err != nil {
		return nil, err
	}
	return &controlv1.CancelWorkflowResponse{Workflow: workflow}, nil
}

func (s *Server) BlockWorkflow(ctx context.Context, request *controlv1.BlockWorkflowRequest) (*controlv1.BlockWorkflowResponse, error) {
	workflow, err := s.controlWorkflow(ctx, request.GetWorkflowRunId(), request.GetReason(), "block")
	if err != nil {
		return nil, err
	}
	return &controlv1.BlockWorkflowResponse{Workflow: workflow}, nil
}

func (s *Server) SyncNow(ctx context.Context, request *controlv1.SyncNowRequest) (*controlv1.SyncNowResponse, error) {
	if _, err := s.requireMutation(ctx); err != nil {
		return nil, err
	}
	projectID := strings.TrimSpace(request.GetProjectId())
	if projectID != "" && !validID(projectID) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	var candidates []db.ListSyncableProjectsRow
	if projectID != "" {
		byID, err := s.queries.ListSyncableProjectByID(ctx, projectID)
		if err != nil {
			return nil, databaseError(err)
		}
		for _, row := range byID {
			candidates = append(candidates, db.ListSyncableProjectsRow(row))
		}
	} else {
		all, err := s.queries.ListSyncableProjects(ctx)
		if err != nil {
			return nil, databaseError(err)
		}
		candidates = all
	}
	response := &controlv1.SyncNowResponse{}
	for _, candidate := range candidates {
		result := &controlv1.ProjectSyncResult{ProjectId: candidate.ID}
		if err := s.syncProject(ctx, candidate.ID, textValue(candidate.RepositoryUrl)); err != nil {
			result.Error = err.Error()
		} else {
			count, err := s.queries.CountProjectIssues(ctx, candidate.ID)
			if err != nil {
				return nil, databaseError(err)
			}
			result.SyncedIssues = int32(count)
		}
		response.Results = append(response.Results, result)
	}
	if projectID != "" && len(response.Results) == 0 {
		return nil, status.Error(codes.NotFound, "enabled project is unknown")
	}
	return response, nil
}

// SyncProjects refreshes the issue snapshot for every enabled project. It is
// the unattended half of SyncNow: the console's "Sync now" button covers an
// operator who is watching, and this covers the deployments that nobody is.
//
// One project's failure does not abandon the rest — a repository with a revoked
// token would otherwise stop every other project discovering work — so failures
// are recorded per project (which is what drives the console's sync health) and
// reported together.
func (s *Server) SyncProjects(ctx context.Context) error {
	projects, err := s.queries.ListSyncableProjects(ctx)
	if err != nil {
		return databaseError(err)
	}
	var failures []error
	for _, candidate := range projects {
		if err := s.syncProject(ctx, candidate.ID, textValue(candidate.RepositoryUrl)); err != nil {
			failures = append(failures, fmt.Errorf("project %s: %w", candidate.ID, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Server) IssueSyncStatus(ctx context.Context, _ *controlv1.IssueSyncStatusRequest) (*controlv1.IssueSyncStatusResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	rows, err := s.queries.IssueSyncStatusEntries(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	response := &controlv1.IssueSyncStatusResponse{}
	for _, row := range rows {
		entry := &controlv1.IssueSyncStatusEntry{
			ProjectId:     row.ID,
			ProjectName:   row.Name,
			Enabled:       row.Enabled,
			IssueCount:    int32(row.IssueCount),
			EligibleCount: int32(row.EligibleCount),
		}
		if row.LastSyncedAt.Valid {
			entry.LastSyncedAt = timestamp(row.LastSyncedAt.Time)
		}
		if row.ConsecutiveFailures.Valid {
			entry.ConsecutiveFailures = row.ConsecutiveFailures.Int32
		}
		if row.NextRetryAt.Valid {
			entry.NextRetryAt = timestamp(row.NextRetryAt.Time)
		}
		if row.LastError.Valid {
			entry.LastError = row.LastError.String
		}
		response.Entries = append(response.Entries, entry)
	}
	return response, nil
}

func (s *Server) syncProject(ctx context.Context, projectID, repositoryURL string) error {
	repository, err := repositoryRef(repositoryURL)
	if err != nil {
		return err
	}
	issues, err := s.github.ListIssues(ctx, projectID, repository)
	if err != nil {
		_ = s.recordSyncFailure(ctx, projectID, err)
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	// Eligibility is label-driven only until an issue has been worked on. From
	// its first workflow run onwards the orchestrator owns the flag — a run that
	// ends without delivering parks the issue, and only a manual retry reopens
	// it — so a sync pass must not hand eligibility back to the label and
	// restart work the operator has not asked for again.
	for _, issue := range issues {
		labels, _ := json.Marshal(issue.Labels)
		raw, _ := json.Marshal(issue)
		if err := queries.UpsertIssue(ctx, db.UpsertIssueParams{
			ID: newID(), ProjectID: projectID, ExternalID: issue.ExternalID, Title: issue.Title, Body: issue.Body, Url: issue.URL,
			Column7: labels, Priority: int32(issue.Priority), Eligible: issue.Eligible,
			ExternalCreatedAt: pgtype.Timestamptz{Time: issue.CreatedAt, Valid: true},
			ExternalUpdatedAt: pgtype.Timestamptz{Time: issue.UpdatedAt, Valid: true},
			Column12:          raw,
		}); err != nil {
			return databaseError(err)
		}
	}
	if err := queries.UpsertIssueSyncStateSuccess(ctx, projectID); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) recordSyncFailure(ctx context.Context, projectID string, cause error) error {
	err := s.queries.UpsertIssueSyncStateFailure(ctx, db.UpsertIssueSyncStateFailureParams{ProjectID: projectID, LastError: pgtype.Text{String: truncate(cause.Error(), 1024), Valid: true}})
	return databaseError(err)
}

func (s *Server) ListRunners(ctx context.Context, _ *controlv1.ListRunnersRequest) (*controlv1.ListRunnersResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	rows, err := s.queries.ListRunners(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	response := &controlv1.ListRunnersResponse{}
	for _, row := range rows {
		runner, err := scanRunnerRowValues(row.ID, row.Name, row.Enabled, row.Draining, row.Status, row.Labels, row.LastSeenAt, row.Version)
		if err != nil {
			return nil, err
		}
		response.Runners = append(response.Runners, runner)
	}
	return response, nil
}

func (s *Server) SetRunnerState(ctx context.Context, request *controlv1.SetRunnerStateRequest) (*controlv1.SetRunnerStateResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(request.GetRunnerId()) {
		return nil, status.Error(codes.InvalidArgument, "runner ID is invalid")
	}
	var affected int64
	switch request.GetState() {
	case "drain":
		affected, err = s.queries.DrainRunner(ctx, request.GetRunnerId())
	case "enable":
		affected, err = s.queries.EnableRunner(ctx, request.GetRunnerId())
	case "revoke":
		affected, err = s.queries.RevokeRunner(ctx, request.GetRunnerId())
	default:
		return nil, status.Error(codes.InvalidArgument, "runner state is invalid")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	if affected != 1 {
		return nil, status.Error(codes.NotFound, "runner is unknown")
	}
	if request.GetState() == "revoke" {
		if err := s.queries.RevokeRunnerCredentials(ctx, request.GetRunnerId()); err != nil {
			return nil, databaseError(err)
		}
	}
	if err := audit(ctx, s.queries, actor.id, "runner."+request.GetState(), "runner", request.GetRunnerId()); err != nil {
		return nil, err
	}
	if request.GetState() == "drain" || request.GetState() == "revoke" {
		// Best-effort, same as workflow cancellation above: a runner with no
		// active session is already not receiving new offers, which is the
		// only thing draining/revoking otherwise guarantees, so a missed
		// delivery here is not a correctness problem — just a slower stop.
		s.enqueue(request.GetRunnerId(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Drain{Drain: &runnerv1.DrainRunner{}}})
	}
	runner, err := s.runner(ctx, request.GetRunnerId())
	if err != nil {
		return nil, err
	}
	return &controlv1.SetRunnerStateResponse{Runner: runner}, nil
}

func (s *Server) ListQueue(ctx context.Context, request *controlv1.ListQueueRequest) (*controlv1.ListQueueResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	limit := request.GetLimit()
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > 100 {
		return nil, status.Error(codes.InvalidArgument, "queue limit must be between 1 and 100")
	}
	rows, err := s.queries.ListQueueEntries(ctx, limit)
	if err != nil {
		return nil, databaseError(err)
	}
	response := &controlv1.ListQueueResponse{}
	for _, row := range rows {
		response.Entries = append(response.Entries, &controlv1.QueueEntry{
			ProjectId:     row.ProjectID,
			ProjectName:   row.ProjectName,
			ExternalId:    row.ExternalID,
			Title:         row.Title,
			Priority:      row.Priority,
			BlockedReason: row.BlockedReason,
		})
	}
	return response, nil
}

// schedulerSnapshot is one reading of the scheduling state the orchestrator
// owns. The heartbeat age is a pointer because it is NULL when no runner is
// enabled: there is no fleet-wide age to report, which is a different fact from
// an age of zero.
type schedulerSnapshot struct {
	queueDepth         int64
	activeWorkflows    int64
	scheduledJobs      int64
	enabledRunners     int64
	oldestHeartbeatAge *float64
}

// readSchedulerSnapshot is the one query behind both the console's
// GetSchedulerMetrics RPC and the Prometheus surface, so the two cannot report
// different numbers for the same word. It is a single round trip: five
// correlated subqueries, each an aggregate over a table the scheduler already
// indexes.
//
// The heartbeat age is MIN over *enabled, unrevoked* runners — the oldest, not
// the newest. A fleet where one runner is healthy and nine are gone is a
// broken fleet, and a MAX (or an average) would hide that behind the one that
// still reports. Disabled and revoked runners are excluded because an operator
// took them out of service deliberately; leaving them in would make the series
// permanently and correctly alarming, which is the same as useless. The age is
// computed by the database from its own clock, so orchestrator clock skew
// cannot make a heartbeat look fresher than it is.
func (s *Server) readSchedulerSnapshot(ctx context.Context) (schedulerSnapshot, error) {
	row, err := s.queries.GetSchedulerSnapshot(ctx)
	if err != nil {
		return schedulerSnapshot{}, databaseError(err)
	}
	snapshot := schedulerSnapshot{
		queueDepth:      row.QueueDepth,
		activeWorkflows: row.ActiveWorkflows,
		scheduledJobs:   row.ScheduledJobs,
		enabledRunners:  row.EnabledRunners,
	}
	if row.OldestHeartbeat.Valid && row.DbNow.Valid {
		seconds := row.DbNow.Time.Sub(row.OldestHeartbeat.Time).Seconds()
		snapshot.oldestHeartbeatAge = &seconds
	}
	return snapshot, nil
}

func (s *Server) GetSchedulerMetrics(ctx context.Context, _ *controlv1.GetSchedulerMetricsRequest) (*controlv1.GetSchedulerMetricsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	snapshot, err := s.readSchedulerSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	return &controlv1.GetSchedulerMetricsResponse{
		QueueDepth:      int32(snapshot.queueDepth),
		ActiveWorkflows: int32(snapshot.activeWorkflows),
		ScheduledJobs:   int32(snapshot.scheduledJobs),
	}, nil
}

// MetricsSnapshot reads the orchestrator-owned state the Prometheus surface
// exports. It runs on the scrape's goroutine, against the same pooled
// connections every other query uses — one round trip, no connection of its
// own — and returns an error rather than a zeroed snapshot when the database
// cannot answer, so the exporter can omit the series instead of publishing a
// zero nothing measured.
func (s *Server) MetricsSnapshot(ctx context.Context) (metrics.Snapshot, error) {
	snapshot, err := s.readSchedulerSnapshot(ctx)
	if err != nil {
		return metrics.Snapshot{}, err
	}
	reading := metrics.Snapshot{
		QueueDepth:      snapshot.queueDepth,
		ActiveWorkflows: snapshot.activeWorkflows,
		ScheduledJobs:   snapshot.scheduledJobs,
		EnabledRunners:  snapshot.enabledRunners,
	}
	if snapshot.oldestHeartbeatAge != nil {
		seconds := *snapshot.oldestHeartbeatAge
		// A last_seen_at in the future is only reachable through a clock that
		// moved; reporting a negative age would read as "seen in the future",
		// so it is clamped the same way the runner clamps its own.
		if seconds < 0 {
			seconds = 0
		}
		reading.OldestHeartbeatAge = time.Duration(seconds * float64(time.Second))
		reading.HeartbeatKnown = true
	}
	return reading, nil
}

// GetSystemVersion is deliberately unauthenticated, as it was before the Go
// rewrite. The API gateway calls it from its own public /api/v1/health handler,
// which has no session to present and silently reports an empty version if the
// call fails; requiring an actor here therefore does not protect anything, it
// just blanks the version operators use to tell deployments apart. The response
// carries only the build identifier, already published next to it as apiVersion.
func (s *Server) GetSystemVersion(_ context.Context, _ *controlv1.GetSystemVersionRequest) (*controlv1.GetSystemVersionResponse, error) {
	return &controlv1.GetSystemVersionResponse{Version: s.version}, nil
}

func (s *Server) RegisterRunner(ctx context.Context, request *runnerv1.RegisterRunnerRequest) (*runnerv1.RegisterRunnerResponse, error) {
	labels, err := normalizeLabels(request.GetLabels())
	if err != nil || strings.TrimSpace(request.GetName()) == "" || request.GetProtocolVersion() != "1.0" {
		return nil, status.Error(codes.InvalidArgument, "runner registration request is invalid")
	}
	capacity := request.GetCapacity()
	if capacity < 1 {
		capacity = 1
	}
	if capacity > 1024 || request.GetToken() == "" {
		return nil, status.Error(codes.InvalidArgument, "runner registration request is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	tokenID, err := queries.SelectValidRegistrationToken(ctx, db.SelectValidRegistrationTokenParams{TokenHash: hashSecret(request.GetToken()), Column2: []byte(jsonLabels(labels))})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.PermissionDenied, "runner registration was rejected")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	runnerID, credential := newID(), randomSecret()
	if err := queries.CreateRunner(ctx, db.CreateRunnerParams{ID: runnerID, Name: strings.TrimSpace(request.GetName()), Column3: []byte(jsonLabels(labels)), Capacity: capacity}); err != nil {
		return nil, databaseError(err)
	}
	if err := queries.CreateRunnerCredential(ctx, db.CreateRunnerCredentialParams{ID: newID(), RunnerID: runnerID, CredentialHash: hashSecret(credential)}); err != nil {
		return nil, databaseError(err)
	}
	if err := queries.MarkRegistrationTokenUsed(ctx, tokenID); err != nil {
		return nil, databaseError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	return &runnerv1.RegisterRunnerResponse{RunnerId: runnerID, Credential: credential}, nil
}

func (s *Server) Connect(stream grpc.BidiStreamingServer[runnerv1.RunnerToOrchestrator, runnerv1.OrchestratorToRunner]) error {
	first, err := stream.Recv()
	if errors.Is(err, io.EOF) {
		return nil
	}
	if err != nil {
		return err
	}
	if first.GetRunnerId() == "" || first.GetCredential() == "" {
		return status.Error(codes.Unauthenticated, "runner authentication was rejected")
	}
	if err := s.authenticateRunner(stream.Context(), first.GetRunnerId(), first.GetCredential()); err != nil {
		return err
	}
	outbound := make(chan *runnerv1.OrchestratorToRunner, 16)
	if !s.addSession(first.GetRunnerId(), outbound) {
		return status.Error(codes.AlreadyExists, "runner already has a control stream")
	}
	defer s.removeSession(first.GetRunnerId(), outbound)
	received := make(chan error, 1)
	go func() {
		if err := s.handleRunnerMessage(stream.Context(), first.GetRunnerId(), first); err != nil {
			received <- err
			return
		}
		for {
			message, err := stream.Recv()
			if err != nil {
				received <- err
				return
			}
			if message.GetRunnerId() != first.GetRunnerId() || message.GetCredential() == "" {
				received <- status.Error(codes.Unauthenticated, "runner authentication was rejected")
				return
			}
			if err := s.authenticateRunner(stream.Context(), message.GetRunnerId(), message.GetCredential()); err != nil {
				received <- err
				return
			}
			if err := s.handleRunnerMessage(stream.Context(), first.GetRunnerId(), message); err != nil {
				received <- err
				return
			}
		}
	}()
	for {
		select {
		case message := <-outbound:
			if err := stream.Send(message); err != nil {
				return err
			}
		case err := <-received:
			if errors.Is(err, io.EOF) {
				return nil
			}
			return err
		case <-stream.Context().Done():
			return stream.Context().Err()
		case <-s.shutdown:
			// Returning here (rather than blocking until the runner
			// disconnects on its own) is what lets GracefulStop finish
			// promptly. Unavailable is one of the codes the runner's
			// supervisor treats as transient, so it reconnects immediately
			// instead of waiting out its normal backoff.
			return status.Error(codes.Unavailable, "server is shutting down")
		}
	}
}

func (s *Server) handleRunnerMessage(ctx context.Context, runnerID string, message *runnerv1.RunnerToOrchestrator) error {
	switch {
	case message.GetHeartbeat() != nil:
		heartbeat := message.GetHeartbeat()
		if _, err := normalizeLabels(heartbeat.GetLabels()); err != nil {
			return status.Error(codes.InvalidArgument, "runner heartbeat labels are invalid")
		}
		err := s.queries.RecordRunnerHeartbeat(ctx, db.RecordRunnerHeartbeatParams{ID: runnerID, Column2: truncate(heartbeat.GetVersion(), 12)})
		return databaseError(err)
	case message.GetOfferAccepted() != nil:
		generation, expiresAt, err := s.acceptOffer(ctx, runnerID, message.GetOfferAccepted().GetJobId())
		if err != nil {
			return err
		}
		if !s.enqueue(runnerID, &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: message.GetOfferAccepted().GetJobId(), LeaseGeneration: generation, ExpiresAtUnixMs: expiresAt.UnixMilli()}}}) {
			return status.Error(codes.Unavailable, "runner control stream is unavailable")
		}
		return nil
	case message.GetOfferRejected() != nil:
		return s.rejectOffer(ctx, runnerID, message.GetOfferRejected().GetJobId(), message.GetOfferRejected().GetReason())
	case message.GetLeaseRenewal() != nil:
		renewal := message.GetLeaseRenewal()
		expiresAt, err := s.renewLease(ctx, runnerID, renewal)
		if err != nil {
			return err
		}
		if !s.enqueue(runnerID, &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: renewal.GetJobId(), LeaseGeneration: renewal.GetLeaseGeneration(), ExpiresAtUnixMs: expiresAt.UnixMilli()}}}) {
			return status.Error(codes.Unavailable, "runner control stream is unavailable")
		}
		return nil
	case message.GetEvent() != nil:
		return s.persistExecutionEvent(ctx, runnerID, message.GetEvent())
	case message.GetRunnerDraining() != nil:
		err := s.queries.SetRunnerDraining(ctx, db.SetRunnerDrainingParams{ID: runnerID, Draining: message.GetRunnerDraining().GetDraining()})
		return databaseError(err)
	default:
		return status.Error(codes.InvalidArgument, "runner message is empty")
	}
}

func (s *Server) addSession(runnerID string, outbound chan *runnerv1.OrchestratorToRunner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[runnerID]; exists {
		return false
	}
	s.sessions[runnerID] = outbound
	return true
}

func (s *Server) removeSession(runnerID string, outbound chan *runnerv1.OrchestratorToRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[runnerID] == outbound {
		delete(s.sessions, runnerID)
	}
}

func (s *Server) enqueue(runnerID string, message *runnerv1.OrchestratorToRunner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	outbound := s.sessions[runnerID]
	if outbound == nil {
		return false
	}
	select {
	case outbound <- message:
		return true
	default:
		return false
	}
}

func (s *Server) connectedRunners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	runners := make([]string, 0, len(s.sessions))
	for runnerID := range s.sessions {
		runners = append(runners, runnerID)
	}
	return runners
}

func (s *Server) ScheduleOnce(ctx context.Context) (bool, error) {
	runners := s.connectedRunners()
	if len(runners) == 0 {
		return false, nil
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return false, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	claim, err := queries.ClaimSchedulableIssue(ctx, runners)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseError(err)
	}
	issueID, externalID, title, body := claim.IssueID, claim.ExternalID, claim.Title, claim.Body
	projectID, mode, defaultBranch, runnerID := claim.ProjectID, claim.RepositoryMode, claim.DefaultBranch, claim.RunnerID
	repositoryURL, localPath := claim.RepositoryUrl, claim.LocalRepositoryPath
	configuration := []byte(claim.Configuration)
	workflowID, jobID, offerID := newID(), newID(), newID()
	branch := "agent/" + workflowID
	if err := queries.CreateWorkflowRun(ctx, db.CreateWorkflowRunParams{ID: workflowID, ProjectID: projectID, IssueID: issueID, ThreadID: workflowID, BranchName: pgtype.Text{String: branch, Valid: true}}); err != nil {
		return false, databaseError(err)
	}
	lockRows, err := queries.CreateProjectLock(ctx, db.CreateProjectLockParams{ProjectID: projectID, WorkflowRunID: workflowID})
	if err != nil {
		return false, databaseError(err)
	}
	if lockRows != 1 {
		return false, nil
	}
	if err := queries.CreateJob(ctx, db.CreateJobParams{ID: jobID, WorkflowRunID: workflowID, ProjectID: projectID, RunnerID: runnerID}); err != nil {
		return false, databaseError(err)
	}
	if err := queries.CreateJobOffer(ctx, db.CreateJobOfferParams{ID: offerID, JobID: jobID, RunnerID: runnerID}); err != nil {
		return false, databaseError(err)
	}
	var config projectConfig
	if err := json.Unmarshal(configuration, &config); err != nil {
		return false, configurationError(err)
	}
	packet, err := developerPacket(jobID, projectID, externalID, title, body, mode, textValue(repositoryURL), textValue(localPath), defaultBranch, branch, config.ExecutionImage)
	if err != nil {
		return false, err
	}
	encoded, err := json.Marshal(packet)
	if err != nil {
		return false, databaseError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return false, databaseError(err)
	}
	message := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: &runnerv1.JobOffer{JobId: jobID, LeaseGeneration: 1, TaskPacketJson: string(encoded)}}}
	if s.enqueue(runnerID, message) {
		return true, nil
	}
	// One transaction, on a context of its own. These three statements undo a
	// job the runner never received, and the lock release is the one that
	// matters: run as separate statements on the request context, a failure in
	// either of the first two — or a shutdown cancelling the context, which is
	// itself a plausible reason the enqueue failed — returned early and left the
	// project locked by a workflow that no longer exists.
	return false, s.releaseUndeliveredOffer(jobID, workflowID, projectID)
}

func (s *Server) releaseUndeliveredOffer(jobID, workflowID, projectID string) error {
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	const reason = "runner disconnected before offer delivery"
	if err := queries.CancelOfferedJob(ctx, db.CancelOfferedJobParams{ID: jobID, RecoveryReason: pgtype.Text{String: reason, Valid: true}}); err != nil {
		return databaseError(err)
	}
	if err := queries.CancelJobOfferByJob(ctx, jobID); err != nil {
		return databaseError(err)
	}
	if err := queries.CancelWorkflowRunUndelivered(ctx, db.CancelWorkflowRunUndeliveredParams{ID: workflowID, TerminalReason: pgtype.Text{String: reason, Valid: true}}); err != nil {
		return databaseError(err)
	}
	// The issue stays eligible on purpose: no execution ran, so nothing was
	// spent and the next scheduling pass should offer this work again.
	if err := queries.DeleteProjectLock(ctx, db.DeleteProjectLockParams{ProjectID: projectID, WorkflowRunID: workflowID}); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

// developerPacket builds the single execution a V1 workflow dispatches.
//
// The role has to be `developer`. The runner refuses to modify or push for a
// planner, pipeline or reviewer packet, and only pushes at all for a role
// granted mayPush — so a planner packet leaves the agent branch unpublished and
// the delivery step that follows opens a pull request from a branch the remote
// has never heard of. V1 has no separate planning phase, so the one execution
// it does dispatch is the one that writes the code and publishes the branch.
//
// mayMerge stays false in every packet: merging is the orchestrator's decision,
// taken after GitHub reports the checks green, and the runner rejects a packet
// that claims otherwise.
// implementExecutionID derives the execution ID the runner will report back
// against a job's single "implement" execution. Deterministic from the job
// ID rather than stored, so cancelling a job can address the runner's active
// execution without another round trip through the database.
func implementExecutionID(jobID string) string {
	return jobID + "-implement"
}

func developerPacket(jobID, projectID, externalID, title, body, mode, repositoryURL, localPath, defaultBranch, branch, executionImage string) (map[string]any, error) {
	if !validID(jobID) || !validID(projectID) || externalID == "" || title == "" || defaultBranch == "" || branch == "" || (mode == "managed_clone" && repositoryURL == "") || (mode == "existing_path" && localPath == "") || (mode != "managed_clone" && mode != "existing_path") {
		return nil, status.Error(codes.FailedPrecondition, "scheduled task is invalid")
	}
	return map[string]any{
		"protocolVersion": "1.0", "jobId": jobID, "executionId": implementExecutionID(jobID), "role": "developer", "objective": "Implement " + externalID + ": " + title,
		"issue":      map[string]string{"externalId": externalID, "title": title, "body": body},
		"repository": map[string]string{"projectId": projectID, "mode": mode, "url": repositoryURL, "localPath": localPath, "defaultBranch": defaultBranch, "branch": branch},
		"promptPath": ".loop/prompt.md", "expectedOutput": ".loop/result.json", "timeoutSeconds": 0, "environmentRefs": []any{}, "executionImage": executionImage,
		"constraints": map[string]bool{"mayModifyFiles": true, "mayPush": true, "mayMerge": false}, "pipeline": []any{}, "acceptanceCriteria": []string{}, "plan": []string{}, "previousFailures": []string{}, "currentCommit": "", "diffSummary": "", "failedChecks": []string{}, "reviewFindings": []string{},
	}, nil
}

func (s *Server) acceptOffer(ctx context.Context, runnerID, jobID string) (int64, time.Time, error) {
	if !validID(jobID) {
		return 0, time.Time{}, status.Error(codes.InvalidArgument, "job ID is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	_, err = queries.AcceptJobOffer(ctx, db.AcceptJobOfferParams{JobID: jobID, RunnerID: runnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, status.Error(codes.FailedPrecondition, "job offer is no longer active")
	}
	if err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	// `AND status='offered'` is load-bearing, not redundant with the offer row
	// above. Cancelling or blocking a workflow cancels the job but leaves the
	// offer row alone, so without this guard a late OfferAccepted drags an
	// administratively cancelled job back to 'preparing' and hands the runner a
	// fresh lease — which is the credential the runner presents to
	// ResolveJobSecret to read the project's tokens.
	accepted, err := queries.AcceptJob(ctx, db.AcceptJobParams{
		LeaseDuration: pgtype.Interval{Microseconds: leaseDuration.Microseconds(), Valid: true},
		ID:            jobID,
		RunnerID:      runnerID,
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, status.Error(codes.FailedPrecondition, "job offer is no longer active")
	}
	if err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	generation, expiresAt := accepted.LeaseGeneration, accepted.LeaseExpiresAt.Time
	if err := queries.SetWorkflowPreparing(ctx, jobID); err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	return generation, expiresAt, nil
}

func (s *Server) rejectOffer(ctx context.Context, runnerID, jobID, reason string) error {
	if !validID(jobID) {
		return status.Error(codes.InvalidArgument, "job ID is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	job, err := queries.GetJobForOfferReject(ctx, db.GetJobForOfferRejectParams{ID: jobID, RunnerID: runnerID})
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.FailedPrecondition, "job offer is no longer active")
	}
	if err != nil {
		return databaseError(err)
	}
	workflowID, projectID := job.WorkflowRunID, job.ProjectID
	if err := queries.CancelJobOfferByRunner(ctx, db.CancelJobOfferByRunnerParams{JobID: jobID, RunnerID: runnerID}); err != nil {
		return databaseError(err)
	}
	if err := queries.CancelJob(ctx, db.CancelJobParams{ID: jobID, Column2: truncate(reason, 1024)}); err != nil {
		return databaseError(err)
	}
	if err := queries.CancelWorkflowRunOfferRejected(ctx, workflowID); err != nil {
		return databaseError(err)
	}
	if err := queries.DeleteProjectLock(ctx, db.DeleteProjectLockParams{ProjectID: projectID, WorkflowRunID: workflowID}); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) renewLease(ctx context.Context, runnerID string, renewal *runnerv1.LeaseRenewal) (time.Time, error) {
	if !validID(renewal.GetJobId()) || renewal.GetLeaseGeneration() < 1 || renewal.GetRequestedExpiresAtUnixMs() <= time.Now().UnixMilli() {
		return time.Time{}, status.Error(codes.InvalidArgument, "lease renewal is invalid")
	}
	expiresAt := time.UnixMilli(renewal.GetRequestedExpiresAtUnixMs()).UTC()
	persisted, err := s.queries.RenewJobLease(ctx, db.RenewJobLeaseParams{
		ID:              renewal.GetJobId(),
		RunnerID:        runnerID,
		LeaseGeneration: renewal.GetLeaseGeneration(),
		LeaseExpiresAt:  pgtype.Timestamptz{Time: expiresAt, Valid: true},
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, status.Error(codes.FailedPrecondition, "runner does not hold this job")
	}
	if err != nil {
		return time.Time{}, databaseError(err)
	}
	return persisted.Time, nil
}

func (s *Server) persistExecutionEvent(ctx context.Context, runnerID string, event *runnerv1.ExecutionEvent) error {
	if !validID(event.GetJobId()) || event.GetLeaseGeneration() < 1 || event.GetEventSequence() < 1 || !validEventType(event.GetType()) || !json.Valid([]byte(event.GetPayloadJson())) {
		return status.Error(codes.InvalidArgument, "execution event is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	workflowID, err := queries.RecordJobExecutionEvent(ctx, db.RecordJobExecutionEventParams{
		ID: event.GetJobId(), RunnerID: runnerID, LeaseGeneration: event.GetLeaseGeneration(),
		EventSequence: event.GetEventSequence(), EventType: event.GetType(),
	})
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.FailedPrecondition, "execution event is stale")
	}
	if err != nil {
		return databaseError(err)
	}
	// The event type is stored unprefixed because it is a vocabulary shared with
	// the console, which switches on bare "log", "started", "failed" and friends
	// to build the timeline and the agent log pane. Writing "runner.log" here
	// left every one of those rows falling through to the default branch.
	if err := queries.CreateWorkflowEvent(ctx, db.CreateWorkflowEventParams{
		WorkflowRunID: workflowID, EventType: event.GetType(), Severity: eventSeverity(event.GetType()), Column4: []byte(event.GetPayloadJson()),
	}); err != nil {
		return databaseError(err)
	}
	if terminalEvent(event.GetType()) {
		// The event type is the shared vocabulary and is stored as it arrived;
		// the run's own terminal status is derived from it. An agent that
		// stopped deliberately and said why reports a `failed` event whose
		// payload is marked `blocked: true` (runner/README.md, "An
		// agent-reported block is not a crash"), and ending that run as
		// `failed` with an empty blocking_reason made a stated, actionable stop
		// indistinguishable from an anonymous crash -- and kept it out of the
		// console's needs-attention triage, which is waiting_human ∪ blocked.
		//
		// blocking_reason and terminal_reason are left untouched when no reason
		// was derived, which is every path but this one, so an ordinary failure
		// writes exactly the columns it wrote before. No competing value should
		// exist to preserve — the other two writers of those columns terminate
		// the run and fence its job, and this statement is only reached by an
		// event that passed the fence — so the COALESCE is a backstop against
		// that reasoning changing, not a case anything is known to hit.
		// event.GetType() is one of "completed", "failed" or "cancelled" here
		// (terminalEvent already fenced anything else), which is the
		// runner-event vocabulary Status shares for "failed" and "cancelled" --
		// but not for "completed", which the run's own status column no longer
		// reuses. A "completed" event means the agent succeeded, not that
		// delivery (opening/merging the pull request) has, so the run moves to
		// StatusDelivering instead: see its doc comment in status.go for why
		// conflating the two under one 'completed' value was the bug #267
		// fixed. The event row above still records the runner's own
		// "completed", since that is the separate, shared vocabulary the
		// console's event timeline switches on.
		runStatus, blockingReason := Status(event.GetType()), ""
		switch event.GetType() {
		case "completed":
			runStatus = StatusDelivering
		case "failed":
			if reason, blocked := agentBlockReason(event.GetPayloadJson()); blocked {
				runStatus, blockingReason = StatusBlocked, reason
			}
		}
		if err := queries.SetWorkflowTerminalStatus(ctx, db.SetWorkflowTerminalStatusParams{
			ID: workflowID, Status: runStatus.String(), Column3: blockingReason,
		}); err != nil {
			return databaseError(err)
		}
		if event.GetType() != "completed" {
			if err := queries.DeleteProjectLockByWorkflow(ctx, workflowID); err != nil {
				return databaseError(err)
			}
			// An execution that actually ran and did not succeed parks its
			// issue. V1 has no automatic retry, so leaving the issue eligible
			// would have the scheduler dispatch the same work again on the very
			// next tick, forever. RetryWorkflow is what makes it eligible again.
			if err := queries.ParkIssue(ctx, workflowID); err != nil {
				return databaseError(err)
			}
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(err)
	}
	if event.GetType() == "completed" {
		return s.deliverWorkflow(ctx, workflowID)
	}
	return nil
}

// databaseError maps an error from a database call to the opaque, constant
// status the client sees. The real cause — PG error code, constraint name,
// failing column, pool exhaustion — is logged here so it isn't lost: this is
// the only place any of the ~140+ call sites need to change to get that
// visibility, rather than annotating every call site individually.
func databaseError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	slog.Error("database operation failed", "error", err)
	return status.Error(codes.Internal, "database operation failed")
}

// configurationError reports a stored-configuration value (project
// configuration, runner labels, registration-token labels) that failed to
// decode. Unlike databaseError, the cause here has nothing to do with the
// database call that fetched the row — it's malformed JSON in a column — so
// it must not be reported as "database operation failed", which would send
// an operator to investigate Postgres instead of the corrupted data. The
// real cause is logged server-side; the client sees a distinct, opaque
// message.
func configurationError(err error) error {
	if err == nil {
		return nil
	}
	slog.Error("stored configuration is invalid", "error", err)
	return status.Error(codes.Internal, "stored configuration is invalid")
}

func (s *Server) requireActor(ctx context.Context, mutation bool) (actor, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get(sessionHeader)) != 1 || strings.TrimSpace(md.Get(sessionHeader)[0]) == "" {
		return actor{}, status.Error(codes.Unauthenticated, "session is required")
	}
	row, err := s.queries.GetSessionActor(ctx, hashSecret(md.Get(sessionHeader)[0]))
	if errors.Is(err, pgx.ErrNoRows) {
		return actor{}, status.Error(codes.Unauthenticated, "session is invalid")
	}
	if err != nil {
		return actor{}, databaseError(err)
	}
	id, role, csrfHash := row.ID, row.Role, row.CsrfTokenHash
	if mutation {
		csrf := md.Get(csrfHeader)
		if len(csrf) != 1 || subtle.ConstantTimeCompare([]byte(hashSecret(csrf[0])), []byte(csrfHash)) != 1 {
			return actor{}, status.Error(codes.PermissionDenied, "CSRF token is invalid")
		}
		if role != "admin" {
			return actor{}, status.Error(codes.PermissionDenied, "administrator access is required")
		}
	}
	return actor{id: id, role: role}, nil
}

func (s *Server) requireMutation(ctx context.Context) (actor, error) {
	return s.requireActor(ctx, true)
}

func (s *Server) authenticateRunner(ctx context.Context, runnerID, credential string) error {
	if !validID(runnerID) {
		return status.Error(codes.Unauthenticated, "runner authentication was rejected")
	}
	stored, err := s.queries.GetRunnerCredentialHash(ctx, runnerID)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(hashSecret(credential))) != 1 {
		return status.Error(codes.Unauthenticated, "runner authentication was rejected")
	}
	return nil
}

// projectQuerier is the subset of *db.Queries the project() helper needs. It
// lets ListProjects/CreateProject/UpdateProject/SetProjectEnabled share this
// helper whether they hold the pool-backed *db.Queries or one scoped to an
// open transaction via WithTx.
type projectQuerier interface {
	GetProject(context.Context, string) (db.GetProjectRow, error)
	ListProjectPipelineSteps(context.Context, string) ([]db.ListProjectPipelineStepsRow, error)
}

func (s *Server) project(ctx context.Context, queries projectQuerier, id string) (*controlv1.Project, error) {
	if !validID(id) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	row, err := queries.GetProject(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	project := &controlv1.Project{
		Id: row.ID, Name: row.Name, Enabled: row.Enabled, RepositoryMode: row.RepositoryMode,
		DefaultBranch:       row.DefaultBranch,
		RepositoryUrl:       textValue(row.RepositoryUrl),
		LocalRepositoryPath: textValue(row.LocalRepositoryPath),
	}
	var config projectConfig
	if err := json.Unmarshal([]byte(row.Configuration), &config); err != nil {
		return nil, configurationError(err)
	}
	project.RequiredRunnerLabels, project.ExecutionImage = config.Labels, config.ExecutionImage
	steps, err := queries.ListProjectPipelineSteps(ctx, id)
	if err != nil {
		return nil, databaseError(err)
	}
	for _, step := range steps {
		project.PipelineSteps = append(project.PipelineSteps, &controlv1.PipelineStep{
			Command: step.Command, TimeoutSeconds: step.TimeoutSeconds, Position: step.Position, Required: step.Required,
		})
	}
	return project, nil
}

func (s *Server) workflow(ctx context.Context, id string) (*controlv1.Workflow, error) {
	if !validID(id) {
		return nil, status.Error(codes.InvalidArgument, "workflow run ID is invalid")
	}
	row, err := s.queries.GetWorkflowDetail(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "workflow run is unknown")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	return workflowFromDetailRow(row), nil
}

// workflowFromDetailRow maps the GetWorkflowDetail/ListWorkflowsPage join
// shape (identical columns, kept as separate sqlc queries since one is
// keyed by id and the other paginated) to the wire type. ListWorkflowsPageRow
// converts to GetWorkflowDetailRow with a plain type conversion: sqlc
// generates them as structurally identical structs.
func workflowFromDetailRow(row db.GetWorkflowDetailRow) *controlv1.Workflow {
	workflow := &controlv1.Workflow{
		Id: row.ID, ProjectId: row.ProjectID, Status: row.Status, Phase: row.CurrentPhase,
		IssueExternalId:        row.ExternalID,
		IssueTitle:             row.Title,
		BranchName:             textValue(row.BranchName),
		PullRequestExternalId:  coalesceText(row.PrExternalID, row.RunPullRequestExternalID),
		PullRequestUrl:         coalesceText(row.PrUrl, row.RunPullRequestUrl),
		PullRequestState:       textValue(row.PullRequestState),
		BlockingReason:         textValue(row.BlockingReason),
		PlanningAttempts:       row.PlanningAttempts,
		ImplementationAttempts: row.ImplementationAttempts,
		PipelineRepairAttempts: row.PipelineRepairAttempts,
		CiRepairAttempts:       row.CiRepairAttempts,
		ReviewCycles:           row.ReviewCycles,
		TotalAgentExecutions:   row.TotalAgentExecutions,
	}
	workflow.CreatedAt, workflow.UpdatedAt = timestamp(row.CreatedAt.Time), timestamp(row.UpdatedAt.Time)
	return workflow
}

func (s *Server) controlWorkflow(ctx context.Context, id, reason, action string) (*controlv1.Workflow, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !validID(id) || len(reason) > 1024 || (action == "block" && strings.TrimSpace(reason) == "") {
		return nil, status.Error(codes.InvalidArgument, "workflow control request is invalid")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	control, err := queries.GetWorkflowForControl(ctx, id)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "workflow run is unknown")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	projectID, current := control.ProjectID, control.Status
	// Populated only when cancelling/blocking finds a job still in flight,
	// which is the case the runner needs to hear about: nothing to notify if
	// the run never got past planning. lease_generation-1 is the generation
	// the runner's lease was actually acknowledged at — the UPDATE below bumps
	// it to fence a lease renewal or event racing the cancellation, so the
	// value the runner is told to cancel must be read from before that bump.
	var jobToCancel, runnerHoldingLease string
	var leaseGenerationAtCancel int64
	switch action {
	case "retry":
		// Retryable is exactly genuinelyTerminalStatuses, not terminalStatuses:
		// a completed run is done but not retryable through this action, and
		// (today) not retryable at all -- it has already delivered, or is
		// still trying to.
		if !genuinelyTerminalStatus(current) {
			return nil, status.Error(codes.FailedPrecondition, "workflow run is not retryable")
		}
		// Retry reopens the issue rather than reviving this run. A job is
		// unique per workflow run (app.jobs.workflow_run_id is UNIQUE), so the
		// run that already had its execution cannot be given another one; the
		// scheduler picks the reopened issue up and creates a fresh run whose
		// history stands alongside this one. The previous implementation took
		// the project lock and parked the run in a "recovering" status nothing
		// reads, which left the project unable to schedule anything again.
		if err := queries.DeleteProjectLock(ctx, db.DeleteProjectLockParams{ProjectID: projectID, WorkflowRunID: id}); err != nil {
			return nil, databaseError(err)
		}
		if err := queries.ReopenIssueForRetry(ctx, id); err != nil {
			return nil, databaseError(err)
		}
		if err := queries.CreateRetryEvent(ctx, id); err != nil {
			return nil, databaseError(err)
		}
	case "cancel", "block":
		next := StatusCancelled
		if action == "block" {
			next = StatusBlocked
		}
		if current != next.String() {
			if terminalStatus(current) {
				return nil, status.Error(codes.FailedPrecondition, "workflow run is already terminal")
			}
			if err := queries.SetWorkflowControlStatus(ctx, db.SetWorkflowControlStatusParams{
				ID: id, Status: next.String(), TerminalReason: pgtype.Text{String: defaultReason(action, reason), Valid: true},
			}); err != nil {
				return nil, databaseError(err)
			}
			cancelled, err := queries.CancelWorkflowJobs(ctx, db.CancelWorkflowJobsParams{
				WorkflowRunID: id, RecoveryReason: pgtype.Text{String: defaultReason(action, reason), Valid: true},
			})
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, databaseError(err)
			}
			if err == nil {
				jobToCancel, runnerHoldingLease, leaseGenerationAtCancel = cancelled.ID, cancelled.RunnerID, int64(cancelled.PreviousLeaseGeneration)
			}
			// Blocking parks the issue; cancelling does not. Blocking says "stop
			// working on this until a human says otherwise", and without parking
			// the scheduler re-created the run from the still-eligible issue on
			// the next one-second tick — the operator's stop button restarting
			// the very work it stopped. Cancelling returns the issue to the
			// queue by design, so it stays eligible.
			if action == "block" {
				if err := queries.ParkIssue(ctx, id); err != nil {
					return nil, databaseError(err)
				}
			}
			// Withdraw any offer still outstanding. Offers do not age out on
			// their own, so an offer left 'offered' is one a runner can still
			// answer after the operator has cancelled the work.
			if err := queries.CancelWorkflowJobOffers(ctx, id); err != nil {
				return nil, databaseError(err)
			}
			if err := queries.CancelWorkflowExecutionRequests(ctx, id); err != nil {
				return nil, databaseError(err)
			}
			if err := queries.DeleteProjectLock(ctx, db.DeleteProjectLockParams{ProjectID: projectID, WorkflowRunID: id}); err != nil {
				return nil, databaseError(err)
			}
		}
	}
	if err := audit(ctx, queries, actor.id, "workflow."+action, "workflow_run", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	// Best-effort: the runner may already be disconnected, in which case the
	// lease sweep is the backstop. Never fail the RPC over it — the database
	// state is already committed and correct regardless of delivery.
	if jobToCancel != "" && runnerHoldingLease != "" {
		s.enqueue(runnerHoldingLease, &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Cancel{Cancel: &runnerv1.CancelExecution{
			ExecutionId:     implementExecutionID(jobToCancel),
			LeaseGeneration: leaseGenerationAtCancel,
		}}})
	}
	return s.workflow(ctx, id)
}

func replacePipelineSteps(ctx context.Context, queries *db.Queries, projectID string, steps []*controlv1.PipelineStep) error {
	if err := queries.DeleteProjectPipelineSteps(ctx, projectID); err != nil {
		return databaseError(err)
	}
	for _, step := range steps {
		if err := queries.CreateProjectPipelineStep(ctx, db.CreateProjectPipelineStepParams{
			ID: newID(), ProjectID: projectID, Position: step.GetPosition(), Name: step.GetCommand(),
			TimeoutSeconds: step.GetTimeoutSeconds(), Required: step.GetRequired(),
		}); err != nil {
			return databaseError(err)
		}
	}
	return nil
}

func validateProject(cfg *controlv1.ProjectConfiguration) (*controlv1.ProjectConfiguration, []*controlv1.PipelineStep, error) {
	if cfg == nil || strings.TrimSpace(cfg.GetName()) == "" || len(cfg.GetName()) > 256 || (cfg.GetRepositoryMode() != "managed_clone" && cfg.GetRepositoryMode() != "existing_path") || strings.TrimSpace(cfg.GetDefaultBranch()) == "" || len(cfg.GetDefaultBranch()) > 256 {
		return nil, nil, status.Error(codes.InvalidArgument, "project configuration is invalid")
	}
	// existing_path has no end-to-end support: issue sync and workflow delivery
	// both talk to GitHub by repository URL, and a local_repository_path gives
	// them no GitHub coordinates to work with. Accepting the mode here would
	// let a project run an entire (expensive) workflow before failing at
	// delivery. Reject it up front instead, at configuration time.
	if cfg.GetRepositoryMode() == "existing_path" {
		return nil, nil, status.Error(codes.InvalidArgument, "repository_mode 'existing_path' is not supported: existing_path projects cannot sync issues or deliver workflows without a repository_url, since neither operation has any other way to learn the project's GitHub coordinates; use repository_mode 'managed_clone' with a repository_url instead")
	}
	if (cfg.GetRepositoryMode() == "managed_clone" && (cfg.GetRepositoryUrl() == "" || cfg.GetLocalRepositoryPath() != "")) || len(cfg.GetExecutionImage()) > 512 {
		return nil, nil, status.Error(codes.InvalidArgument, "project configuration is invalid")
	}
	labels, err := normalizeLabels(cfg.GetRequiredRunnerLabels())
	if err != nil {
		return nil, nil, status.Error(codes.InvalidArgument, "project labels are invalid")
	}
	seen := map[int32]bool{}
	for _, step := range cfg.GetPipelineSteps() {
		if step == nil || strings.TrimSpace(step.GetCommand()) == "" || step.GetTimeoutSeconds() < 1 || step.GetPosition() < 0 || seen[step.GetPosition()] {
			return nil, nil, status.Error(codes.InvalidArgument, "pipeline steps are invalid")
		}
		seen[step.GetPosition()] = true
	}
	return &controlv1.ProjectConfiguration{
		Name:                 strings.TrimSpace(cfg.GetName()),
		RepositoryMode:       cfg.GetRepositoryMode(),
		RepositoryUrl:        cfg.GetRepositoryUrl(),
		LocalRepositoryPath:  cfg.GetLocalRepositoryPath(),
		DefaultBranch:        cfg.GetDefaultBranch(),
		RequiredRunnerLabels: labels,
		PipelineSteps:        cfg.GetPipelineSteps(),
		ExecutionImage:       cfg.GetExecutionImage(),
	}, cfg.GetPipelineSteps(), nil
}

func normalizeLabels(labels []string) ([]string, error) {
	seen := map[string]bool{}
	result := make([]string, 0, len(labels))
	for _, label := range labels {
		label = strings.TrimSpace(label)
		if label == "" || len(label) > 128 || seen[label] {
			return nil, errors.New("invalid label")
		}
		seen[label] = true
		result = append(result, label)
	}
	return result, nil
}
func (s *Server) runner(ctx context.Context, id string) (*controlv1.Runner, error) {
	if !validID(id) {
		return nil, status.Error(codes.InvalidArgument, "runner ID is invalid")
	}
	row, err := s.queries.GetRunner(ctx, id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "runner is unknown")
		}
		return nil, databaseError(err)
	}
	return scanRunnerRowValues(row.ID, row.Name, row.Enabled, row.Draining, row.Status, row.Labels, row.LastSeenAt, row.Version)
}
func scanRunnerRowValues(id, name string, enabled, draining bool, status_ string, labels string, seen pgtype.Timestamptz, version string) (*controlv1.Runner, error) {
	runner := &controlv1.Runner{Id: id, Name: name, Enabled: enabled, Draining: draining, Status: status_, Version: version}
	if err := json.Unmarshal([]byte(labels), &runner.Labels); err != nil {
		return nil, configurationError(err)
	}
	if seen.Valid {
		runner.LastSeenAt = timestamp(seen.Time)
	}
	return runner, nil
}

// auditQuerier is the subset of *db.Queries the audit() helper needs, so
// call sites can pass either the pool-backed s.queries or one scoped to an
// open transaction via WithTx.
type auditQuerier interface {
	CreateAuditEvent(context.Context, db.CreateAuditEventParams) error
}

func audit(ctx context.Context, queries auditQuerier, actorID, action, targetType, targetID string) error {
	err := queries.CreateAuditEvent(ctx, db.CreateAuditEventParams{
		ActorID: pgtype.Text{String: actorID, Valid: true}, Action: action, TargetType: targetType, TargetID: targetID,
	})
	return databaseError(err)
}
func commit(tx pgx.Tx) error {
	if err := tx.Commit(context.Background()); err != nil {
		return databaseError(err)
	}
	return nil
}
func validID(value string) bool {
	if len(value) != 36 {
		return false
	}
	for i, c := range value {
		if i == 8 || i == 13 || i == 18 || i == 23 {
			if c != '-' {
				return false
			}
		} else if !strings.ContainsRune("0123456789abcdefABCDEF", c) {
			return false
		}
	}
	return true
}
func newID() string {
	bytes := make([]byte, 16)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	bytes[6] = bytes[6]&0x0f | 0x40
	bytes[8] = bytes[8]&0x3f | 0x80
	encoded := hex.EncodeToString(bytes)
	return encoded[:8] + "-" + encoded[8:12] + "-" + encoded[12:16] + "-" + encoded[16:20] + "-" + encoded[20:]
}
func randomSecret() string {
	bytes := make([]byte, 32)
	if _, err := rand.Read(bytes); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(bytes)
}

func optionalSecret(name string) (string, bool, error) {
	value, file := os.Getenv(name), os.Getenv(name+"_FILE")
	if value != "" && file != "" {
		return "", false, fmt.Errorf("%s and %s_FILE cannot both be set", name, name)
	}
	if file == "" {
		return value, value != "", nil
	}
	info, err := os.Stat(file)
	if err != nil {
		return "", false, fmt.Errorf("read %s_FILE: %w", name, err)
	}
	if !info.Mode().IsRegular() || info.Size() > 16*1024 {
		return "", false, fmt.Errorf("%s_FILE is invalid", name)
	}
	contents, err := os.ReadFile(file)
	if err != nil {
		return "", false, fmt.Errorf("read %s_FILE: %w", name, err)
	}
	value = strings.TrimSpace(string(contents))
	return value, value != "", nil
}

func validPassword(password string) bool {
	if len(password) < 8 || len(password) > 1024 {
		return false
	}
	var digit, upper, lower, symbol bool
	for _, character := range password {
		switch {
		case character >= '0' && character <= '9':
			digit = true
		case character >= 'A' && character <= 'Z':
			upper = true
		case character >= 'a' && character <= 'z':
			lower = true
		default:
			symbol = true
		}
	}
	return digit && upper && lower && symbol
}

func passwordHash(password string) (string, error) {
	if !validPassword(password) {
		return "", errors.New("password is invalid")
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	digest, err := scrypt.Key([]byte(password), salt, 1<<14, 8, 1, 32)
	if err != nil {
		return "", err
	}
	return strings.Join([]string{"scrypt", "16384", "8", "1", base64.RawURLEncoding.EncodeToString(salt), base64.RawURLEncoding.EncodeToString(digest)}, "$"), nil
}

// passwordMatches reports whether password matches the stored, encoded
// scrypt hash. It intentionally always returns (false, nil) — never an
// error — for a hash that doesn't parse as the expected format: to the
// caller this must be indistinguishable from a wrong password, since
// otherwise the error return would give a timing/behavior oracle for which
// accounts have a corrupted password_hash row. That indistinguishability is
// exactly what makes a corrupted row silently unrecoverable, so the
// unparseable cases are logged here — for an operator reading logs, not for
// the caller — before returning.
func passwordMatches(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "scrypt" || parts[1] != "16384" || parts[2] != "8" || parts[3] != "1" {
		slog.Error("password hash has an unrecognized format", "encoding", parts[0])
		return false, nil
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		slog.Error("password hash has an invalid salt encoding", "error", err)
		return false, nil
	}
	expected, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
		slog.Error("password hash has an invalid digest encoding", "error", err)
		return false, nil
	}
	actual, err := scrypt.Key([]byte(password), salt, 1<<14, 8, 1, len(expected))
	if err != nil {
		return false, err
	}
	return subtle.ConstantTimeCompare(actual, expected) == 1, nil
}

func hashSecret(value string) string {
	digest := sha256.Sum256([]byte(value))
	return hex.EncodeToString(digest[:])
}
func jsonLabels(labels []string) string { encoded, _ := json.Marshal(labels); return string(encoded) }

func terminalEvent(event string) bool {
	return event == "completed" || event == "failed" || event == "cancelled"
}
func validEventType(event string) bool {
	switch event {
	case "started", "log", "progress", "completed", "failed", "cancelled":
		return true
	}
	return false
}

// eventSeverity classifies a runner event for the console, which colours the
// timeline by severity. Everything used to be stored as "info", including the
// failure that ended the run.
func eventSeverity(event string) string {
	switch event {
	case "failed":
		return "error"
	case "cancelled":
		return "warning"
	}
	return "info"
}

// agentBlockPrefix opens an agent-composed blocking_reason. It is the phrasing
// the runner's goal gate already uses for the same fact, so an operator reading
// the console and an operator reading the runner log read the same sentence.
const agentBlockPrefix = "the agent reported itself blocked"

// maxBlockingReasonBytes is the width blocking_reason already carries: the
// operator block path rejects a longer reason outright and terminateWorkflow
// truncates to it. An agent-composed reason is held to the same bound. The
// runner bounds the summary and the remaining work it sends, but the
// orchestrator must not depend on a runner it does not control to have done so.
const maxBlockingReasonBytes = 1024

const (
	reasonTruncationMarker = "…"
	summaryLead            = ": "
	remainingWorkLead      = " (remaining work: "
	remainingWorkJoin      = "; "
	// A slot too small to say anything in is not worth the separator that
	// introduces it, so the list stops rather than trailing an ellipsis.
	minReasonEntryBytes = 16
)

// agentBlockReason reads a terminal `failed` payload for the agent's own
// declaration that it stopped deliberately, and composes the operator-facing
// reason from it. The second result is what the caller switches on: false means
// "this is an ordinary failure", and an empty reason with a true is impossible
// because the prefix is unconditional.
//
// The runner keeps the terminal event type `failed` -- that vocabulary is a
// contract shared with app.jobs.status and the console -- and marks the payload
// `blocked: true` only when the agent process exited cleanly and its own result
// document said `blocked`. A crashed process is never trusted to report a
// block, so a genuine crash never carries this flag and still ends as `failed`.
//
// The payload is agent-supplied, so nothing here may fail the event: losing a
// terminal event strands the run and its project lock. It is decoded field by
// field into json.RawMessage, and a payload that is not a JSON object, a
// missing or null `blocked`, or a wrongly typed `summary`/`remainingWork` all
// fall back to the ordinary `failed` outcome. `blocked` must be the JSON
// literal `true` specifically: a truthy string or a 1 is a malformed payload,
// not a declaration.
func agentBlockReason(payloadJSON string) (string, bool) {
	var payload map[string]json.RawMessage
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return "", false
	}
	var blocked bool
	if err := json.Unmarshal(payload["blocked"], &blocked); err != nil || !blocked {
		return "", false
	}
	// Both fields are best effort. encoding/json fills in what it can and
	// reports the first type mismatch; a summary that is not a string simply
	// stays empty, and the block is still recorded with its prefix.
	var summary string
	_ = json.Unmarshal(payload["summary"], &summary)
	var remaining []string
	_ = json.Unmarshal(payload["remainingWork"], &remaining)
	return composeBlockReason(summary, remaining), true
}

// composeBlockReason renders an agent's account in the shape blocking_reason
// already carries: one bounded line of plain English, written to
// blocking_reason and terminal_reason together, exactly as the operator block
// path and terminateWorkflow write it.
//
// The budget is shared rather than spent first-come. Bounding the summary on
// its own let a verbose agent fill the reason with prose and push its own list
// of remaining work out of it silently — and the list is the actionable half of
// the account, the part a human reads to decide what to do next. So when there
// is remaining work to report, the summary may take at most half of what the
// prefix leaves, and the list's room stops one byte short so its closing
// parenthesis always fits: a reason ending in an unclosed "(remaining work:"
// reads as malformed rather than as truncated.
func composeBlockReason(summary string, remaining []string) string {
	items := make([]string, 0, len(remaining))
	for _, entry := range remaining {
		if text := agentReasonText(entry); text != "" {
			items = append(items, text)
		}
	}
	reason := agentBlockPrefix
	if text := agentReasonText(summary); text != "" {
		share := maxBlockingReasonBytes - len(reason) - len(summaryLead)
		if len(items) > 0 {
			share /= 2
		}
		if text = boundedReason(text, share); text != "" {
			reason += summaryLead + text
		}
	}
	lead := remainingWorkLead
	for _, entry := range items {
		room := maxBlockingReasonBytes - len(reason) - len(lead) - len(")")
		if room < minReasonEntryBytes {
			break
		}
		reason += lead + boundedReason(entry, room)
		lead = remainingWorkJoin
	}
	if lead == remainingWorkJoin {
		reason += ")"
	}
	// The arithmetic above already holds the result to the bound; this is the
	// backstop, because what is on the other side of it is a text column that
	// rejects the write outright rather than storing something too long.
	return boundedReason(reason, maxBlockingReasonBytes)
}

// agentReasonText makes agent-written prose fit to store and to show. The text
// reaches here from the agent through the runner, so it is treated as hostile:
// PostgreSQL rejects a NUL byte in a text column outright, which would fail the
// terminal event and leave the run holding its project lock forever, and an
// escape sequence in an operator-facing field is a terminal-injection vector
// wherever the value is later printed. Control characters therefore become
// spaces, invalid UTF-8 is dropped, and the runs of whitespace that leaves are
// collapsed, because blocking_reason is rendered as a single line.
//
// Unicode format characters go the same way, and they are not covered by
// unicode.IsControl: a right-to-left override (U+202E) or a bidirectional
// isolate makes the console render a reason as text other than the one stored,
// which is the same class of lie as a terminal escape and reaches further,
// since it survives HTML escaping.
func agentReasonText(value string) string {
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
			return ' '
		}
		return r
	}, strings.ToValidUTF8(value, ""))
	return strings.Join(strings.Fields(cleaned), " ")
}

// boundedReason caps text at limit bytes without splitting a rune, marking what
// it cut. A byte slice through a multi-byte character would leave invalid
// UTF-8, which PostgreSQL rejects on a text column — the whole terminal event
// would fail, for no reason other than where a bound happened to land. A limit
// with no room for even one rune plus the marker yields nothing, so a caller
// never appends a separator introducing an empty fragment.
func boundedReason(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit - len(reasonTruncationMarker)
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	if end <= 0 {
		return ""
	}
	return value[:end] + reasonTruncationMarker
}

// parkIssue makes a workflow run's issue ineligible so the scheduler stops
// offering it. It is the mechanism behind "no automatic retries": without it a
// run that fails releases its project lock and is immediately re-created from
// the same still-eligible issue.
//
// It still takes a bare pgx.Tx (rather than the *db.Queries every other
// helper in this file now takes) because delivery.go -- out of scope for this
// conversion, see issue #306's own scope note -- calls it against a
// transaction it manages itself; db.New(tx).ParkIssue(...) scopes the
// generated query to that same transaction without requiring delivery.go to
// change.
func parkIssue(ctx context.Context, tx pgx.Tx, workflowID string) error {
	return databaseError(db.New(tx).ParkIssue(ctx, workflowID))
}
func defaultReason(action, reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "operator " + action
	}
	return strings.TrimSpace(reason)
}
func timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }
func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// textValue reads a nullable sqlc-generated pgtype.Text the same way
// stringValue reads a *string: empty when NULL.
func textValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

// coalesceText mirrors SQL's COALESCE over two nullable columns read
// separately (see GetWorkflowDetail's query comment for why they are no
// longer combined with COALESCE in SQL): the first valid value wins, and
// both absent yields "".
func coalesceText(preferred, fallback pgtype.Text) string {
	if preferred.Valid {
		return preferred.String
	}
	return textValue(fallback)
}
func truncate(value string, length int) string {
	if len(value) > length {
		return value[:length]
	}
	return value
}
func parseInt(value string) (int64, error) {
	var result int64
	_, err := fmt.Sscan(value, &result)
	return result, err
}
