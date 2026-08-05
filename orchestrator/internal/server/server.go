package server

import (
	"context"
	"crypto/subtle"
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

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	runnerv1 "github.com/alexandre-leites/moirai/contracts/gen/runner/v1"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/loop-engineering/orchestrator/internal/db"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/metrics"
	"github.com/loop-engineering/orchestrator/internal/secrethash"
	"github.com/loop-engineering/orchestrator/internal/textutil"
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

// Core holds everything ControlServer (the human/console-facing
// controlv1.ControlPlaneServer) and RunnerServer (the machine-facing
// runnerv1.RunnerControlServer) need in common: the database handle, the
// runner control-stream session map, the shutdown signal and the
// background-loop recorder. Neither gRPC interface is implemented on Core
// itself -- main.go constructs one Core and hands `&ControlServer{Core}` /
// `&RunnerServer{Core}` to the two Register*Server calls, and also calls
// Core's own lifecycle/background-loop methods (Bootstrap, ScheduleOnce,
// ObserveWorkflows, RecoverOnce, SyncProjects, Shutdown) directly, since none
// of those are part of either gRPC service.
//
// Splitting the type this way keeps the human/machine trust boundary
// structural: a ControlServer value has no method that satisfies
// runnerv1.RunnerControlServer, and a RunnerServer value has no method that
// satisfies controlv1.ControlPlaneServer, so the two surfaces cannot be
// confused at the call site the way two interfaces implemented by one type
// can be. Every helper both sides (or a background loop) need --
// requireActor, the delivery/observation chain, the session map primitives,
// project/workflow/runner lookups -- lives here rather than being forced onto
// one side with the other reaching across a back-reference.
type Core struct {
	pool     *pgxpool.Pool
	queries  *db.Queries
	version  string
	adapters providerAdapters
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
	// loopRecorder reports background-loop liveness through GetSchedulerMetrics.
	// It is wired in by main.go once the metrics recorder exists (SetLoopRecorder),
	// after this Core is already constructed; nil until then, and in most unit
	// tests, which is why every read of it goes through the recorder's own
	// nil-safe methods rather than a nil check here.
	loopRecorder *metrics.Recorder
}

// ControlServer implements controlv1.ControlPlaneServer: every RPC a human
// operator's console session (or the API gateway on its behalf) calls,
// gated by session/CSRF/admin checks rather than a runner credential. It
// holds no state of its own -- every field it needs lives on the shared
// *Core.
type ControlServer struct {
	controlv1.UnimplementedControlPlaneServer
	*Core
}

// RunnerServer implements runnerv1.RunnerControlServer: every RPC a runner
// process calls, authenticated by its own credential/lease rather than a
// human session. It holds no state of its own -- every field it needs lives
// on the shared *Core.
type RunnerServer struct {
	runnerv1.UnimplementedRunnerControlServer
	*Core
}

// SetLoopRecorder wires the background-loop liveness recorder into the server
// so GetSchedulerMetrics can expose it over gRPC, in addition to the recorder's
// own Prometheus surface and readiness endpoint. Optional: a Server this is
// never called on (most unit tests) simply reports no loop statuses.
func (s *Core) SetLoopRecorder(recorder *metrics.Recorder) {
	s.loopRecorder = recorder
}

type actor struct {
	id   string
	role string
}

type projectConfig struct {
	Labels                  []string `json:"required_runner_labels"`
	ExecutionImage          string   `json:"execution_image"`
	ExecutionTimeoutSeconds int32    `json:"execution_timeout_seconds"`
	// RequireHumanApproval opts a project into the human-approval gate: absent
	// (the zero value, false) on every project created before this field
	// existed, since app.projects.configuration defaults to '{}'::jsonb and a
	// missing JSON key decodes to false, not an error. See
	// observeWorkflow/deliveryWorkflow (delivery.go) for where it is read.
	RequireHumanApproval bool `json:"require_human_approval"`
	// RequirePlanning opts a project into the planning phase (#351):
	// ScheduleOnce dispatches a planner-role packet before the developer
	// packet, and persistExecutionEvent folds its output into the developer
	// packet's Plan context once it completes. Absent (the zero value, false)
	// on every project created before this field existed, for the same
	// reason RequireHumanApproval is -- a missing JSON key decodes to false,
	// not an error.
	RequirePlanning bool `json:"require_planning"`
	// EnableAiReview opts a project into dispatching an independent reviewer
	// execution after a developer execution reports success and before
	// delivery (review.go's dispatchReviewerJob). Absent (the zero value,
	// false) on every project created before this field existed, for the same
	// reason RequireHumanApproval is: a missing JSON key decodes to false, not
	// an error, so an existing project's behaviour is unchanged until it opts
	// in. See persistExecutionEvent (server.go) for where it is read.
	EnableAiReview bool `json:"enable_ai_review"`
	// EnableRepairLoop opts a project into a bounded, AI-review-informed repair
	// attempt (repair.go's dispatchRepairJob) instead of terminating a run at
	// StatusBlocked the moment an independent review rejects it. Absent (the
	// zero value, false) on every project for the same reason EnableAiReview
	// is, and only ever consulted for a run that already has a review verdict
	// to repair against -- a project with EnableAiReview off never reaches the
	// code that reads this field at all. Like EnableAiReview, this is read
	// directly from app.projects.configuration; it has no CreateProject/
	// UpdateProject/proto field of its own yet (see EnableAiReview's own gap,
	// #353), so today it can only be set by writing the JSON column directly.
	EnableRepairLoop bool `json:"enable_repair_loop"`
}

// defaultExecutionTimeoutSeconds bounds a dispatched developer execution's
// total wall clock, continuations included (see runner/internal/dispatch's
// goalgate.go), when the project has not configured one of its own. An hour
// is generous for a single autonomous coding-agent attempt at a typical
// issue -- implement, run the pipeline, iterate through a few continuations
// -- while still well short of the 24-hour ceiling the runner's task packet
// validation enforces, and it stops a wedged agent process from holding a
// project's execution slot indefinitely (see issue #276).
const defaultExecutionTimeoutSeconds = 3600

// executionTimeoutSeconds resolves the timeout a developer packet carries: the
// project's own configured value if it set one, or the fixed default. Zero
// means "not configured" rather than "no deadline" -- a real, positive value
// is always sent.
func executionTimeoutSeconds(configured int32) int {
	if configured > 0 {
		return int(configured)
	}
	return defaultExecutionTimeoutSeconds
}

func New(pool *pgxpool.Pool, version string) (*Core, error) {
	if pool == nil {
		return nil, errors.New("database pool is required")
	}
	queries := db.New(pool)
	github := NewGitHubCLI(nil, func(ctx context.Context, projectID, taskSourceID string) (string, error) {
		return resolveGitHubToken(ctx, queries, projectID, taskSourceID)
	})
	return NewWithAdapters(pool, version, github, github, LocalFileTaskSource{})
}

// gitHubLikeAdapter is satisfied by anything backing both TaskSource and
// CodeHost from a single value -- what the real githubCLI does, and what
// every pre-refactor test double for it still does after being updated to
// the new method names/return types.
type gitHubLikeAdapter interface {
	TaskSource
	CodeHost
}

// NewWithGitHub keeps the pre-refactor constructor shape for callers (chiefly
// tests) that only need one adapter standing in for both TaskSource and
// CodeHost, with no local-file task source configured. Use NewWithAdapters
// directly to also wire up a local-file task source.
func NewWithGitHub(pool *pgxpool.Pool, version string, github gitHubLikeAdapter) (*Core, error) {
	return NewWithAdapters(pool, version, github, github, nil)
}

// NewWithAdapters is the general constructor: defaultTaskSource and
// defaultCodeHost back every task source whose provider is unset or "github"
// (every source migration 026 auto-created for a project that predates this
// seam) and every project's code_host_type respectively, and
// localFileTaskSource additionally backs a task source explicitly configured
// with provider = "local_file" (see resolveTaskSource). localFileTaskSource
// may be nil when no caller needs that path (e.g. most tests, and any process
// that never registers a local-file project).
func NewWithAdapters(pool *pgxpool.Pool, version string, defaultTaskSource TaskSource, defaultCodeHost CodeHost, localFileTaskSource TaskSource) (*Core, error) {
	if pool == nil || defaultTaskSource == nil || defaultCodeHost == nil {
		return nil, errors.New("server dependencies are required")
	}
	adapters := providerAdapters{
		defaultTaskSource:   defaultTaskSource,
		defaultCodeHost:     defaultCodeHost,
		localFileTaskSource: localFileTaskSource,
	}
	return &Core{pool: pool, queries: db.New(pool), version: version, adapters: adapters, sessions: make(map[string]chan *runnerv1.OrchestratorToRunner), shutdown: make(chan struct{})}, nil
}

// Shutdown tells every stream handler currently blocked on Connect or
// StreamEvents to return. Safe to call more than once and from any
// goroutine; only the first call has an effect. It must run before (or
// concurrently with, never after) grpc.Server.GracefulStop — GracefulStop
// blocks until in-flight RPCs finish, and both stream handlers only finish
// early because they observe this signal, so calling it after GracefulStop
// has already started blocking would deadlock the very shutdown it exists to
// unblock.
func (s *Core) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdown) })
}

// withShutdown returns a context derived from parent that is also cancelled
// when the server starts shutting down, plus its cancel func. Callers must
// still call parent's own cancellation/Done handling as before; this only
// adds the extra trigger. The goroutine it starts exits as soon as either
// signal fires, so it never outlives the returned context.
func (s *Core) withShutdown(parent context.Context) (context.Context, context.CancelFunc) {
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

func (s *Core) Bootstrap(ctx context.Context) error {
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
			if len(username) > 128 || !secrethash.ValidPassword(password) {
				return errors.New("initial administrator configuration is invalid")
			}
			hash, err := secrethash.PasswordHash(password)
			if err != nil {
				return err
			}
			if err := s.queries.CreateAdminUser(ctx, db.CreateAdminUserParams{ID: idgen.NewID(), Username: username, PasswordHash: hash}); err != nil {
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
	encodedLabels, err := jsonLabels(labels)
	if err != nil {
		return err
	}
	err = s.queries.UpsertSeedRunnerRegistrationToken(ctx, db.UpsertSeedRunnerRegistrationTokenParams{ID: idgen.NewID(), TokenHash: secrethash.HashSecret(token), Column3: []byte(encodedLabels)})
	return databaseError(err)
}

func (s *ControlServer) Login(ctx context.Context, request *controlv1.LoginRequest) (*controlv1.LoginResponse, error) {
	username := strings.TrimSpace(request.GetUsername())
	if username == "" || len(username) > 128 || request.GetPassword() == "" {
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	row, err := s.queries.GetUserByUsername(ctx, username)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = secrethash.PasswordMatches(request.GetPassword(), "scrypt$16384$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	userID, encoded, enabled := row.ID, row.PasswordHash, row.Enabled
	matches, err := secrethash.PasswordMatches(request.GetPassword(), encoded)
	if err != nil {
		// A genuine scrypt failure, not a wrong password: log it so it isn't
		// silently indistinguishable from bad credentials.
		slog.Error("password verification failed", "error", err, "user_id", userID)
	}
	if err != nil || !enabled || !matches {
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	sessionToken, csrfToken := idgen.RandomSecret(), idgen.RandomSecret()
	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	if err := s.queries.CreateUserSession(ctx, db.CreateUserSessionParams{ID: idgen.NewID(), UserID: userID, TokenHash: secrethash.HashSecret(sessionToken), CsrfTokenHash: secrethash.HashSecret(csrfToken), ExpiresAt: pgtype.Timestamptz{Time: expiresAt, Valid: true}}); err != nil {
		return nil, databaseError(err)
	}
	return &controlv1.LoginResponse{SessionToken: sessionToken, UserId: userID, CsrfToken: csrfToken}, nil
}

func (s *ControlServer) WhoAmI(ctx context.Context, _ *controlv1.WhoAmIRequest) (*controlv1.WhoAmIResponse, error) {
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

func (s *ControlServer) Logout(ctx context.Context, _ *controlv1.LogoutRequest) (*controlv1.LogoutResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get(sessionHeader)) != 1 || len(md.Get(csrfHeader)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "session is required")
	}
	affected, err := s.queries.RevokeSessionByTokens(ctx, db.RevokeSessionByTokensParams{TokenHash: secrethash.HashSecret(md.Get(sessionHeader)[0]), CsrfTokenHash: secrethash.HashSecret(md.Get(csrfHeader)[0])})
	if err != nil {
		return nil, databaseError(err)
	}
	if affected != 1 {
		return nil, status.Error(codes.Unauthenticated, "session is invalid")
	}
	return &controlv1.LogoutResponse{}, nil
}

func (s *ControlServer) ListProjects(ctx context.Context, _ *controlv1.ListProjectsRequest) (*controlv1.ListProjectsResponse, error) {
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

func (s *ControlServer) CreateProject(ctx context.Context, request *controlv1.CreateProjectRequest) (*controlv1.CreateProjectResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	cfg, steps, err := validateProject(request.GetProject())
	if err != nil {
		return nil, err
	}
	id := idgen.NewID()
	encoded, err := json.Marshal(projectConfig{Labels: cfg.GetRequiredRunnerLabels(), ExecutionImage: cfg.GetExecutionImage(), ExecutionTimeoutSeconds: cfg.GetExecutionTimeoutSeconds(), RequireHumanApproval: cfg.GetRequireHumanApproval(), RequirePlanning: cfg.GetRequirePlanning()})
	if err != nil {
		return nil, status.Error(codes.Internal, "encode project configuration")
	}
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

func (s *ControlServer) UpdateProject(ctx context.Context, request *controlv1.UpdateProjectRequest) (*controlv1.UpdateProjectResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetProjectId()) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	cfg, steps, err := validateProject(request.GetProject())
	if err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(projectConfig{Labels: cfg.GetRequiredRunnerLabels(), ExecutionImage: cfg.GetExecutionImage(), ExecutionTimeoutSeconds: cfg.GetExecutionTimeoutSeconds(), RequireHumanApproval: cfg.GetRequireHumanApproval(), RequirePlanning: cfg.GetRequirePlanning()})
	if err != nil {
		return nil, status.Error(codes.Internal, "encode project configuration")
	}
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

func (s *ControlServer) SetProjectEnabled(ctx context.Context, request *controlv1.SetProjectEnabledRequest) (*controlv1.SetProjectEnabledResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetProjectId()) {
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

func (s *ControlServer) ListWorkflows(ctx context.Context, _ *controlv1.ListWorkflowsRequest) (*controlv1.ListWorkflowsResponse, error) {
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

func (s *ControlServer) GetWorkflow(ctx context.Context, request *controlv1.GetWorkflowRequest) (*controlv1.GetWorkflowResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	workflow, err := s.workflow(ctx, request.GetWorkflowRunId())
	if err != nil {
		return nil, err
	}
	return &controlv1.GetWorkflowResponse{Workflow: workflow}, nil
}

func (s *ControlServer) ListWorkflowEvents(ctx context.Context, request *controlv1.ListWorkflowEventsRequest) (*controlv1.ListWorkflowEventsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetWorkflowRunId()) || request.GetAfterId() < 0 {
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
			CreatedAt:   textutil.Timestamp(row.CreatedAt.Time),
		}
		response.Events = append(response.Events, &event)
		parsed, err := textutil.ParseInt(event.Id)
		if err != nil {
			return nil, eventIDError(event.Id, err)
		}
		last = parsed
	}
	if len(response.Events) == int(limit) {
		response.NextCursor = fmt.Sprintf("%d", last)
	}
	return response, nil
}

func (s *ControlServer) RetryWorkflow(ctx context.Context, request *controlv1.RetryWorkflowRequest) (*controlv1.RetryWorkflowResponse, error) {
	workflow, err := s.controlWorkflow(ctx, request.GetWorkflowRunId(), request.GetReason(), "retry")
	if err != nil {
		return nil, err
	}
	return &controlv1.RetryWorkflowResponse{Workflow: workflow}, nil
}

func (s *ControlServer) CancelWorkflow(ctx context.Context, request *controlv1.CancelWorkflowRequest) (*controlv1.CancelWorkflowResponse, error) {
	workflow, err := s.controlWorkflow(ctx, request.GetWorkflowRunId(), request.GetReason(), "cancel")
	if err != nil {
		return nil, err
	}
	return &controlv1.CancelWorkflowResponse{Workflow: workflow}, nil
}

func (s *ControlServer) BlockWorkflow(ctx context.Context, request *controlv1.BlockWorkflowRequest) (*controlv1.BlockWorkflowResponse, error) {
	workflow, err := s.controlWorkflow(ctx, request.GetWorkflowRunId(), request.GetReason(), "block")
	if err != nil {
		return nil, err
	}
	return &controlv1.BlockWorkflowResponse{Workflow: workflow}, nil
}

func (s *ControlServer) SyncNow(ctx context.Context, request *controlv1.SyncNowRequest) (*controlv1.SyncNowResponse, error) {
	if _, err := s.requireMutation(ctx); err != nil {
		return nil, err
	}
	projectID := strings.TrimSpace(request.GetProjectId())
	if projectID != "" && !idgen.ValidID(projectID) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	var projectIDs []string
	if projectID != "" {
		enabled, err := s.queries.ProjectIsEnabled(ctx, projectID)
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, status.Error(codes.NotFound, "enabled project is unknown")
		}
		if err != nil {
			return nil, databaseError(err)
		}
		if !enabled {
			return nil, status.Error(codes.NotFound, "enabled project is unknown")
		}
		projectIDs = []string{projectID}
	} else {
		ids, err := s.queries.ListEnabledProjectIDs(ctx)
		if err != nil {
			return nil, databaseError(err)
		}
		projectIDs = ids
	}
	response := &controlv1.SyncNowResponse{}
	for _, id := range projectIDs {
		result := &controlv1.ProjectSyncResult{ProjectId: id}
		if err := s.syncProject(ctx, id); err != nil {
			result.Error = syncErrorMessage(err)
		} else {
			count, err := s.queries.CountProjectIssues(ctx, id)
			if err != nil {
				return nil, databaseError(err)
			}
			result.SyncedIssues = int32(count)
		}
		response.Results = append(response.Results, result)
	}
	return response, nil
}

// SyncProjects refreshes the issue snapshot for every enabled project's
// enabled task sources. It is the unattended half of SyncNow: the console's
// "Sync now" button covers an operator who is watching, and this covers the
// deployments that nobody is.
//
// One source's failure does not abandon its siblings, on the same project or
// any other -- a Jira source with a revoked token would otherwise stop a
// healthy GitHub source on the same project, or every other project's
// sources, from discovering work. Failures are recorded per source (which is
// what drives app.issue_sync_state's per-source backoff) and reported
// together.
func (s *Core) SyncProjects(ctx context.Context) error {
	// ListSourcesDueForSync (unlike ListSyncableSources, which SyncNow uses)
	// excludes a source still inside the backoff window recordSyncFailure set
	// on it, so a repository with a revoked token or a deleted remote is not
	// retried at full rate forever.
	sources, err := s.queries.ListSourcesDueForSync(ctx)
	if err != nil {
		return databaseError(err)
	}
	var failures []error
	for _, source := range sources {
		if err := s.syncSource(ctx, source.ID, source.ProjectID, source.Provider, sourceRef(source.Configuration)); err != nil {
			failures = append(failures, fmt.Errorf("task source %s (project %s): %w", source.ID, source.ProjectID, err))
		}
	}
	return errors.Join(failures...)
}

func (s *ControlServer) IssueSyncStatus(ctx context.Context, _ *controlv1.IssueSyncStatusRequest) (*controlv1.IssueSyncStatusResponse, error) {
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
			// ConsecutiveFailures/LastError are aggregated (COALESCEd to
			// 0/"") across a project's task sources, so they are never NULL:
			// a project with zero sources reports the same "nothing to
			// show" values a project whose one source has never failed
			// already did.
			ConsecutiveFailures: row.ConsecutiveFailures,
			LastError:           row.LastError,
		}
		if row.LastSyncedAt.Valid {
			entry.LastSyncedAt = textutil.Timestamp(row.LastSyncedAt.Time)
		}
		if row.NextRetryAt.Valid {
			entry.NextRetryAt = textutil.Timestamp(row.NextRetryAt.Time)
		}
		response.Entries = append(response.Entries, entry)
	}
	return response, nil
}

// providerName normalizes a task source's provider (or a project's
// code_host_type) column into the value persisted as app.issues.provider /
// app.pull_requests.provider: the schema default and every pre-existing
// project have it empty or "github" respectively, and both mean the same
// thing to the rest of the system, so both are recorded identically.
func providerName(configuredType string) string {
	if configuredType == "" {
		return "github"
	}
	return configuredType
}

// sourceRef reads the "ref" key out of a task source's own
// configuration JSONB (app.project_task_sources.configuration), which is
// where syncSource's ref argument comes from. Every source migration 026
// auto-created has one, copied from the project's old repository_url; a
// malformed or absent value degrades to "" rather than failing the sync
// outright, matching how a NULL repository_url read as "" before this
// column existed (see textutil.TextValue's prior use here).
func sourceRef(configuration string) string {
	var decoded struct {
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal([]byte(configuration), &decoded); err != nil {
		return ""
	}
	return decoded.Ref
}

// legacyTaskLabels extracts the GitHub-era "Labels" array back out of a
// task's raw provider snapshot for app.issues.labels, kept only for
// backward-compatible audit: nothing in the application reads that column
// back. The neutral Task type intentionally carries no label list of its own
// (only the already-derived Priority/Eligible) -- a source whose raw snapshot
// has no such key (like LocalFileTaskSource's) simply leaves the column
// empty.
func legacyTaskLabels(raw json.RawMessage) []byte {
	var decoded struct {
		Labels []string `json:"Labels"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil || decoded.Labels == nil {
		return []byte("[]")
	}
	encoded, err := json.Marshal(decoded.Labels)
	if err != nil {
		return []byte("[]")
	}
	return encoded
}

// syncProject refreshes every enabled task source configured for one
// project. A project with zero configured sources (valid -- see #293) simply
// iterates nothing and returns nil: there is no error path for "nothing to
// sync". One source's failure does not stop its siblings on the same
// project, matching SyncProjects' cross-project isolation one level down;
// every failure is still collected and returned together so a caller (SyncNow)
// can report all of them rather than only the first.
func (s *Core) syncProject(ctx context.Context, projectID string) error {
	sources, err := s.queries.ListSyncableSourcesByProject(ctx, projectID)
	if err != nil {
		return databaseError(err)
	}
	var failures []error
	for _, source := range sources {
		if err := s.syncSource(ctx, source.ID, source.ProjectID, source.Provider, sourceRef(source.Configuration)); err != nil {
			failures = append(failures, fmt.Errorf("task source %s: %w", source.ID, err))
		}
	}
	return errors.Join(failures...)
}

// syncSource refreshes one task source's issue snapshot: the unit of work
// both SyncProjects and syncProject (SyncNow's per-project helper) iterate
// over. provider is the source's own app.project_task_sources.provider
// (selecting the adapter via resolveTaskSource), and ref is that source's own
// configuration->>'ref' (see sourceRef) -- both scoped to this one source,
// never to the project as a whole, which is what lets two sources on the
// same project use different adapters and point at different repositories/
// directories/queries.
func (s *Core) syncSource(ctx context.Context, sourceID, projectID, provider, ref string) error {
	adapter, err := s.adapters.resolveTaskSource(provider)
	if err != nil {
		return err
	}
	tasks, err := adapter.ListTasks(ctx, projectID, sourceID, ref)
	if err != nil {
		_ = s.recordSyncFailure(ctx, sourceID, projectID, err)
		return err
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	recordedProvider := providerName(provider)
	// Eligible is written unconditionally from the source's own derivation on
	// every sync (for GitHub: issuePriority reading agent:ready/agent:blocked/
	// agent:delivered labels) — it is not, by itself, the scheduler's
	// candidate signal any more. A run that ends without delivering excludes
	// its issue from scheduling via app.workflow_runs.status/superseded_at
	// instead (see ListQueueEntries and ClaimSchedulableIssue), so a source
	// reporting a task ineligible always takes effect, and only RetryWorkflow
	// (which supersedes the run) reopens work the scheduler had parked. See
	// #268: this used to be a single label-driven bit the orchestrator's own
	// lifecycle also wrote, which meant a sync pass had to stop recomputing
	// it from labels entirely once any run existed, silently breaking
	// operator label edits from that point on.
	//
	// task.State likewise comes straight from the source on every sync (the
	// GitHub adapter fetches --state all instead of --state open), so a task
	// closed on the tracker is reconciled to state='closed' here instead of
	// staying 'open' -- and therefore schedulable -- forever just because a
	// sync that only ever asked for open issues could never observe the
	// close.
	for _, task := range tasks {
		// raw_snapshot is NOT NULL; a TaskSource is not required to populate
		// Task.Raw (it is an audit convenience, not part of the contract), so
		// a nil snapshot here becomes an empty JSON object rather than a
		// constraint violation.
		raw := task.Raw
		if raw == nil {
			raw = json.RawMessage("{}")
		}
		if err := queries.UpsertIssue(ctx, db.UpsertIssueParams{
			ID: idgen.NewID(), ProjectID: projectID, TaskSourceID: sourceID, Provider: recordedProvider, ExternalID: task.ExternalID, Title: task.Title, Body: task.Body, Url: task.URL,
			State: task.State, Column10: legacyTaskLabels(raw), Priority: int32(task.Priority), Eligible: task.Eligible,
			ExternalCreatedAt: pgtype.Timestamptz{Time: task.CreatedAt, Valid: true},
			ExternalUpdatedAt: pgtype.Timestamptz{Time: task.UpdatedAt, Valid: true},
			Column15:          raw,
		}); err != nil {
			return databaseError(err)
		}
	}
	if err := queries.UpsertIssueSyncStateSuccess(ctx, db.UpsertIssueSyncStateSuccessParams{TaskSourceID: sourceID, ProjectID: projectID}); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Core) recordSyncFailure(ctx context.Context, sourceID, projectID string, cause error) error {
	err := s.queries.UpsertIssueSyncStateFailure(ctx, db.UpsertIssueSyncStateFailureParams{
		TaskSourceID: sourceID, ProjectID: projectID, LastError: pgtype.Text{String: textutil.Truncate(cause.Error(), 1024), Valid: true},
	})
	return databaseError(err)
}

func (s *ControlServer) ListRunners(ctx context.Context, _ *controlv1.ListRunnersRequest) (*controlv1.ListRunnersResponse, error) {
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

func (s *ControlServer) SetRunnerState(ctx context.Context, request *controlv1.SetRunnerStateRequest) (*controlv1.SetRunnerStateResponse, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(request.GetRunnerId()) {
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

func (s *ControlServer) ListQueue(ctx context.Context, request *controlv1.ListQueueRequest) (*controlv1.ListQueueResponse, error) {
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
func (s *Core) readSchedulerSnapshot(ctx context.Context) (schedulerSnapshot, error) {
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

func (s *ControlServer) GetSchedulerMetrics(ctx context.Context, _ *controlv1.GetSchedulerMetricsRequest) (*controlv1.GetSchedulerMetricsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	snapshot, err := s.readSchedulerSnapshot(ctx)
	if err != nil {
		return nil, err
	}
	response := &controlv1.GetSchedulerMetricsResponse{
		QueueDepth:      int32(snapshot.queueDepth),
		ActiveWorkflows: int32(snapshot.activeWorkflows),
		ScheduledJobs:   int32(snapshot.scheduledJobs),
	}
	// LoopStatuses() is nil-safe: a Server that never had SetLoopRecorder called
	// on it (most unit tests, and any process that has not finished starting up
	// yet) reports no loop statuses rather than panicking.
	for _, status := range s.loopRecorder.LoopStatuses() {
		entry := &controlv1.LoopStatus{Name: status.Name, Healthy: status.Healthy}
		if !status.LastSuccess.IsZero() {
			entry.LastSuccessAt = textutil.Timestamp(status.LastSuccess)
		}
		if status.LastError != "" {
			entry.LastError = status.LastError
			entry.LastErrorAt = textutil.Timestamp(status.LastErrorAt)
		}
		response.LoopStatuses = append(response.LoopStatuses, entry)
	}
	return response, nil
}

// MetricsSnapshot reads the orchestrator-owned state the Prometheus surface
// exports. It runs on the scrape's goroutine, against the same pooled
// connections every other query uses — one round trip, no connection of its
// own — and returns an error rather than a zeroed snapshot when the database
// cannot answer, so the exporter can omit the series instead of publishing a
// zero nothing measured.
func (s *Core) MetricsSnapshot(ctx context.Context) (metrics.Snapshot, error) {
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
func (s *ControlServer) GetSystemVersion(_ context.Context, _ *controlv1.GetSystemVersionRequest) (*controlv1.GetSystemVersionResponse, error) {
	return &controlv1.GetSystemVersionResponse{Version: s.version}, nil
}

func (s *RunnerServer) RegisterRunner(ctx context.Context, request *runnerv1.RegisterRunnerRequest) (*runnerv1.RegisterRunnerResponse, error) {
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
	encodedLabels, err := jsonLabels(labels)
	if err != nil {
		return nil, status.Error(codes.Internal, "encode runner labels")
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	tokenID, err := queries.SelectValidRegistrationToken(ctx, db.SelectValidRegistrationTokenParams{TokenHash: secrethash.HashSecret(request.GetToken()), Column2: []byte(encodedLabels)})
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.PermissionDenied, "runner registration was rejected")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	runnerID, credential := idgen.NewID(), idgen.RandomSecret()
	if err := queries.CreateRunner(ctx, db.CreateRunnerParams{ID: runnerID, Name: strings.TrimSpace(request.GetName()), Column3: []byte(encodedLabels), Capacity: capacity}); err != nil {
		return nil, databaseError(err)
	}
	if err := queries.CreateRunnerCredential(ctx, db.CreateRunnerCredentialParams{ID: idgen.NewID(), RunnerID: runnerID, CredentialHash: secrethash.HashSecret(credential)}); err != nil {
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

func (s *RunnerServer) Connect(stream grpc.BidiStreamingServer[runnerv1.RunnerToOrchestrator, runnerv1.OrchestratorToRunner]) error {
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

func (s *Core) handleRunnerMessage(ctx context.Context, runnerID string, message *runnerv1.RunnerToOrchestrator) error {
	switch {
	case message.GetHeartbeat() != nil:
		heartbeat := message.GetHeartbeat()
		if _, err := normalizeLabels(heartbeat.GetLabels()); err != nil {
			return status.Error(codes.InvalidArgument, "runner heartbeat labels are invalid")
		}
		err := s.queries.RecordRunnerHeartbeat(ctx, db.RecordRunnerHeartbeatParams{ID: runnerID, Column2: textutil.Truncate(heartbeat.GetVersion(), 12)})
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

func (s *Core) addSession(runnerID string, outbound chan *runnerv1.OrchestratorToRunner) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.sessions[runnerID]; exists {
		return false
	}
	s.sessions[runnerID] = outbound
	return true
}

func (s *Core) removeSession(runnerID string, outbound chan *runnerv1.OrchestratorToRunner) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.sessions[runnerID] == outbound {
		delete(s.sessions, runnerID)
	}
}

func (s *Core) enqueue(runnerID string, message *runnerv1.OrchestratorToRunner) bool {
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

func (s *Core) connectedRunners() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	runners := make([]string, 0, len(s.sessions))
	for runnerID := range s.sessions {
		runners = append(runners, runnerID)
	}
	return runners
}

func (s *Core) ScheduleOnce(ctx context.Context) (bool, error) {
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
	workflowID, jobID, offerID := idgen.NewID(), idgen.NewID(), idgen.NewID()
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
	var config projectConfig
	if err := json.Unmarshal(configuration, &config); err != nil {
		return false, configurationError(err)
	}
	// A project that opted into RequirePlanning gets a 'planner' job: its
	// first execution is a planner-role packet rather than the developer one,
	// and dispatchImplementationJob (server.go) reopens this same job (app.jobs.
	// workflow_run_id stays UNIQUE) from 'planner' back to 'developer' once
	// that execution completes, carrying its plan forward as the developer
	// packet's Plan context. See jobRolePlanner's doc comment (review.go).
	role := jobRoleDeveloper
	if config.RequirePlanning {
		role = jobRolePlanner
	}
	if err := queries.CreateJob(ctx, db.CreateJobParams{ID: jobID, WorkflowRunID: workflowID, ProjectID: projectID, RunnerID: runnerID, Role: role}); err != nil {
		return false, databaseError(err)
	}
	if err := queries.CreateJobOffer(ctx, db.CreateJobOfferParams{ID: offerID, JobID: jobID, RunnerID: runnerID}); err != nil {
		return false, databaseError(err)
	}
	var packet map[string]any
	if config.RequirePlanning {
		if err := queries.IncrementPlanningAttempts(ctx, workflowID); err != nil {
			return false, databaseError(err)
		}
		packet, err = plannerPacket(jobID, projectID, externalID, title, body, mode, textutil.TextValue(repositoryURL), textutil.TextValue(localPath), defaultBranch, branch, config.ExecutionImage, executionTimeoutSeconds(config.ExecutionTimeoutSeconds))
	} else {
		pipeline, pipelineErr := pipelineStepsForPacket(ctx, queries, projectID)
		if pipelineErr != nil {
			return false, pipelineErr
		}
		packet, err = developerPacket(jobID, projectID, externalID, title, body, mode, textutil.TextValue(repositoryURL), textutil.TextValue(localPath), defaultBranch, branch, config.ExecutionImage, executionTimeoutSeconds(config.ExecutionTimeoutSeconds), nil, pipeline)
	}
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

func (s *Core) releaseUndeliveredOffer(jobID, workflowID, projectID string) error {
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

// developerPacket builds the execution that writes the code and publishes the
// branch. It is the only execution a V1 workflow dispatches unless the
// project opted into RequirePlanning (#351), in which case plannerPacket goes
// out first and this one follows once persistExecutionEvent sees it complete,
// carrying that execution's plan forward as this packet's Plan context.
//
// The role has to be `developer`. The runner refuses to modify or push for a
// planner, pipeline or reviewer packet, and only pushes at all for a role
// granted mayPush — so a planner packet leaves the agent branch unpublished and
// the delivery step that follows opens a pull request from a branch the remote
// has never heard of.
//
// mayMerge stays false in every packet: merging is the orchestrator's decision,
// taken after GitHub reports the checks green, and the runner rejects a packet
// that claims otherwise.
// implementExecutionID derives the execution ID the runner will report back
// against a job's developer execution. Deterministic from the job ID rather
// than stored, so cancelling a job can address the runner's active execution
// without another round trip through the database. See planExecutionID for
// the same job's planner execution, when it has one.
func implementExecutionID(jobID string) string {
	return jobID + "-implement"
}

// repositoryEnvironmentRefs declares the code-host credential a packet's
// repository operations need. The runner resolves each declared reference
// (control plane credential first, the runner's own environment second) and
// uses it to authenticate the clone, fetch, and push that populate the
// workspace; an undeclared reference is never even looked up, which is what
// left a GitHub HTTPS managed clone unauthenticated (#391) -- git then tried
// to prompt for a username and failed with "could not read Username ... No
// such device or address" because there is no terminal to read one from.
//
// Only a managed clone from a GitHub HTTPS URL declares anything (GITHUB_TOKEN).
// Every other repository shape authenticates some other way -- an SSH key the
// runner host already has, or a code host its git already trusts -- and
// declaring a reference it cannot resolve would fail the execution outright,
// so those shapes declare nothing.
func repositoryEnvironmentRefs(mode, repositoryURL string) []map[string]string {
	if mode == "managed_clone" && strings.HasPrefix(repositoryURL, "https://github.com/") {
		return []map[string]string{{"name": "GITHUB_TOKEN", "secretRef": "github_token"}}
	}
	return []map[string]string{}
}

// planExecutionID is implementExecutionID's counterpart for a job's planning
// execution (#351) -- distinct so a CancelExecution sent while a job is still
// planning addresses the execution actually running, not the developer one
// that has not been dispatched yet.
func planExecutionID(jobID string) string {
	return jobID + "-plan"
}

func developerPacket(jobID, projectID, externalID, title, body, mode, repositoryURL, localPath, defaultBranch, branch, executionImage string, timeoutSeconds int, plan []string, pipeline []map[string]any) (map[string]any, error) {
	if !idgen.ValidID(jobID) || !idgen.ValidID(projectID) || externalID == "" || title == "" || defaultBranch == "" || branch == "" || (mode == "managed_clone" && repositoryURL == "") || (mode == "existing_path" && localPath == "") || (mode != "managed_clone" && mode != "existing_path") || timeoutSeconds < 1 {
		return nil, status.Error(codes.FailedPrecondition, "scheduled task is invalid")
	}
	if plan == nil {
		plan = []string{}
	}
	if pipeline == nil {
		pipeline = []map[string]any{}
	}
	return map[string]any{
		"protocolVersion": "1.0", "jobId": jobID, "executionId": implementExecutionID(jobID), "role": "developer", "objective": "Implement " + externalID + ": " + title,
		"issue":      map[string]string{"externalId": externalID, "title": title, "body": body},
		"repository": map[string]string{"projectId": projectID, "mode": mode, "url": repositoryURL, "localPath": localPath, "defaultBranch": defaultBranch, "branch": branch},
		"promptPath": ".loop/prompt.md", "expectedOutput": ".loop/result.json", "timeoutSeconds": timeoutSeconds, "environmentRefs": repositoryEnvironmentRefs(mode, repositoryURL), "executionImage": executionImage,
		"constraints": map[string]bool{"mayModifyFiles": true, "mayPush": true, "mayMerge": false}, "pipeline": pipeline, "acceptanceCriteria": []string{}, "plan": plan, "previousFailures": []string{}, "currentCommit": "", "diffSummary": "", "failedChecks": []string{}, "reviewFindings": []string{},
	}, nil
}

// pipelineStepsForPacket reads a project's configured pipeline steps
// (app.project_pipeline_steps, configured through ProjectConfiguration's
// pipeline_steps -- see web/src/projects.tsx's "Local pipeline" fieldset) and
// renders them as a developer or repair packet's pipeline field: PROJECT.md's
// deterministic completion gate. A project with none configured -- every
// project created before this was wired up, and any that simply never opted
// in -- gets an empty slice, so the runner's own len(packet.Pipeline)>0 guard
// (runner/internal/dispatch/dispatch.go) never runs a pipeline for it and its
// behaviour is unchanged. Each step's own Required flag travels through
// unchanged: the runner (runner/internal/pipeline) only gates completion on a
// required step's own failure, exactly as web/src/projects.tsx's "Required
// commands must pass before AI review. No required command blocks
// completion" already promises.
func pipelineStepsForPacket(ctx context.Context, queries *db.Queries, projectID string) ([]map[string]any, error) {
	steps, err := queries.ListProjectPipelineSteps(ctx, projectID)
	if err != nil {
		return nil, databaseError(err)
	}
	commands := make([]map[string]any, 0, len(steps))
	for _, step := range steps {
		timeout := step.TimeoutSeconds
		// project_pipeline_steps.timeout_seconds only CHECKs > 0; the task
		// packet's own protocol ceiling (taskpacket.go, ProtocolVersion 1.0) is
		// 3600 seconds per command. Clamped here rather than left to fail
		// packet validation outright, which would silently stop this project's
		// workflow from ever dispatching a developer execution again.
		if timeout > 3600 {
			timeout = 3600
		}
		commands = append(commands, map[string]any{"command": step.Command, "timeoutSeconds": timeout, "required": step.Required})
	}
	return commands, nil
}

// plannerPacket builds the execution a RequirePlanning project runs before its
// developer packet (#351). It shares the developer packet's repository and
// issue context, but its constraints refuse both mayModifyFiles and mayPush —
// enforced independently by the runner's own taskpacket.Validate, not just
// trusted here — so the runner's existing role-generic dispatch (runner/
// internal/dispatch/dispatch.go) never commits or pushes on its behalf: it
// runs the agent, snapshots the repository, and reports its summary and
// remaining-work list as the plan, exactly the way any other non-modifying
// role already does. No runner-side change was needed to support this role.
func plannerPacket(jobID, projectID, externalID, title, body, mode, repositoryURL, localPath, defaultBranch, branch, executionImage string, timeoutSeconds int) (map[string]any, error) {
	if !idgen.ValidID(jobID) || !idgen.ValidID(projectID) || externalID == "" || title == "" || defaultBranch == "" || branch == "" || (mode == "managed_clone" && repositoryURL == "") || (mode == "existing_path" && localPath == "") || (mode != "managed_clone" && mode != "existing_path") || timeoutSeconds < 1 {
		return nil, status.Error(codes.FailedPrecondition, "scheduled task is invalid")
	}
	return map[string]any{
		"protocolVersion": "1.0", "jobId": jobID, "executionId": planExecutionID(jobID), "role": "planner", "objective": "Plan an implementation for " + externalID + ": " + title,
		"issue":      map[string]string{"externalId": externalID, "title": title, "body": body},
		"repository": map[string]string{"projectId": projectID, "mode": mode, "url": repositoryURL, "localPath": localPath, "defaultBranch": defaultBranch, "branch": branch},
		"promptPath": ".loop/prompt.md", "expectedOutput": ".loop/result.json", "timeoutSeconds": timeoutSeconds, "environmentRefs": repositoryEnvironmentRefs(mode, repositoryURL), "executionImage": executionImage,
		"constraints": map[string]bool{"mayModifyFiles": false, "mayPush": false, "mayMerge": false}, "pipeline": []any{}, "acceptanceCriteria": []string{}, "plan": []string{}, "previousFailures": []string{}, "currentCommit": "", "diffSummary": "", "failedChecks": []string{}, "reviewFindings": []string{},
	}, nil
}

func (s *Core) acceptOffer(ctx context.Context, runnerID, jobID string) (int64, time.Time, error) {
	if !idgen.ValidID(jobID) {
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
	// accepted.Role (AcceptJob's own RETURNING) is how acceptOffer tells a
	// planning offer apart from a developer or reviewer one: it only ever has
	// the job ID, never the task packet. A reviewer's own job offer being
	// accepted must not move the run off StatusWaitingAiReview: unlike a
	// developer's or a planner's job, there is no separate "in flight" status
	// for a review -- StatusWaitingAiReview itself covers the whole of
	// dispatch, offer, acceptance and execution, the same way StatusPreparing
	// alone covers a developer's, and StatusPlanning a planner's, without a
	// distinct "running" status of their own (see status.go).
	switch accepted.Role {
	case jobRoleDeveloper:
		if err := queries.SetWorkflowPreparing(ctx, jobID); err != nil {
			return 0, time.Time{}, databaseError(err)
		}
	case jobRolePlanner:
		if err := queries.SetWorkflowPlanningActive(ctx, jobID); err != nil {
			return 0, time.Time{}, databaseError(err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	return generation, expiresAt, nil
}

func (s *Core) rejectOffer(ctx context.Context, runnerID, jobID, reason string) error {
	if !idgen.ValidID(jobID) {
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
	if err := queries.CancelJob(ctx, db.CancelJobParams{ID: jobID, Column2: textutil.Truncate(reason, 1024)}); err != nil {
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

func (s *Core) renewLease(ctx context.Context, runnerID string, renewal *runnerv1.LeaseRenewal) (time.Time, error) {
	if !idgen.ValidID(renewal.GetJobId()) || renewal.GetLeaseGeneration() < 1 || renewal.GetRequestedExpiresAtUnixMs() <= time.Now().UnixMilli() {
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

// planFromPayload extracts the plan a planner execution's terminal payload
// carries (runner/internal/dispatch/control_loop.go's terminalPayload:
// "summary" and, when non-empty, "remainingWork") into the developer packet's
// Plan context: the summary first, then each remaining-work entry, the same
// order promptFor (runner/internal/dispatch/dispatch.go) renders packet.Plan
// in under "# CURRENT PLAN". Both fields already passed through the runner's
// own boundedAgentText/boundedList limits when the planner reported them, well
// inside the developer packet's own Plan bounds (taskpacket.go: 64 entries,
// 8192 bytes each), so nothing here needs to re-bound them.
func planFromPayload(payloadJSON string) []string {
	var payload struct {
		Summary       string   `json:"summary"`
		RemainingWork []string `json:"remainingWork"`
	}
	if err := json.Unmarshal([]byte(payloadJSON), &payload); err != nil {
		return nil
	}
	plan := make([]string, 0, len(payload.RemainingWork)+1)
	if payload.Summary != "" {
		plan = append(plan, payload.Summary)
	}
	plan = append(plan, payload.RemainingWork...)
	return plan
}

// dispatchImplementationJob runs once persistExecutionEvent's own transaction
// has committed a planner's "completed" event (#351) -- the same after-commit
// shape dispatchReviewerJob (review.go, #353) already runs in for a
// developer's own "completed" event. It records the plan, reopens the
// workflow run's one job (app.jobs.workflow_run_id stays UNIQUE) from
// 'planner' back to 'developer' via ReopenJobForImplementation -- the same
// one-job-many-roles mechanism ReopenJobForReview uses, just reopened
// backwards, from the first execution to the second rather than after it --
// and offers the resulting developer packet, carrying the plan forward as its
// Plan context.
//
// Unlike dispatchReviewerJob, a disconnected or unreachable runner is not
// retried by a recovery sweep: the job is re-offered to the same runner_id
// planning already ran on (no fresh eligible-runner selection, since planning
// and implementation are one continuous attempt, not an independent second
// pass), and a failed enqueue undoes the offer via releaseUndeliveredOffer --
// the same fallback ScheduleOnce's own enqueue failure uses -- rather than
// leaving the run stranded waiting for a runner that may never reconnect.
func (s *Core) dispatchImplementationJob(ctx context.Context, workflowID, jobID, payloadJSON string) error {
	plan := planFromPayload(payloadJSON)
	summary := ""
	if len(plan) > 0 {
		summary = plan[0]
	}
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return databaseError(err)
	}
	defer tx.Rollback(ctx)
	queries := s.queries.WithTx(tx)
	if err := queries.RecordPlanSummary(ctx, db.RecordPlanSummaryParams{ID: workflowID, PlanSummary: summary}); err != nil {
		return databaseError(err)
	}
	// A dedicated event, distinct from the "completed" row persistExecutionEvent
	// already wrote, so the console's timeline reads "the plan is ready" rather
	// than "an agent execution completed" for a phase an operator has no other
	// way to tell apart from the developer one that follows it.
	planPayload, err := json.Marshal(map[string]any{"plan": plan})
	if err != nil {
		return databaseError(err)
	}
	if err := queries.CreateWorkflowEvent(ctx, db.CreateWorkflowEventParams{
		WorkflowRunID: workflowID, EventType: "plan.recorded", Severity: "info", Column4: planPayload,
	}); err != nil {
		return databaseError(err)
	}
	reopened, err := queries.ReopenJobForImplementation(ctx, jobID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil // raced with another dispatch attempt for this same job
	}
	if err != nil {
		return databaseError(err)
	}
	facts, err := queries.GetImplementationDispatchFacts(ctx, workflowID)
	if err != nil {
		return databaseError(err)
	}
	var config projectConfig
	if err := json.Unmarshal([]byte(facts.Configuration), &config); err != nil {
		return configurationError(err)
	}
	pipeline, err := pipelineStepsForPacket(ctx, queries, facts.ProjectID)
	if err != nil {
		return err
	}
	packet, err := developerPacket(jobID, facts.ProjectID, facts.ExternalID, facts.Title, facts.Body, facts.RepositoryMode,
		textutil.TextValue(facts.RepositoryUrl), textutil.TextValue(facts.LocalRepositoryPath), facts.DefaultBranch,
		textutil.TextValue(facts.BranchName), config.ExecutionImage, executionTimeoutSeconds(config.ExecutionTimeoutSeconds), plan, pipeline)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(packet)
	if err != nil {
		return databaseError(err)
	}
	if err := queries.CreateJobOffer(ctx, db.CreateJobOfferParams{ID: idgen.NewID(), JobID: jobID, RunnerID: reopened.RunnerID}); err != nil {
		return databaseError(err)
	}
	if err := tx.Commit(ctx); err != nil {
		return databaseError(err)
	}
	message := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: &runnerv1.JobOffer{
		JobId: jobID, LeaseGeneration: reopened.LeaseGeneration, TaskPacketJson: string(encoded),
	}}}
	if s.enqueue(reopened.RunnerID, message) {
		return nil
	}
	return s.releaseUndeliveredOffer(jobID, workflowID, facts.ProjectID)
}

// jsonUnicodeEscapeMarker is how a `\uXXXX` escape spells in JSON text --
// the only place either of the two bytes sanitizeEventPayload cares about,
// NUL and an unpaired surrogate half, can appear in text that already passed
// json.Valid: a literal NUL byte is a raw control character, which json.Valid
// itself rejects (encoding/json requires it escaped), and a bare surrogate
// half has no other spelling. Payload text with no `\u` escape at all -- the
// overwhelming majority of events -- is guaranteed clean and returned
// untouched, so the decode-and-rebuild path below only runs, and only
// reorders map keys, on the rare payload that needs it.
const jsonUnicodeEscapeMarker = `\u`

// sanitizeEventPayload returns payloadJSON, or an equivalent JSON document
// with every NUL byte and unpaired UTF-16 surrogate replaced by U+FFFD
// (the Unicode replacement character), if either was present. Both are legal
// inside a JSON `\uXXXX` escape -- json.Valid accepts them -- but Postgres's
// jsonb rejects both when storing them ("unsupported Unicode escape
// sequence" for NUL, "invalid input syntax for type json" for a lone
// surrogate), which would otherwise abort the INSERT this payload is headed
// for. The fix runs here, not in the runner, because this is the trust
// boundary: payloadJSON forwards an agent's own result document verbatim,
// and an event this server cannot store must still not be dropped -- a lost
// terminal event stops the run from ever reaching a terminal status.
//
// Decoding into `any` and walking it (rather than a textual replace) is what
// makes this correct at any nesting depth, including inside object keys, and
// UseNumber preserves large integers exactly instead of rounding them
// through float64. If payloadJSON is somehow no longer valid JSON by the
// time this runs (unreachable: the caller already ran json.Valid on the same
// bytes), decoding fails and the original text is returned -- returning the
// untouched payload is still strictly better than failing the event outright,
// and the ::jsonb insert will surface the real error if it still cannot
// store it.
func sanitizeEventPayload(payloadJSON string) string {
	if !strings.Contains(payloadJSON, jsonUnicodeEscapeMarker) {
		return payloadJSON
	}
	dec := json.NewDecoder(strings.NewReader(payloadJSON))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return payloadJSON
	}
	sanitized, err := json.Marshal(stripNUL(v))
	if err != nil {
		return payloadJSON
	}
	return string(sanitized)
}

// nulReplacement is what a NUL byte, or an unpaired surrogate half already
// folded to U+FFFD by json.Decode, becomes in stored payload text. It is
// printable so the console's event timeline still renders the field as
// plain text rather than showing a hole in the string.
const nulReplacement = "�"

// stripNUL walks a decoded JSON value replacing every NUL byte, in any
// string it finds at any depth -- object keys included, since a NUL is just
// as fatal to jsonb there as in a value -- with nulReplacement. Unpaired
// surrogates need no separate handling: json.Decode already replaced them
// with U+FFFD (a valid rune) while producing v, so only a literal NUL byte
// (the one escape sequence encoding/json does not itself reject) can still
// be present here.
func stripNUL(v any) any {
	switch t := v.(type) {
	case string:
		if !strings.ContainsRune(t, 0) {
			return t
		}
		return strings.ReplaceAll(t, "\x00", nulReplacement)
	case map[string]any:
		out := make(map[string]any, len(t))
		for k, val := range t {
			if strings.ContainsRune(k, 0) {
				k = strings.ReplaceAll(k, "\x00", nulReplacement)
			}
			out[k] = stripNUL(val)
		}
		return out
	case []any:
		out := make([]any, len(t))
		for i, val := range t {
			out[i] = stripNUL(val)
		}
		return out
	default:
		return v
	}
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
	// A canceled or timed-out context is not a database failure: it means the
	// caller (a disconnected client, a shutdown, a deadline) stopped waiting
	// while the query was in flight. Logging it as ERROR spams the console
	// with noise that has nothing to do with Postgres, so it is surfaced to
	// the caller with the matching gRPC code and no error log.
	if errors.Is(err, context.Canceled) {
		return status.Error(codes.Canceled, "request canceled")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return status.Error(codes.DeadlineExceeded, "request deadline exceeded")
	}
	slog.Error("database operation failed", "error", err)
	return status.Error(codes.Internal, "database operation failed")
}

// syncErrorMessage renders a per-project SyncNow failure for the console.
// syncProject's error can be a plain Go error (a malformed repository URL) or
// one already run through databaseError/configurationError, which wraps it as
// a gRPC status so it survives the RPC boundary correctly elsewhere in this
// server. But ProjectSyncResult.Error is a plain string field read straight
// by an operator, not another gRPC hop, so passing a status error through
// err.Error() would print the wrapper verbatim: "rpc error: code = Internal
// desc = database operation failed". status.Convert treats a non-status error
// as an Unknown-code status carrying the original message unchanged, so
// Message() gives the clean text either way.
func syncErrorMessage(err error) string {
	return status.Convert(err).Message()
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

// eventIDError reports a workflow event whose id (a bigint stored as text)
// failed to parse back into a number. In practice the column is a BIGSERIAL,
// so this should be unreachable, but ListWorkflowEvents used to ignore
// parseInt's error and build its pagination cursor from whatever "last" last
// held anyway: a caller that keeps ignoring it would silently pin the cursor
// to a stale or wrong value and could hand a client the same page forever.
// The real id is logged server-side for investigation; the client sees a
// distinct, opaque message rather than a fabricated cursor.
func eventIDError(id string, err error) error {
	slog.Error("workflow event id is not numeric", "event_id", id, "error", err)
	return status.Error(codes.Internal, "workflow event id is invalid")
}

func (s *Core) requireActor(ctx context.Context, mutation bool) (actor, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get(sessionHeader)) != 1 || strings.TrimSpace(md.Get(sessionHeader)[0]) == "" {
		return actor{}, status.Error(codes.Unauthenticated, "session is required")
	}
	row, err := s.queries.GetSessionActor(ctx, secrethash.HashSecret(md.Get(sessionHeader)[0]))
	if errors.Is(err, pgx.ErrNoRows) {
		return actor{}, status.Error(codes.Unauthenticated, "session is invalid")
	}
	if err != nil {
		return actor{}, databaseError(err)
	}
	id, role, csrfHash := row.ID, row.Role, row.CsrfTokenHash
	if mutation {
		csrf := md.Get(csrfHeader)
		if len(csrf) != 1 || subtle.ConstantTimeCompare([]byte(secrethash.HashSecret(csrf[0])), []byte(csrfHash)) != 1 {
			return actor{}, status.Error(codes.PermissionDenied, "CSRF token is invalid")
		}
		if role != "admin" {
			return actor{}, status.Error(codes.PermissionDenied, "administrator access is required")
		}
	}
	return actor{id: id, role: role}, nil
}

func (s *Core) requireMutation(ctx context.Context) (actor, error) {
	return s.requireActor(ctx, true)
}

func (s *Core) authenticateRunner(ctx context.Context, runnerID, credential string) error {
	if !idgen.ValidID(runnerID) {
		return status.Error(codes.Unauthenticated, "runner authentication was rejected")
	}
	stored, err := s.queries.GetRunnerCredentialHash(ctx, runnerID)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(secrethash.HashSecret(credential))) != 1 {
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
	ListProjectTaskSources(context.Context, string) ([]db.ListProjectTaskSourcesRow, error)
	ListProjectCredentials(context.Context, string) ([]db.ListProjectCredentialsRow, error)
}

func (s *Core) project(ctx context.Context, queries projectQuerier, id string) (*controlv1.Project, error) {
	if !idgen.ValidID(id) {
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
		RepositoryUrl:       textutil.TextValue(row.RepositoryUrl),
		LocalRepositoryPath: textutil.TextValue(row.LocalRepositoryPath),
	}
	var config projectConfig
	if err := json.Unmarshal([]byte(row.Configuration), &config); err != nil {
		return nil, configurationError(err)
	}
	project.RequiredRunnerLabels, project.ExecutionImage = config.Labels, config.ExecutionImage
	project.ExecutionTimeoutSeconds = config.ExecutionTimeoutSeconds
	project.RequireHumanApproval = config.RequireHumanApproval
	project.RequirePlanning = config.RequirePlanning
	steps, err := queries.ListProjectPipelineSteps(ctx, id)
	if err != nil {
		return nil, databaseError(err)
	}
	for _, step := range steps {
		project.PipelineSteps = append(project.PipelineSteps, &controlv1.PipelineStep{
			Command: step.Command, TimeoutSeconds: step.TimeoutSeconds, Position: step.Position, Required: step.Required,
		})
	}
	// TaskSources: the minimal read surface #293 adds for what used to be the
	// single, invisible app.projects.issue_tracker_type column. Creating,
	// editing and deleting a source needs #294's field-level descriptor for a
	// real write API rather than one this issue would have to hand-roll and
	// #294 would then redesign around; see the PR description. Every project
	// still has at least the source migration 026 auto-created for it, so
	// this is never misleadingly empty for an existing project.
	sources, err := queries.ListProjectTaskSources(ctx, id)
	if err != nil {
		return nil, databaseError(err)
	}
	for _, source := range sources {
		// taskSourceProto (tasksources_rpc.go) is the one place that decides
		// what a TaskSource's `secrets` field reports: every read of a
		// project's sources, not just the CRUD RPCs #294 adds, must show
		// "configured/not configured" rather than ever a value.
		taskSource, err := taskSourceProto(ctx, queries, source.ID, source.ProjectID, source.Provider, source.Name, source.Enabled, source.Configuration)
		if err != nil {
			return nil, err
		}
		project.TaskSources = append(project.TaskSources, taskSource)
	}
	return project, nil
}

func (s *Core) workflow(ctx context.Context, id string) (*controlv1.Workflow, error) {
	if !idgen.ValidID(id) {
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
		BranchName:             textutil.TextValue(row.BranchName),
		PullRequestExternalId:  textutil.CoalesceText(row.PrExternalID, row.RunPullRequestExternalID),
		PullRequestUrl:         textutil.CoalesceText(row.PrUrl, row.RunPullRequestUrl),
		PullRequestState:       textutil.TextValue(row.PullRequestState),
		BlockingReason:         textutil.TextValue(row.BlockingReason),
		PlanningAttempts:       row.PlanningAttempts,
		ImplementationAttempts: row.ImplementationAttempts,
		PipelineRepairAttempts: row.PipelineRepairAttempts,
		CiRepairAttempts:       row.CiRepairAttempts,
		ReviewCycles:           row.ReviewCycles,
		TotalAgentExecutions:   row.TotalAgentExecutions,
		PlanSummary:            textutil.TextValue(row.PlanSummary),
	}
	workflow.CreatedAt, workflow.UpdatedAt = textutil.Timestamp(row.CreatedAt.Time), textutil.Timestamp(row.UpdatedAt.Time)
	return workflow
}

func (s *Core) controlWorkflow(ctx context.Context, id, reason, action string) (*controlv1.Workflow, error) {
	actor, err := s.requireMutation(ctx)
	if err != nil {
		return nil, err
	}
	if !idgen.ValidID(id) || len(reason) > 1024 || (action == "block" && strings.TrimSpace(reason) == "") {
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
		// Retry supersedes this run rather than reviving it. A job is unique
		// per workflow run (app.jobs.workflow_run_id is UNIQUE), so the run
		// that already had its execution cannot be given another one; the
		// scheduler picks the issue up again -- its own eligible bit is
		// untouched, see #268 -- and creates a fresh run whose history
		// stands alongside this one. Superseding is what stops this run's
		// own failed/blocked/completed status from continuing to exclude
		// the issue via the app.workflow_runs join ListQueueEntries and
		// ClaimSchedulableIssue both run. The previous implementation took
		// the project lock and parked the run in a "recovering" status
		// nothing reads, which left the project unable to schedule anything
		// again.
		if err := queries.DeleteProjectLock(ctx, db.DeleteProjectLockParams{ProjectID: projectID, WorkflowRunID: id}); err != nil {
			return nil, databaseError(err)
		}
		if err := queries.SupersedeWorkflowRun(ctx, id); err != nil {
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
			// Blocking excludes the issue from scheduling; cancelling does not.
			// Blocking says "stop working on this until a human says
			// otherwise", and this run's own 'blocked' status is what the
			// scheduler's app.workflow_runs join (ListQueueEntries,
			// ClaimSchedulableIssue) excludes on -- without it the scheduler
			// re-created the run from the still-eligible issue on the next
			// one-second tick, the operator's stop button restarting the very
			// work it stopped. Cancelling lands on 'cancelled' instead, which
			// that join does not exclude, so the issue returns to the queue by
			// design with no operator action needed. See #268.
			//
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
		// A job cancelled mid-planning (#351) is still running its
		// planner-role execution, not the developer one -- current (read
		// before this function's own switch above) is this run's status at
		// the moment cancellation was requested, and StatusPlanning is the
		// only status a job's execution ID differs under.
		executionID := implementExecutionID(jobToCancel)
		if Status(current) == StatusPlanning {
			executionID = planExecutionID(jobToCancel)
		}
		s.enqueue(runnerHoldingLease, &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Cancel{Cancel: &runnerv1.CancelExecution{
			ExecutionId:     executionID,
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
			ID: idgen.NewID(), ProjectID: projectID, Position: step.GetPosition(), Name: step.GetCommand(),
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
	// 0 means "use the fixed default" (see executionTimeoutSeconds); anything
	// else must be a real, positive duration inside the runner task packet's
	// own ceiling (taskpacket.go's 86400-second ProtocolVersion 1.0 bound), so a
	// project cannot configure a value the runner would refuse.
	if cfg.GetExecutionTimeoutSeconds() < 0 || cfg.GetExecutionTimeoutSeconds() > 86400 {
		return nil, nil, status.Error(codes.InvalidArgument, "project execution timeout is invalid")
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
		Name:                    strings.TrimSpace(cfg.GetName()),
		RepositoryMode:          cfg.GetRepositoryMode(),
		RepositoryUrl:           cfg.GetRepositoryUrl(),
		LocalRepositoryPath:     cfg.GetLocalRepositoryPath(),
		DefaultBranch:           cfg.GetDefaultBranch(),
		RequiredRunnerLabels:    labels,
		PipelineSteps:           cfg.GetPipelineSteps(),
		ExecutionImage:          cfg.GetExecutionImage(),
		ExecutionTimeoutSeconds: cfg.GetExecutionTimeoutSeconds(),
		RequireHumanApproval:    cfg.GetRequireHumanApproval(),
		RequirePlanning:         cfg.GetRequirePlanning(),
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
func (s *Core) runner(ctx context.Context, id string) (*controlv1.Runner, error) {
	if !idgen.ValidID(id) {
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
		runner.LastSeenAt = textutil.Timestamp(seen.Time)
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

// jsonLabels marshals a runner/token label set for a jsonb column. The input
// is always a caller-controlled []string, so marshaling is infallible in
// practice -- but a discarded error here used to turn a marshal failure into
// an empty string silently fed to a jsonb containment check deciding runner
// registration authorization, so the error is surfaced rather than swallowed.
func jsonLabels(labels []string) (string, error) {
	encoded, err := json.Marshal(labels)
	if err != nil {
		return "", err
	}
	return string(encoded), nil
}

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

func defaultReason(action, reason string) string {
	if strings.TrimSpace(reason) == "" {
		return "operator " + action
	}
	return strings.TrimSpace(reason)
}
