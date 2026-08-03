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
	"os"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
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
	return &Server{pool: pool, queries: db.New(pool), version: version, github: github, sessions: make(map[string]chan *runnerv1.OrchestratorToRunner)}, nil
}

func (s *Server) Bootstrap(ctx context.Context) error {
	var userCount int
	if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM app.users`).Scan(&userCount); err != nil {
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
			if _, err := s.pool.Exec(ctx, `INSERT INTO app.users(id,username,password_hash,role) VALUES($1,$2,$3,'admin') ON CONFLICT(username) DO NOTHING`, newID(), username, hash); err != nil {
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
	_, err = s.pool.Exec(ctx, `INSERT INTO app.runner_registration_tokens(id,token_hash,allowed_labels,expires_at) VALUES($1,$2,$3::jsonb,now()+interval '15 minutes') ON CONFLICT(token_hash) DO UPDATE SET allowed_labels=EXCLUDED.allowed_labels,expires_at=EXCLUDED.expires_at WHERE app.runner_registration_tokens.used_at IS NULL`, newID(), hashSecret(token), jsonLabels(labels))
	return databaseError(err)
}

func (s *Server) Login(ctx context.Context, request *controlv1.LoginRequest) (*controlv1.LoginResponse, error) {
	username := strings.TrimSpace(request.GetUsername())
	if username == "" || len(username) > 128 || request.GetPassword() == "" {
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	var userID, encoded string
	var enabled bool
	err := s.pool.QueryRow(ctx, `SELECT id::text,password_hash,enabled FROM app.users WHERE username=$1`, username).Scan(&userID, &encoded, &enabled)
	if errors.Is(err, pgx.ErrNoRows) {
		_, _ = passwordMatches(request.GetPassword(), "scrypt$16384$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	matches, err := passwordMatches(request.GetPassword(), encoded)
	if err != nil || !enabled || !matches {
		return nil, status.Error(codes.Unauthenticated, "login was rejected")
	}
	sessionToken, csrfToken := randomSecret(), randomSecret()
	expiresAt := time.Now().UTC().Add(7 * 24 * time.Hour)
	if _, err := s.pool.Exec(ctx, `INSERT INTO app.user_sessions(id,user_id,token_hash,csrf_token_hash,expires_at,last_seen_at) VALUES($1,$2,$3,$4,$5,now())`, newID(), userID, hashSecret(sessionToken), hashSecret(csrfToken), expiresAt); err != nil {
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
	err = s.pool.QueryRow(ctx, `SELECT username,email,display_name FROM app.users WHERE id=$1`, current.id).Scan(&response.Username, &response.Email, &response.DisplayName)
	if err != nil {
		return nil, databaseError(err)
	}
	return response, nil
}

func (s *Server) Logout(ctx context.Context, _ *controlv1.LogoutRequest) (*controlv1.LogoutResponse, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get(sessionHeader)) != 1 || len(md.Get(csrfHeader)) != 1 {
		return nil, status.Error(codes.Unauthenticated, "session is required")
	}
	command, err := s.pool.Exec(ctx, `UPDATE app.user_sessions SET revoked_at=now() WHERE token_hash=$1 AND csrf_token_hash=$2 AND revoked_at IS NULL AND expires_at>now()`, hashSecret(md.Get(sessionHeader)[0]), hashSecret(md.Get(csrfHeader)[0]))
	if err != nil {
		return nil, databaseError(err)
	}
	if command.RowsAffected() != 1 {
		return nil, status.Error(codes.Unauthenticated, "session is invalid")
	}
	return &controlv1.LogoutResponse{}, nil
}

func (s *Server) ListProjects(ctx context.Context, _ *controlv1.ListProjectsRequest) (*controlv1.ListProjectsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text FROM app.projects ORDER BY name, id`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.ListProjectsResponse{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, databaseError(err)
		}
		project, err := s.project(ctx, s.pool, id)
		if err != nil {
			return nil, err
		}
		response.Projects = append(response.Projects, project)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	_, err = tx.Exec(ctx, `INSERT INTO app.projects (id, name, repository_mode, repository_url, local_repository_path, default_branch, configuration) VALUES ($1, $2, $3, NULLIF($4, ''), NULLIF($5, ''), $6, $7::jsonb)`, id, cfg.GetName(), cfg.GetRepositoryMode(), cfg.GetRepositoryUrl(), cfg.GetLocalRepositoryPath(), cfg.GetDefaultBranch(), encoded)
	if err != nil {
		return nil, databaseError(err)
	}
	if err := replacePipelineSteps(ctx, tx, id, steps); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, actor.id, "project.create", "project", id); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	project, err := s.project(ctx, s.pool, id)
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
	command, err := tx.Exec(ctx, `UPDATE app.projects SET name=$2, repository_mode=$3, repository_url=NULLIF($4, ''), local_repository_path=NULLIF($5, ''), default_branch=$6, configuration=$7::jsonb, updated_at=now() WHERE id=$1`, request.GetProjectId(), cfg.GetName(), cfg.GetRepositoryMode(), cfg.GetRepositoryUrl(), cfg.GetLocalRepositoryPath(), cfg.GetDefaultBranch(), encoded)
	if err != nil {
		return nil, databaseError(err)
	}
	if command.RowsAffected() != 1 {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	if err := replacePipelineSteps(ctx, tx, request.GetProjectId(), steps); err != nil {
		return nil, err
	}
	if err := audit(ctx, tx, actor.id, "project.update", "project", request.GetProjectId()); err != nil {
		return nil, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, databaseError(err)
	}
	project, err := s.project(ctx, s.pool, request.GetProjectId())
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
	command, err := s.pool.Exec(ctx, `UPDATE app.projects SET enabled=$2, updated_at=now() WHERE id=$1`, request.GetProjectId(), request.GetEnabled())
	if err != nil {
		return nil, databaseError(err)
	}
	if command.RowsAffected() != 1 {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	if err := audit(ctx, s.pool, actor.id, "project.enabled", "project", request.GetProjectId()); err != nil {
		return nil, err
	}
	project, err := s.project(ctx, s.pool, request.GetProjectId())
	if err != nil {
		return nil, err
	}
	return &controlv1.SetProjectEnabledResponse{Project: project}, nil
}

func (s *Server) ListWorkflows(ctx context.Context, _ *controlv1.ListWorkflowsRequest) (*controlv1.ListWorkflowsResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT wr.id::text FROM app.workflow_runs wr ORDER BY wr.created_at DESC, wr.id`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.ListWorkflowsResponse{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, databaseError(err)
		}
		workflow, err := s.workflow(ctx, id)
		if err != nil {
			return nil, err
		}
		response.Workflows = append(response.Workflows, workflow)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	rows, err := s.pool.Query(ctx, `SELECT id, event_type, created_at, payload::text FROM app.workflow_events WHERE workflow_run_id=$1 AND id>$2 ORDER BY id LIMIT $3`, request.GetWorkflowRunId(), request.GetAfterId(), limit)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.ListWorkflowEventsResponse{}
	var last int64
	for rows.Next() {
		var event controlv1.WorkflowEvent
		var created time.Time
		if err := rows.Scan(&event.Id, &event.EventType, &created, &event.PayloadJson); err != nil {
			return nil, databaseError(err)
		}
		event.CreatedAt = timestamp(created)
		response.Events = append(response.Events, &event)
		last, _ = parseInt(event.Id)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	query := `SELECT id::text,repository_url FROM app.projects WHERE enabled`
	args := []any{}
	if projectID != "" {
		query += ` AND id=$1`
		args = append(args, projectID)
	}
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.SyncNowResponse{}
	for rows.Next() {
		var id string
		var repositoryURL *string
		if err := rows.Scan(&id, &repositoryURL); err != nil {
			return nil, databaseError(err)
		}
		result := &controlv1.ProjectSyncResult{ProjectId: id}
		if err := s.syncProject(ctx, id, stringValue(repositoryURL)); err != nil {
			result.Error = err.Error()
		} else {
			var count int32
			if err := s.pool.QueryRow(ctx, `SELECT COUNT(*) FROM app.issues WHERE project_id=$1`, id).Scan(&count); err != nil {
				return nil, databaseError(err)
			}
			result.SyncedIssues = count
		}
		response.Results = append(response.Results, result)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	rows, err := s.pool.Query(ctx, `SELECT id::text,repository_url FROM app.projects WHERE enabled`)
	if err != nil {
		return databaseError(err)
	}
	type project struct{ id, repositoryURL string }
	var projects []project
	for rows.Next() {
		var id string
		var repositoryURL *string
		if err := rows.Scan(&id, &repositoryURL); err != nil {
			rows.Close()
			return databaseError(err)
		}
		projects = append(projects, project{id: id, repositoryURL: stringValue(repositoryURL)})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return databaseError(err)
	}
	var failures []error
	for _, candidate := range projects {
		if err := s.syncProject(ctx, candidate.id, candidate.repositoryURL); err != nil {
			failures = append(failures, fmt.Errorf("project %s: %w", candidate.id, err))
		}
	}
	return errors.Join(failures...)
}

func (s *Server) IssueSyncStatus(ctx context.Context, _ *controlv1.IssueSyncStatusRequest) (*controlv1.IssueSyncStatusResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT p.id::text,p.name,p.enabled,COUNT(i.id),COUNT(i.id) FILTER(WHERE i.eligible),s.last_synced_at,s.consecutive_failures,s.next_retry_at,s.last_error FROM app.projects p LEFT JOIN app.issues i ON i.project_id=p.id LEFT JOIN app.issue_sync_state s ON s.project_id=p.id GROUP BY p.id,p.name,p.enabled,s.last_synced_at,s.consecutive_failures,s.next_retry_at,s.last_error ORDER BY p.name,p.id`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.IssueSyncStatusResponse{}
	for rows.Next() {
		entry := &controlv1.IssueSyncStatusEntry{}
		var syncedAt, retryAt *time.Time
		var failures *int32
		var lastError *string
		if err := rows.Scan(&entry.ProjectId, &entry.ProjectName, &entry.Enabled, &entry.IssueCount, &entry.EligibleCount, &syncedAt, &failures, &retryAt, &lastError); err != nil {
			return nil, databaseError(err)
		}
		if syncedAt != nil {
			entry.LastSyncedAt = timestamp(*syncedAt)
		}
		if failures != nil {
			entry.ConsecutiveFailures = *failures
		}
		if retryAt != nil {
			entry.NextRetryAt = timestamp(*retryAt)
		}
		if lastError != nil {
			entry.LastError = *lastError
		}
		response.Entries = append(response.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	// Eligibility is label-driven only until an issue has been worked on. From
	// its first workflow run onwards the orchestrator owns the flag — a run that
	// ends without delivering parks the issue, and only a manual retry reopens
	// it — so a sync pass must not hand eligibility back to the label and
	// restart work the operator has not asked for again.
	for _, issue := range issues {
		labels, _ := json.Marshal(issue.Labels)
		raw, _ := json.Marshal(issue)
		_, err := tx.Exec(ctx, `INSERT INTO app.issues(id,project_id,provider,external_id,display_number,title,body,url,state,labels,priority,eligible,external_created_at,external_updated_at,last_synced_at,raw_snapshot) VALUES($1,$2,'github',$3,$3,$4,$5,$6,'open',$7::jsonb,$8,$9,$10,$11,now(),$12::jsonb) ON CONFLICT(project_id,provider,external_id) DO UPDATE SET display_number=EXCLUDED.display_number,title=EXCLUDED.title,body=EXCLUDED.body,url=EXCLUDED.url,state='open',labels=EXCLUDED.labels,priority=EXCLUDED.priority,eligible=CASE WHEN EXISTS(SELECT 1 FROM app.workflow_runs w WHERE w.issue_id=app.issues.id) THEN app.issues.eligible ELSE EXCLUDED.eligible END,external_created_at=EXCLUDED.external_created_at,external_updated_at=EXCLUDED.external_updated_at,last_synced_at=now(),raw_snapshot=EXCLUDED.raw_snapshot`, newID(), projectID, issue.ExternalID, issue.Title, issue.Body, issue.URL, string(labels), issue.Priority, issue.Eligible, issue.CreatedAt, issue.UpdatedAt, string(raw))
		if err != nil {
			return databaseError(err)
		}
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.issue_sync_state(project_id,consecutive_failures,last_error,last_synced_at,updated_at) VALUES($1,0,NULL,now(),now()) ON CONFLICT(project_id) DO UPDATE SET consecutive_failures=0,last_error=NULL,last_synced_at=now(),updated_at=now()`, projectID); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) recordSyncFailure(ctx context.Context, projectID string, cause error) error {
	_, err := s.pool.Exec(ctx, `INSERT INTO app.issue_sync_state(project_id,consecutive_failures,last_error,updated_at) VALUES($1,1,$2,now()) ON CONFLICT(project_id) DO UPDATE SET consecutive_failures=app.issue_sync_state.consecutive_failures+1,last_error=EXCLUDED.last_error,updated_at=now()`, projectID, truncate(cause.Error(), 1024))
	return databaseError(err)
}

func (s *Server) ListRunners(ctx context.Context, _ *controlv1.ListRunnersRequest) (*controlv1.ListRunnersResponse, error) {
	if _, err := s.requireActor(ctx, false); err != nil {
		return nil, err
	}
	rows, err := s.pool.Query(ctx, `SELECT id::text, name, enabled, draining, status, labels::text, last_seen_at, version FROM app.runners ORDER BY name, id`)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.ListRunnersResponse{}
	for rows.Next() {
		runner, err := scanRunner(rows)
		if err != nil {
			return nil, err
		}
		response.Runners = append(response.Runners, runner)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	var query string
	switch request.GetState() {
	case "drain":
		query = `UPDATE app.runners SET draining=true WHERE id=$1 AND revoked_at IS NULL`
	case "enable":
		query = `UPDATE app.runners SET enabled=true, draining=false WHERE id=$1 AND revoked_at IS NULL`
	case "revoke":
		query = `UPDATE app.runners SET enabled=false, status='offline', revoked_at=now() WHERE id=$1 AND revoked_at IS NULL`
	default:
		return nil, status.Error(codes.InvalidArgument, "runner state is invalid")
	}
	command, err := s.pool.Exec(ctx, query, request.GetRunnerId())
	if err != nil {
		return nil, databaseError(err)
	}
	if command.RowsAffected() != 1 {
		return nil, status.Error(codes.NotFound, "runner is unknown")
	}
	if request.GetState() == "revoke" {
		if _, err := s.pool.Exec(ctx, `UPDATE app.runner_credentials SET revoked_at=now() WHERE runner_id=$1 AND revoked_at IS NULL`, request.GetRunnerId()); err != nil {
			return nil, databaseError(err)
		}
	}
	if err := audit(ctx, s.pool, actor.id, "runner."+request.GetState(), "runner", request.GetRunnerId()); err != nil {
		return nil, err
	}
	if request.GetState() == "drain" || request.GetState() == "revoke" {
		// Best-effort, same as workflow cancellation above: a runner with no
		// active session is already not receiving new offers, which is the
		// only thing draining/revoking otherwise guarantees, so a missed
		// delivery here is not a correctness problem — just a slower stop.
		s.enqueue(request.GetRunnerId(), &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Drain{Drain: &runnerv1.DrainRunner{}}})
	}
	var runner controlv1.Runner
	row := s.pool.QueryRow(ctx, `SELECT id::text, name, enabled, draining, status, labels::text, last_seen_at, version FROM app.runners WHERE id=$1`, request.GetRunnerId())
	if err := scanRunnerRow(row, &runner); err != nil {
		return nil, err
	}
	return &controlv1.SetRunnerStateResponse{Runner: &runner}, nil
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
	rows, err := s.pool.Query(ctx, `SELECT p.id::text, p.name, i.external_id, i.title, i.priority, CASE WHEN NOT p.enabled THEN 'project_disabled' WHEN EXISTS (SELECT 1 FROM app.project_locks l WHERE l.project_id=p.id) THEN 'project_locked' ELSE '' END FROM app.issues i JOIN app.projects p ON p.id=i.project_id WHERE i.eligible AND i.state='open' ORDER BY i.priority DESC, i.external_created_at, i.last_synced_at, i.project_id, i.external_id LIMIT $1`, limit)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	response := &controlv1.ListQueueResponse{}
	for rows.Next() {
		entry := &controlv1.QueueEntry{}
		if err := rows.Scan(&entry.ProjectId, &entry.ProjectName, &entry.ExternalId, &entry.Title, &entry.Priority, &entry.BlockedReason); err != nil {
			return nil, databaseError(err)
		}
		response.Entries = append(response.Entries, entry)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
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
	var snapshot schedulerSnapshot
	err := s.pool.QueryRow(ctx, `SELECT (SELECT COUNT(*) FROM app.issues i JOIN app.projects p ON p.id=i.project_id WHERE p.enabled AND i.eligible AND i.state='open'), (SELECT COUNT(*) FROM app.workflow_runs WHERE status NOT IN (`+terminalStatusList+`)), (SELECT COUNT(*) FROM app.jobs WHERE status IN ('offered','preparing','running')), (SELECT COUNT(*) FROM app.runners WHERE enabled AND revoked_at IS NULL), (SELECT EXTRACT(EPOCH FROM now()-MIN(COALESCE(last_seen_at,registered_at)))::double precision FROM app.runners WHERE enabled AND revoked_at IS NULL)`).
		Scan(&snapshot.queueDepth, &snapshot.activeWorkflows, &snapshot.scheduledJobs, &snapshot.enabledRunners, &snapshot.oldestHeartbeatAge)
	if err != nil {
		return schedulerSnapshot{}, databaseError(err)
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
	var tokenID string
	err = tx.QueryRow(ctx, `SELECT id::text FROM app.runner_registration_tokens WHERE token_hash=$1 AND used_at IS NULL AND revoked_at IS NULL AND expires_at>now() AND allowed_labels @> $2::jsonb FOR UPDATE`, hashSecret(request.GetToken()), jsonLabels(labels)).Scan(&tokenID)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.PermissionDenied, "runner registration was rejected")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	runnerID, credential := newID(), randomSecret()
	if _, err := tx.Exec(ctx, `INSERT INTO app.runners (id, name, status, version, labels, capacity, last_seen_at) VALUES ($1,$2,'offline','',$3::jsonb,$4,now())`, runnerID, strings.TrimSpace(request.GetName()), jsonLabels(labels), capacity); err != nil {
		return nil, databaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.runner_credentials (id, runner_id, credential_hash) VALUES ($1,$2,$3)`, newID(), runnerID, hashSecret(credential)); err != nil {
		return nil, databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.runner_registration_tokens SET used_at=now() WHERE id=$1`, tokenID); err != nil {
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
		_, err := s.pool.Exec(ctx, `UPDATE app.runners SET status='online', last_seen_at=now(), version=COALESCE(NULLIF($2,''), version) WHERE id=$1 AND enabled AND revoked_at IS NULL`, runnerID, truncate(heartbeat.GetVersion(), 12))
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
		_, err := s.pool.Exec(ctx, `UPDATE app.runners SET draining=$2, last_seen_at=now() WHERE id=$1`, runnerID, message.GetRunnerDraining().GetDraining())
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
	var issueID, externalID, title, body, projectID, mode, defaultBranch, runnerID string
	var repositoryURL, localPath *string
	var configuration []byte
	err = tx.QueryRow(ctx, `SELECT i.id::text,i.external_id,i.title,i.body,p.id::text,p.repository_mode,p.repository_url,p.local_repository_path,p.default_branch,p.configuration::text,r.id::text FROM app.issues i JOIN app.projects p ON p.id=i.project_id JOIN app.runners r ON r.status='online' AND r.enabled AND NOT r.draining AND r.revoked_at IS NULL WHERE i.eligible AND i.state='open' AND p.enabled AND r.id::text=ANY($1) AND r.labels @> COALESCE(p.configuration->'required_runner_labels','[]'::jsonb) AND NOT EXISTS(SELECT 1 FROM app.project_locks l WHERE l.project_id=p.id) AND NOT EXISTS(SELECT 1 FROM app.jobs j WHERE j.runner_id=r.id AND j.status IN ('offered','preparing','running')) ORDER BY i.priority DESC,i.external_created_at,i.last_synced_at,i.project_id,i.external_id,r.id FOR UPDATE OF i,r SKIP LOCKED LIMIT 1`, runners).Scan(&issueID, &externalID, &title, &body, &projectID, &mode, &repositoryURL, &localPath, &defaultBranch, &configuration, &runnerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, databaseError(err)
	}
	workflowID, jobID, offerID := newID(), newID(), newID()
	branch := "agent/" + workflowID
	if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_runs(id,project_id,issue_id,thread_id,status,current_phase,branch_name) VALUES($1,$2,$3,$4,`+qOffered+`,`+qOffered+`,$5)`, workflowID, projectID, issueID, workflowID, branch); err != nil {
		return false, databaseError(err)
	}
	lock, err := tx.Exec(ctx, `INSERT INTO app.project_locks(project_id,workflow_run_id) VALUES($1,$2) ON CONFLICT(project_id) DO NOTHING`, projectID, workflowID)
	if err != nil {
		return false, databaseError(err)
	}
	if lock.RowsAffected() != 1 {
		return false, nil
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.jobs(id,workflow_run_id,project_id,runner_id,status,lease_generation,offered_at) VALUES($1,$2,$3,$4,'offered',1,now())`, jobID, workflowID, projectID, runnerID); err != nil {
		return false, databaseError(err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app.job_offers(id,job_id,runner_id,status,expires_at) VALUES($1,$2,$3,'offered','infinity')`, offerID, jobID, runnerID); err != nil {
		return false, databaseError(err)
	}
	var config projectConfig
	if err := json.Unmarshal(configuration, &config); err != nil {
		return false, databaseError(err)
	}
	packet, err := developerPacket(jobID, projectID, externalID, title, body, mode, stringValue(repositoryURL), stringValue(localPath), defaultBranch, branch, config.ExecutionImage)
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
	const reason = "runner disconnected before offer delivery"
	if _, err := tx.Exec(ctx, `UPDATE app.jobs SET status='cancelled',finished_at=now(),recovery_reason=$2 WHERE id=$1 AND status='offered'`, jobID, reason); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.job_offers SET status='cancelled',responded_at=now() WHERE job_id=$1 AND status='offered'`, jobID); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status=`+qCancelled+`,current_phase=`+qCancelled+`,completed_at=now(),terminal_reason=$2 WHERE id=$1`, workflowID, reason); err != nil {
		return databaseError(err)
	}
	// The issue stays eligible on purpose: no execution ran, so nothing was
	// spent and the next scheduling pass should offer this work again.
	if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, projectID, workflowID); err != nil {
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
	var offerID string
	err = tx.QueryRow(ctx, `UPDATE app.job_offers SET status='accepted', responded_at=now() WHERE job_id=$1 AND runner_id=$2 AND status='offered' AND expires_at>now() RETURNING id::text`, jobID, runnerID).Scan(&offerID)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, status.Error(codes.FailedPrecondition, "job offer is no longer active")
	}
	if err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	var generation int64
	var expiresAt time.Time
	// `AND status='offered'` is load-bearing, not redundant with the offer row
	// above. Cancelling or blocking a workflow cancels the job but leaves the
	// offer row alone, so without this guard a late OfferAccepted drags an
	// administratively cancelled job back to 'preparing' and hands the runner a
	// fresh lease — which is the credential the runner presents to
	// ResolveJobSecret to read the project's tokens.
	err = tx.QueryRow(ctx, `UPDATE app.jobs SET status='preparing', accepted_at=now(), lease_expires_at=now()+$3::interval WHERE id=$1 AND runner_id=$2 AND status='offered' RETURNING lease_generation, lease_expires_at`, jobID, runnerID, durationInterval(leaseDuration)).Scan(&generation, &expiresAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, time.Time{}, status.Error(codes.FailedPrecondition, "job offer is no longer active")
	}
	if err != nil {
		return 0, time.Time{}, databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status=`+qPreparing+`, updated_at=now() WHERE id=(SELECT workflow_run_id FROM app.jobs WHERE id=$1) AND status NOT IN (`+terminalStatusList+`)`, jobID); err != nil {
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
	var workflowID, projectID string
	err = tx.QueryRow(ctx, `SELECT j.workflow_run_id::text, j.project_id::text FROM app.jobs j JOIN app.job_offers o ON o.job_id=j.id WHERE j.id=$1 AND o.runner_id=$2 AND o.status='offered' AND o.expires_at>now() FOR UPDATE`, jobID, runnerID).Scan(&workflowID, &projectID)
	if errors.Is(err, pgx.ErrNoRows) {
		return status.Error(codes.FailedPrecondition, "job offer is no longer active")
	}
	if err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.job_offers SET status='rejected', responded_at=now() WHERE job_id=$1 AND runner_id=$2`, jobID, runnerID); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.jobs SET status='cancelled', finished_at=now(), recovery_reason=NULLIF($2,'') WHERE id=$1`, jobID, truncate(reason, 1024)); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status=`+qCancelled+`, current_phase=`+qCancelled+`, terminal_reason='runner rejected offer', completed_at=now(), updated_at=now() WHERE id=$1`, workflowID); err != nil {
		return databaseError(err)
	}
	if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, projectID, workflowID); err != nil {
		return databaseError(err)
	}
	return commit(tx)
}

func (s *Server) renewLease(ctx context.Context, runnerID string, renewal *runnerv1.LeaseRenewal) (time.Time, error) {
	if !validID(renewal.GetJobId()) || renewal.GetLeaseGeneration() < 1 || renewal.GetRequestedExpiresAtUnixMs() <= time.Now().UnixMilli() {
		return time.Time{}, status.Error(codes.InvalidArgument, "lease renewal is invalid")
	}
	expiresAt := time.UnixMilli(renewal.GetRequestedExpiresAtUnixMs()).UTC()
	var persisted time.Time
	err := s.pool.QueryRow(ctx, `UPDATE app.jobs SET lease_expires_at=$4 WHERE id=$1 AND runner_id=$2 AND lease_generation=$3 AND status IN ('preparing','running') AND lease_expires_at>now() RETURNING lease_expires_at`, renewal.GetJobId(), runnerID, renewal.GetLeaseGeneration(), expiresAt).Scan(&persisted)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, status.Error(codes.FailedPrecondition, "runner does not hold this job")
	}
	if err != nil {
		return time.Time{}, databaseError(err)
	}
	return persisted, nil
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
	var workflowID string
	err = tx.QueryRow(ctx, `UPDATE app.jobs SET last_event_sequence=$4, status=CASE WHEN $5='started' THEN 'running' WHEN $5 IN ('completed','failed','cancelled') THEN $5 ELSE status END, started_at=CASE WHEN $5='started' THEN COALESCE(started_at,now()) ELSE started_at END, finished_at=CASE WHEN $5 IN ('completed','failed','cancelled') THEN now() ELSE finished_at END WHERE id=$1 AND runner_id=$2 AND lease_generation=$3 AND status IN ('preparing','running') AND lease_expires_at>now() AND last_event_sequence<$4 RETURNING workflow_run_id::text`, event.GetJobId(), runnerID, event.GetLeaseGeneration(), event.GetEventSequence(), event.GetType()).Scan(&workflowID)
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
	if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_events (workflow_run_id,event_type,severity,payload) VALUES ($1,$2,$3,$4::jsonb)`, workflowID, event.GetType(), eventSeverity(event.GetType()), event.GetPayloadJson()); err != nil {
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
		if _, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status=$2,current_phase=$2,blocking_reason=COALESCE(NULLIF($3,''),blocking_reason),terminal_reason=COALESCE(NULLIF($3,''),terminal_reason),updated_at=now(),completed_at=CASE WHEN $2 IN (`+terminalStatusList+`) THEN now() ELSE completed_at END WHERE id=$1`, workflowID, runStatus.String(), blockingReason); err != nil {
			return databaseError(err)
		}
		if event.GetType() != "completed" {
			if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE workflow_run_id=$1`, workflowID); err != nil {
				return databaseError(err)
			}
			// An execution that actually ran and did not succeed parks its
			// issue. V1 has no automatic retry, so leaving the issue eligible
			// would have the scheduler dispatch the same work again on the very
			// next tick, forever. RetryWorkflow is what makes it eligible again.
			if err := parkIssue(ctx, tx, workflowID); err != nil {
				return err
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

func databaseError(err error) error {
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unknown {
		return err
	}
	return status.Error(codes.Internal, "database operation failed")
}

func (s *Server) requireActor(ctx context.Context, mutation bool) (actor, error) {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok || len(md.Get(sessionHeader)) != 1 || strings.TrimSpace(md.Get(sessionHeader)[0]) == "" {
		return actor{}, status.Error(codes.Unauthenticated, "session is required")
	}
	var id, role, csrfHash string
	err := s.pool.QueryRow(ctx, `SELECT u.id::text, u.role, us.csrf_token_hash FROM app.user_sessions us JOIN app.users u ON u.id=us.user_id WHERE us.token_hash=$1 AND us.revoked_at IS NULL AND us.expires_at>now() AND u.enabled`, hashSecret(md.Get(sessionHeader)[0])).Scan(&id, &role, &csrfHash)
	if errors.Is(err, pgx.ErrNoRows) {
		return actor{}, status.Error(codes.Unauthenticated, "session is invalid")
	}
	if err != nil {
		return actor{}, databaseError(err)
	}
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
	var stored string
	err := s.pool.QueryRow(ctx, `SELECT c.credential_hash FROM app.runners r JOIN app.runner_credentials c ON c.runner_id=r.id WHERE r.id=$1 AND r.enabled AND r.revoked_at IS NULL AND c.revoked_at IS NULL AND (c.expires_at IS NULL OR c.expires_at>now()) ORDER BY c.created_at DESC LIMIT 1`, runnerID).Scan(&stored)
	if err != nil || subtle.ConstantTimeCompare([]byte(stored), []byte(hashSecret(credential))) != 1 {
		return status.Error(codes.Unauthenticated, "runner authentication was rejected")
	}
	return nil
}

func (s *Server) project(ctx context.Context, db interface {
	QueryRow(context.Context, string, ...any) pgx.Row
	Query(context.Context, string, ...any) (pgx.Rows, error)
}, id string) (*controlv1.Project, error) {
	if !validID(id) {
		return nil, status.Error(codes.InvalidArgument, "project ID is invalid")
	}
	project := &controlv1.Project{}
	var repositoryURL, localPath *string
	var configJSON []byte
	err := db.QueryRow(ctx, `SELECT id::text,name,enabled,repository_mode,repository_url,local_repository_path,default_branch,configuration::text FROM app.projects WHERE id=$1`, id).Scan(&project.Id, &project.Name, &project.Enabled, &project.RepositoryMode, &repositoryURL, &localPath, &project.DefaultBranch, &configJSON)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	project.RepositoryUrl, project.LocalRepositoryPath = stringValue(repositoryURL), stringValue(localPath)
	var config projectConfig
	if err := json.Unmarshal(configJSON, &config); err != nil {
		return nil, databaseError(err)
	}
	project.RequiredRunnerLabels, project.ExecutionImage = config.Labels, config.ExecutionImage
	rows, err := db.Query(ctx, `SELECT command,timeout_seconds,position,required FROM app.project_pipeline_steps WHERE project_id=$1 ORDER BY position,id`, id)
	if err != nil {
		return nil, databaseError(err)
	}
	defer rows.Close()
	for rows.Next() {
		step := &controlv1.PipelineStep{}
		if err := rows.Scan(&step.Command, &step.TimeoutSeconds, &step.Position, &step.Required); err != nil {
			return nil, databaseError(err)
		}
		project.PipelineSteps = append(project.PipelineSteps, step)
	}
	if err := rows.Err(); err != nil {
		return nil, databaseError(err)
	}
	return project, nil
}

func (s *Server) workflow(ctx context.Context, id string) (*controlv1.Workflow, error) {
	if !validID(id) {
		return nil, status.Error(codes.InvalidArgument, "workflow run ID is invalid")
	}
	workflow := &controlv1.Workflow{}
	var branch, externalID, url, state, reason *string
	var created, updated time.Time
	err := s.pool.QueryRow(ctx, `SELECT wr.id::text,wr.project_id::text,wr.status,wr.current_phase,i.external_id,i.title,wr.branch_name,COALESCE(pr.external_id,wr.pull_request_external_id),COALESCE(pr.url,wr.pull_request_url),pr.state,wr.blocking_reason,wr.planning_attempts,wr.implementation_attempts,wr.pipeline_repair_attempts,wr.ci_repair_attempts,wr.review_cycles,wr.total_agent_executions,wr.created_at,wr.updated_at FROM app.workflow_runs wr JOIN app.issues i ON i.id=wr.issue_id LEFT JOIN app.pull_requests pr ON pr.workflow_run_id=wr.id WHERE wr.id=$1`, id).Scan(&workflow.Id, &workflow.ProjectId, &workflow.Status, &workflow.Phase, &workflow.IssueExternalId, &workflow.IssueTitle, &branch, &externalID, &url, &state, &reason, &workflow.PlanningAttempts, &workflow.ImplementationAttempts, &workflow.PipelineRepairAttempts, &workflow.CiRepairAttempts, &workflow.ReviewCycles, &workflow.TotalAgentExecutions, &created, &updated)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "workflow run is unknown")
	}
	if err != nil {
		return nil, databaseError(err)
	}
	workflow.BranchName, workflow.PullRequestExternalId, workflow.PullRequestUrl, workflow.PullRequestState, workflow.BlockingReason = stringValue(branch), stringValue(externalID), stringValue(url), stringValue(state), stringValue(reason)
	workflow.CreatedAt, workflow.UpdatedAt = timestamp(created), timestamp(updated)
	return workflow, nil
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
	var projectID, current string
	err = tx.QueryRow(ctx, `SELECT project_id::text,status FROM app.workflow_runs WHERE id=$1 FOR UPDATE`, id).Scan(&projectID, &current)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, status.Error(codes.NotFound, "workflow run is unknown")
	}
	if err != nil {
		return nil, databaseError(err)
	}
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
		if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, projectID, id); err != nil {
			return nil, databaseError(err)
		}
		if _, err := tx.Exec(ctx, `UPDATE app.issues SET eligible=true WHERE id=(SELECT issue_id FROM app.workflow_runs WHERE id=$1) AND state='open'`, id); err != nil {
			return nil, databaseError(err)
		}
		if _, err := tx.Exec(ctx, `INSERT INTO app.workflow_events(workflow_run_id,event_type,severity,payload) VALUES($1,'workflow_transition','info','{"reason":"reopened by manual retry"}'::jsonb)`, id); err != nil {
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
			if _, err := tx.Exec(ctx, `UPDATE app.workflow_runs SET status=$2,current_phase=$2,blocking_reason=CASE WHEN $2=`+qBlocked+` THEN $3 ELSE blocking_reason END,terminal_reason=$3,completed_at=now(),updated_at=now() WHERE id=$1`, id, next.String(), defaultReason(action, reason)); err != nil {
				return nil, databaseError(err)
			}
			err = tx.QueryRow(ctx, `UPDATE app.jobs SET status='cancelled',finished_at=now(),lease_generation=lease_generation+1,recovery_reason=$2 WHERE workflow_run_id=$1 AND status IN ('offered','preparing','running') RETURNING id::text,runner_id::text,lease_generation-1`, id, defaultReason(action, reason)).Scan(&jobToCancel, &runnerHoldingLease, &leaseGenerationAtCancel)
			if err != nil && !errors.Is(err, pgx.ErrNoRows) {
				return nil, databaseError(err)
			}
			// Blocking parks the issue; cancelling does not. Blocking says "stop
			// working on this until a human says otherwise", and without parking
			// the scheduler re-created the run from the still-eligible issue on
			// the next one-second tick — the operator's stop button restarting
			// the very work it stopped. Cancelling returns the issue to the
			// queue by design, so it stays eligible.
			if action == "block" {
				if err := parkIssue(ctx, tx, id); err != nil {
					return nil, err
				}
			}
			// Withdraw any offer still outstanding. Offers do not age out on
			// their own, so an offer left 'offered' is one a runner can still
			// answer after the operator has cancelled the work.
			if _, err := tx.Exec(ctx, `UPDATE app.job_offers SET status='cancelled',responded_at=now() WHERE status='offered' AND job_id IN (SELECT id FROM app.jobs WHERE workflow_run_id=$1)`, id); err != nil {
				return nil, databaseError(err)
			}
			if _, err := tx.Exec(ctx, `UPDATE app.workflow_execution_requests SET status='cancelled' WHERE workflow_run_id=$1 AND status IN ('queued','dispatched')`, id); err != nil {
				return nil, databaseError(err)
			}
			if _, err := tx.Exec(ctx, `DELETE FROM app.project_locks WHERE project_id=$1 AND workflow_run_id=$2`, projectID, id); err != nil {
				return nil, databaseError(err)
			}
		}
	}
	if err := audit(ctx, tx, actor.id, "workflow."+action, "workflow_run", id); err != nil {
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

func replacePipelineSteps(ctx context.Context, tx pgx.Tx, projectID string, steps []*controlv1.PipelineStep) error {
	if _, err := tx.Exec(ctx, `DELETE FROM app.project_pipeline_steps WHERE project_id=$1`, projectID); err != nil {
		return databaseError(err)
	}
	for _, step := range steps {
		if _, err := tx.Exec(ctx, `INSERT INTO app.project_pipeline_steps(id,project_id,position,name,command,timeout_seconds,required) VALUES($1,$2,$3,$4,$4,$5,$6)`, newID(), projectID, step.GetPosition(), step.GetCommand(), step.GetTimeoutSeconds(), step.GetRequired()); err != nil {
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
	runner := &controlv1.Runner{}
	row := s.pool.QueryRow(ctx, `SELECT id::text, name, enabled, draining, status, labels::text, last_seen_at, version FROM app.runners WHERE id=$1`, id)
	if err := scanRunnerRow(row, runner); err != nil {
		return nil, err
	}
	return runner, nil
}
func scanRunner(rows pgx.Rows) (*controlv1.Runner, error) {
	runner := &controlv1.Runner{}
	if err := scanRunnerRow(rows, runner); err != nil {
		return nil, err
	}
	return runner, nil
}
func scanRunnerRow(row pgx.Row, runner *controlv1.Runner) error {
	var labels []byte
	var seen *time.Time
	if err := row.Scan(&runner.Id, &runner.Name, &runner.Enabled, &runner.Draining, &runner.Status, &labels, &seen, &runner.Version); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return status.Error(codes.NotFound, "runner is unknown")
		}
		return databaseError(err)
	}
	if err := json.Unmarshal(labels, &runner.Labels); err != nil {
		return databaseError(err)
	}
	if seen != nil {
		runner.LastSeenAt = timestamp(*seen)
	}
	return nil
}
func audit(ctx context.Context, db interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, actorID, action, targetType, targetID string) error {
	_, err := db.Exec(ctx, `INSERT INTO app.audit_events(actor_type,actor_id,action,target_type,target_id) VALUES('user',$1,$2,$3,$4)`, actorID, action, targetType, targetID)
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

func passwordMatches(password, encoded string) (bool, error) {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[0] != "scrypt" || parts[1] != "16384" || parts[2] != "8" || parts[3] != "1" {
		return false, nil
	}
	salt, err := base64.RawURLEncoding.DecodeString(parts[4])
	if err != nil {
		return false, nil
	}
	expected, err := base64.RawURLEncoding.DecodeString(parts[5])
	if err != nil || len(expected) != 32 {
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
func parkIssue(ctx context.Context, tx pgx.Tx, workflowID string) error {
	_, err := tx.Exec(ctx, `UPDATE app.issues SET eligible=false WHERE id=(SELECT issue_id FROM app.workflow_runs WHERE id=$1)`, workflowID)
	return databaseError(err)
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
func truncate(value string, length int) string {
	if len(value) > length {
		return value[:length]
	}
	return value
}
func durationInterval(value time.Duration) string {
	return fmt.Sprintf("%d milliseconds", value.Milliseconds())
}
func parseInt(value string) (int64, error) {
	var result int64
	_, err := fmt.Sscan(value, &result)
	return result, err
}
