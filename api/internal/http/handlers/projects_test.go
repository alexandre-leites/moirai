package handlers

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/loop-engineering/api/internal/auth"
	"github.com/loop-engineering/api/internal/orchestrator"
	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

// fakeControlPlane is a minimal in-memory ControlPlane server. It exists so the
// project HTTP handlers can be exercised through a real gRPC client — the
// handlers take a concrete *orchestrator.Client, not an interface, so a nil
// client (as the other handler tests use) can never reach the project RPCs.
type fakeControlPlane struct {
	controlv1.UnimplementedControlPlaneServer
	mu       sync.Mutex
	projects map[string]*controlv1.Project
}

func newFakeControlPlane() *fakeControlPlane {
	return &fakeControlPlane{
		projects: map[string]*controlv1.Project{
			"p-1": {
				Id:                   "p-1",
				Name:                 "billing",
				Enabled:              true,
				RepositoryMode:       "managed_clone",
				RepositoryUrl:        "git@github.com:acme/billing.git",
				LocalRepositoryPath:  "",
				DefaultBranch:        "main",
				RequiredRunnerLabels: []string{"linux", "docker"},
			},
			"p-2": {
				Id:                  "p-2",
				Name:                "secure",
				Enabled:             false,
				RepositoryMode:      "managed_clone",
				RepositoryUrl:       "https://x-access-token:abc123@github.com/acme/secure.git",
				LocalRepositoryPath: "",
				DefaultBranch:       "trunk",
			},
		},
	}
}

func (f *fakeControlPlane) session(ctx context.Context, mutation bool) error {
	md, _ := metadata.FromIncomingContext(ctx)
	session := ""
	if v := md.Get("x-loop-session"); len(v) > 0 {
		session = v[0]
	}
	if !mutation {
		if session == "" || session == "unknown-session" {
			return status.Error(codes.Unauthenticated, "session is invalid")
		}
		return nil
	}
	switch session {
	case "admin-session":
		csrf := ""
		if v := md.Get("x-loop-csrf"); len(v) > 0 {
			csrf = v[0]
		}
		if csrf == "" {
			return status.Error(codes.Unauthenticated, "session is invalid")
		}
		return nil
	case "viewer-session":
		return status.Error(codes.PermissionDenied, "administrator access is required")
	default:
		return status.Error(codes.Unauthenticated, "session is invalid")
	}
}

func (f *fakeControlPlane) validateConfig(c *controlv1.ProjectConfiguration) error {
	if strings.TrimSpace(c.Name) == "" {
		return status.Error(codes.InvalidArgument, "project configuration is invalid")
	}
	return nil
}

func (f *fakeControlPlane) ListProjects(ctx context.Context, _ *controlv1.ListProjectsRequest) (*controlv1.ListProjectsResponse, error) {
	if err := f.session(ctx, false); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	projects := make([]*controlv1.Project, 0, len(f.projects))
	for _, p := range f.projects {
		projects = append(projects, p)
	}
	return &controlv1.ListProjectsResponse{Projects: projects}, nil
}

func (f *fakeControlPlane) CreateProject(ctx context.Context, req *controlv1.CreateProjectRequest) (*controlv1.CreateProjectResponse, error) {
	if err := f.session(ctx, true); err != nil {
		return nil, err
	}
	if err := f.validateConfig(req.Project); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	project := &controlv1.Project{
		Id:                   "p-new",
		Name:                 req.Project.Name,
		Enabled:              true,
		RepositoryMode:       req.Project.RepositoryMode,
		RepositoryUrl:        req.Project.RepositoryUrl,
		LocalRepositoryPath:  req.Project.LocalRepositoryPath,
		DefaultBranch:        req.Project.DefaultBranch,
		RequiredRunnerLabels: req.Project.RequiredRunnerLabels,
	}
	f.projects[project.Id] = project
	return &controlv1.CreateProjectResponse{Project: project}, nil
}

func (f *fakeControlPlane) UpdateProject(ctx context.Context, req *controlv1.UpdateProjectRequest) (*controlv1.UpdateProjectResponse, error) {
	if err := f.session(ctx, true); err != nil {
		return nil, err
	}
	if err := f.validateConfig(req.Project); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	project, ok := f.projects[req.ProjectId]
	if !ok {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	project.Name = req.Project.Name
	project.RepositoryMode = req.Project.RepositoryMode
	project.RepositoryUrl = req.Project.RepositoryUrl
	project.LocalRepositoryPath = req.Project.LocalRepositoryPath
	project.DefaultBranch = req.Project.DefaultBranch
	project.RequiredRunnerLabels = req.Project.RequiredRunnerLabels
	return &controlv1.UpdateProjectResponse{Project: project}, nil
}

func (f *fakeControlPlane) SetProjectEnabled(ctx context.Context, req *controlv1.SetProjectEnabledRequest) (*controlv1.SetProjectEnabledResponse, error) {
	if err := f.session(ctx, true); err != nil {
		return nil, err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	project, ok := f.projects[req.ProjectId]
	if !ok {
		return nil, status.Error(codes.NotFound, "project is unknown")
	}
	project.Enabled = req.Enabled
	return &controlv1.SetProjectEnabledResponse{Project: project}, nil
}

func startProjectServer(t *testing.T) (http.Handler, *fakeControlPlane) {
	t.Helper()
	fake := newFakeControlPlane()
	grpcServer := grpc.NewServer()
	controlv1.RegisterControlPlaneServer(grpcServer, fake)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	t.Cleanup(grpcServer.Stop)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := orchestrator.Dial(ctx, listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	t.Cleanup(func() { _ = client.Close() })

	mux := http.NewServeMux()
	NewProjectHandlers(client, auth.NewRateLimiter(time.Minute, 60)).RegisterRoutes(mux)
	return mux, fake
}

func projectRequest(t *testing.T, method, path, body string, session string) *http.Request {
	t.Helper()
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if session != "" {
		req.AddCookie(&http.Cookie{Name: auth.SessionCookieName, Value: session})
	}
	return req
}

func mutateRequest(t *testing.T, method, path, body, session string) *http.Request {
	t.Helper()
	req := projectRequest(t, method, path, body, session)
	if session != "" {
		req.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "csrf-token"})
		req.Header.Set(auth.CSRFHeaderName, "csrf-token")
	}
	return req
}

func decodeProject(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var body map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	return body
}

func TestProjectRoutesRequireSession(t *testing.T) {
	mux, _ := startProjectServer(t)
	for _, req := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/api/v1/projects", nil),
		projectRequest(t, http.MethodPut, "/api/v1/projects/p-1", `{"name":"x"}`, ""),
	} {
		rec := httptest.NewRecorder()
		mux.ServeHTTP(rec, req)
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without session: got %d, want %d", req.Method, req.URL.Path, rec.Code, http.StatusUnauthorized)
		}
	}
}

func TestListProjectsReturnsConfiguration(t *testing.T) {
	mux, _ := startProjectServer(t)
	req := projectRequest(t, http.MethodGet, "/api/v1/projects", "", "admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var body struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(body.Projects) != 2 {
		t.Fatalf("got %d projects, want 2", len(body.Projects))
	}
	billing := body.Projects[0]
	if billing["id"] != "p-1" && billing["name"] != "billing" {
		billing = body.Projects[1]
	}
	if billing["repositoryMode"] != "managed_clone" {
		t.Errorf("repositoryMode = %v, want managed_clone", billing["repositoryMode"])
	}
	if billing["repositoryUrl"] != "git@github.com:acme/billing.git" {
		t.Errorf("repositoryUrl = %v, want scp URL unchanged", billing["repositoryUrl"])
	}
	if billing["defaultBranch"] != "main" {
		t.Errorf("defaultBranch = %v, want main", billing["defaultBranch"])
	}
	labels, ok := billing["requiredRunnerLabels"].([]any)
	if !ok || len(labels) != 2 {
		t.Errorf("requiredRunnerLabels = %#v, want a 2-element array", billing["requiredRunnerLabels"])
	}
}

func TestListProjectsRedactsURLUserinfo(t *testing.T) {
	mux, _ := startProjectServer(t)
	req := projectRequest(t, http.MethodGet, "/api/v1/projects", "", "admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	var body struct {
		Projects []map[string]any `json:"projects"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode: %v", err)
	}
	secure := body.Projects[0]
	if secure["id"] != "p-2" && secure["name"] != "secure" {
		secure = body.Projects[1]
	}
	url := secure["repositoryUrl"].(string)
	if strings.Contains(url, "x-access-token") || strings.Contains(url, "abc123") {
		t.Errorf("repositoryUrl leaked userinfo: %q", url)
	}
	if !strings.HasPrefix(url, "https://redacted@") {
		t.Errorf("repositoryUrl = %q, want redacted userinfo", url)
	}
}

func TestCreateProjectSuccessReturnsConfiguration(t *testing.T) {
	mux, fake := startProjectServer(t)
	req := mutateRequest(t, http.MethodPost, "/api/v1/projects",
		`{"name":"ledger","repositoryMode":"managed_clone","repositoryUrl":"git@github.com:acme/ledger.git","defaultBranch":"main","requiredRunnerLabels":["linux"]}`,
		"admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusCreated {
		t.Fatalf("got %d, want %d: %s", rec.Code, http.StatusCreated, rec.Body.String())
	}
	project := decodeProject(t, rec)
	if project["repositoryMode"] != "managed_clone" || project["repositoryUrl"] != "git@github.com:acme/ledger.git" {
		t.Errorf("unexpected create payload: %v", project)
	}
	fake.mu.Lock()
	stored, ok := fake.projects["p-new"]
	fake.mu.Unlock()
	if !ok || stored.RepositoryUrl != "git@github.com:acme/ledger.git" {
		t.Errorf("stored project = %#v, want the submitted URL persisted", stored)
	}
}

func TestUpdateProjectSuccessPersistsAndReturnsConfiguration(t *testing.T) {
	mux, fake := startProjectServer(t)
	req := mutateRequest(t, http.MethodPut, "/api/v1/projects/p-1",
		`{"name":"billing","repositoryMode":"existing_path","localRepositoryPath":"/repositories/billing","defaultBranch":"trunk","requiredRunnerLabels":["linux"]}`,
		"admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want %d: %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	project := decodeProject(t, rec)
	if project["repositoryMode"] != "existing_path" {
		t.Errorf("repositoryMode = %v, want existing_path", project["repositoryMode"])
	}
	if project["localRepositoryPath"] != "/repositories/billing" {
		t.Errorf("localRepositoryPath = %v, want /repositories/billing", project["localRepositoryPath"])
	}
	if project["defaultBranch"] != "trunk" {
		t.Errorf("defaultBranch = %v, want trunk", project["defaultBranch"])
	}
	fake.mu.Lock()
	stored := fake.projects["p-1"]
	fake.mu.Unlock()
	if stored.RepositoryMode != "existing_path" || stored.LocalRepositoryPath != "/repositories/billing" || stored.DefaultBranch != "trunk" {
		t.Errorf("stored project = %#v, want the submitted configuration", stored)
	}
}

func TestUpdateProjectRejectsMalformedJSON(t *testing.T) {
	mux, _ := startProjectServer(t)
	req := mutateRequest(t, http.MethodPut, "/api/v1/projects/p-1", `{"name":`, "admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("got %d, want %d", rec.Code, http.StatusBadRequest)
	}
}

func TestUpdateProjectValidationFailure(t *testing.T) {
	mux, _ := startProjectServer(t)
	req := mutateRequest(t, http.MethodPut, "/api/v1/projects/p-1",
		`{"name":"   ","repositoryMode":"managed_clone","defaultBranch":"main"}`, "admin-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, req)

	if rec.Code != http.StatusUnprocessableEntity {
		t.Errorf("got %d, want %d: %s", rec.Code, http.StatusUnprocessableEntity, rec.Body.String())
	}
}

func TestUpdateProjectUnauthorizedMutation(t *testing.T) {
	mux, _ := startProjectServer(t)
	body := `{"name":"billing","repositoryMode":"managed_clone","repositoryUrl":"git@github.com:acme/billing.git","defaultBranch":"main"}`

	viewer := mutateRequest(t, http.MethodPut, "/api/v1/projects/p-1", body, "viewer-session")
	rec := httptest.NewRecorder()
	mux.ServeHTTP(rec, viewer)
	if rec.Code != http.StatusForbidden {
		t.Errorf("viewer mutation: got %d, want %d", rec.Code, http.StatusForbidden)
	}

	noCSRF := projectRequest(t, http.MethodPut, "/api/v1/projects/p-1", body, "admin-session")
	noCSRF.AddCookie(&http.Cookie{Name: auth.CSRFCookieName, Value: "csrf-token"})
	rec = httptest.NewRecorder()
	mux.ServeHTTP(rec, noCSRF)
	if rec.Code != http.StatusForbidden {
		t.Errorf("mutation without CSRF header: got %d, want %d", rec.Code, http.StatusForbidden)
	}
}

func TestRedactURLUserinfo(t *testing.T) {
	cases := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"https://github.com/acme/repo.git", "https://github.com/acme/repo.git"},
		{"https://user:secret@github.com/acme/repo.git", "https://redacted@github.com/acme/repo.git"},
		{"https://token@github.com/acme/repo.git", "https://redacted@github.com/acme/repo.git"},
		{"git@github.com:acme/repo.git", "git@github.com:acme/repo.git"},
		{"/repositories/ledger", "/repositories/ledger"},
		{"ssh://git@github.com/acme/repo.git", "ssh://git@github.com/acme/repo.git"},
	}
	for _, tc := range cases {
		if got := redactURLUserinfo(tc.in); got != tc.want {
			t.Errorf("redactURLUserinfo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
