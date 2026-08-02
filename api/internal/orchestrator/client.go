package orchestrator

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"

	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"github.com/prometheus/client_golang/prometheus"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	ErrUnavailable  = errors.New("orchestrator unavailable")
	ErrUnauthorized = errors.New("orchestrator rejected request: unauthorized")
	ErrForbidden    = errors.New("orchestrator rejected request: forbidden")
	ErrInvalidInput = errors.New("orchestrator rejected request: invalid input")
	ErrNotFound     = errors.New("orchestrator resource not found")
)

// orchestratorCalls counts every unary RPC the API issues to the orchestrator.
// Both labels are bounded by construction: `rpc` is the method name from the
// generated client, so its values are fixed by the proto service, and `code` is
// a gRPC status code, a closed enum. It is a package variable rather than a
// field because the interceptor that records it is installed at dial time,
// before any server exists to hold it; Collectors exposes it for registration.
var orchestratorCalls = prometheus.NewCounterVec(prometheus.CounterOpts{
	Name: "moirai_api_orchestrator_calls_total",
	Help: "Orchestrator gRPC calls issued by the API, by RPC and resulting gRPC status code.",
}, []string{"rpc", "code"})

// Collectors returns this package's Prometheus collectors so the API server can
// export them from the registry it serves.
func Collectors() []prometheus.Collector {
	return []prometheus.Collector{orchestratorCalls}
}

type EventStream interface {
	Recv() (*controlv1.ControlPlaneEvent, error)
}

type Client struct {
	conn   *grpc.ClientConn
	client controlv1.ControlPlaneClient
}

type TLSOptions struct {
	Enabled    bool
	CAFile     string
	ServerName string
}

type requestIDKey struct{}

func WithRequestID(ctx context.Context, requestID string) context.Context {
	return context.WithValue(ctx, requestIDKey{}, requestID)
}

func requestIDFromContext(ctx context.Context) string {
	requestID, _ := ctx.Value(requestIDKey{}).(string)
	return requestID
}

func WithSession(ctx context.Context, sessionToken string, csrfToken ...string) context.Context {
	if sessionToken == "" {
		return ctx
	}
	md := metadata.Pairs("x-loop-session", sessionToken)
	if len(csrfToken) > 0 && csrfToken[0] != "" {
		md.Append("x-loop-csrf", csrfToken[0])
	}
	return metadata.NewOutgoingContext(ctx, md)
}

func Dial(ctx context.Context, endpoint string) (*Client, error) {
	return DialWithTLS(ctx, endpoint, TLSOptions{})
}

func DialWithTLS(ctx context.Context, endpoint string, options TLSOptions) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("orchestrator endpoint is required")
	}
	transport, err := transportCredentials(options)
	if err != nil {
		return nil, err
	}
	conn, err := grpc.DialContext(
		ctx,
		endpoint,
		grpc.WithTransportCredentials(transport),
		grpc.WithChainUnaryInterceptor(correlationInterceptor, callMetricsInterceptor),
		grpc.WithBlock(),
	)
	if err != nil {
		return nil, fmt.Errorf("dial orchestrator %s: %w", endpoint, err)
	}
	return &Client{
		conn:   conn,
		client: controlv1.NewControlPlaneClient(conn),
	}, nil
}

func correlationInterceptor(ctx context.Context, method string, request, reply any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
	if requestID := requestIDFromContext(ctx); requestID != "" {
		ctx = metadata.AppendToOutgoingContext(ctx, "x-request-id", requestID)
	}
	return invoke(ctx, method, request, reply, conn, options...)
}

// callMetricsInterceptor counts the outcome of every unary orchestrator RPC.
// It sits on the interceptor chain rather than in each wrapper method because
// the chain is the one place every call passes through, so a new RPC added to
// Client is counted without anyone remembering to instrument it.
func callMetricsInterceptor(ctx context.Context, method string, request, reply any, conn *grpc.ClientConn, invoke grpc.UnaryInvoker, options ...grpc.CallOption) error {
	err := invoke(ctx, method, request, reply, conn, options...)
	orchestratorCalls.WithLabelValues(rpcLabel(method), status.Code(err).String()).Inc()
	return err
}

// rpcLabel reduces a fully qualified gRPC method — "/control.v1.ControlPlane/Login"
// — to its method name. The service is the same for every call this client
// makes, so the package qualifier is noise in a label.
func rpcLabel(method string) string {
	if index := strings.LastIndex(method, "/"); index >= 0 && index+1 < len(method) {
		return method[index+1:]
	}
	if method == "" {
		return "unknown"
	}
	return method
}

func transportCredentials(options TLSOptions) (credentials.TransportCredentials, error) {
	if !options.Enabled {
		if options.CAFile != "" || options.ServerName != "" {
			return nil, errors.New("orchestrator TLS options require TLS")
		}
		return insecure.NewCredentials(), nil
	}
	config := &tls.Config{MinVersion: tls.VersionTLS12, ServerName: options.ServerName}
	if options.CAFile != "" {
		contents, err := os.ReadFile(options.CAFile)
		if err != nil {
			return nil, fmt.Errorf("read orchestrator TLS CA file: %w", err)
		}
		roots := x509.NewCertPool()
		if !roots.AppendCertsFromPEM(contents) {
			return nil, errors.New("orchestrator TLS CA file contains no certificates")
		}
		config.RootCAs = roots
	}
	return credentials.NewTLS(config), nil
}

func (c *Client) Close() error {
	return c.conn.Close()
}

// Healthy reports whether the gRPC connection to the orchestrator is usable.
// It inspects the channel's connectivity state rather than issuing an RPC, so
// it is cheap enough to call on every health/readiness probe.
func (c *Client) Healthy() bool {
	state := c.conn.GetState()
	return state == connectivity.Ready || state == connectivity.Idle
}

func (c *Client) Logout(ctx context.Context) error {
	_, err := c.client.Logout(ctx, &controlv1.LogoutRequest{})
	return mapError(err)
}

func (c *Client) Login(ctx context.Context, username, password string) (*controlv1.LoginResponse, error) {
	resp, err := c.client.Login(ctx, &controlv1.LoginRequest{
		Username: username,
		Password: password,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) WhoAmI(ctx context.Context) (*controlv1.WhoAmIResponse, error) {
	resp, err := c.client.WhoAmI(ctx, &controlv1.WhoAmIRequest{})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) UpdateAccount(ctx context.Context, req *controlv1.UpdateAccountRequest) (*controlv1.UpdateAccountResponse, error) {
	resp, err := c.client.UpdateAccount(ctx, req)
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ListProjects(ctx context.Context) (*controlv1.ListProjectsResponse, error) {
	resp, err := c.client.ListProjects(ctx, &controlv1.ListProjectsRequest{})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) CreateProject(ctx context.Context, req *controlv1.CreateProjectRequest) (*controlv1.CreateProjectResponse, error) {
	resp, err := c.client.CreateProject(ctx, req)
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) UpdateProject(ctx context.Context, req *controlv1.UpdateProjectRequest) (*controlv1.UpdateProjectResponse, error) {
	resp, err := c.client.UpdateProject(ctx, req)
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) SetProjectEnabled(ctx context.Context, projectID string, enabled bool) (*controlv1.SetProjectEnabledResponse, error) {
	resp, err := c.client.SetProjectEnabled(ctx, &controlv1.SetProjectEnabledRequest{
		ProjectId: projectID,
		Enabled:   enabled,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) SetProjectCredential(ctx context.Context, projectID, kind, value string) (*controlv1.SetProjectCredentialResponse, error) {
	resp, err := c.client.SetProjectCredential(ctx, &controlv1.SetProjectCredentialRequest{
		ProjectId: projectID,
		Kind:      kind,
		Value:     value,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ClearProjectCredential(ctx context.Context, projectID, kind string) (*controlv1.ClearProjectCredentialResponse, error) {
	resp, err := c.client.ClearProjectCredential(ctx, &controlv1.ClearProjectCredentialRequest{
		ProjectId: projectID,
		Kind:      kind,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ListProjectCredentials(ctx context.Context, projectID string) (*controlv1.ListProjectCredentialsResponse, error) {
	resp, err := c.client.ListProjectCredentials(ctx, &controlv1.ListProjectCredentialsRequest{ProjectId: projectID})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) CreateRunnerRegistrationToken(ctx context.Context, allowedLabels []string) (*controlv1.CreateRunnerRegistrationTokenResponse, error) {
	resp, err := c.client.CreateRunnerRegistrationToken(ctx, &controlv1.CreateRunnerRegistrationTokenRequest{
		AllowedLabels: allowedLabels,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ListRunnerRegistrationTokens(ctx context.Context) (*controlv1.ListRunnerRegistrationTokensResponse, error) {
	resp, err := c.client.ListRunnerRegistrationTokens(ctx, &controlv1.ListRunnerRegistrationTokensRequest{})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) RevokeRunnerRegistrationToken(ctx context.Context, tokenID string) (*controlv1.RevokeRunnerRegistrationTokenResponse, error) {
	resp, err := c.client.RevokeRunnerRegistrationToken(ctx, &controlv1.RevokeRunnerRegistrationTokenRequest{
		TokenId: tokenID,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ListRunners(ctx context.Context) (*controlv1.ListRunnersResponse, error) {
	resp, err := c.client.ListRunners(ctx, &controlv1.ListRunnersRequest{})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ListQueue(ctx context.Context, limit int32) (*controlv1.ListQueueResponse, error) {
	resp, err := c.client.ListQueue(ctx, &controlv1.ListQueueRequest{Limit: limit})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) SyncNow(ctx context.Context, projectID string) (*controlv1.SyncNowResponse, error) {
	resp, err := c.client.SyncNow(ctx, &controlv1.SyncNowRequest{ProjectId: projectID})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) IssueSyncStatus(ctx context.Context) (*controlv1.IssueSyncStatusResponse, error) {
	resp, err := c.client.IssueSyncStatus(ctx, &controlv1.IssueSyncStatusRequest{})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) GetSchedulerMetrics(ctx context.Context) (*controlv1.GetSchedulerMetricsResponse, error) {
	resp, err := c.client.GetSchedulerMetrics(ctx, &controlv1.GetSchedulerMetricsRequest{})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) StreamEvents(ctx context.Context, lastEventID string) (EventStream, error) {
	stream, err := c.client.StreamEvents(ctx, &controlv1.StreamEventsRequest{LastEventId: lastEventID})
	if err != nil {
		return nil, mapError(err)
	}
	return stream, nil
}

func (c *Client) SetRunnerState(ctx context.Context, runnerID, state string) (*controlv1.SetRunnerStateResponse, error) {
	resp, err := c.client.SetRunnerState(ctx, &controlv1.SetRunnerStateRequest{RunnerId: runnerID, State: state})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ListWorkflows(ctx context.Context) (*controlv1.ListWorkflowsResponse, error) {
	resp, err := c.client.ListWorkflows(ctx, &controlv1.ListWorkflowsRequest{})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) GetWorkflow(ctx context.Context, workflowRunID string) (*controlv1.GetWorkflowResponse, error) {
	resp, err := c.client.GetWorkflow(ctx, &controlv1.GetWorkflowRequest{WorkflowRunId: workflowRunID})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) ListWorkflowEvents(ctx context.Context, workflowRunID string, afterID int64, limit int32) (*controlv1.ListWorkflowEventsResponse, error) {
	resp, err := c.client.ListWorkflowEvents(ctx, &controlv1.ListWorkflowEventsRequest{
		WorkflowRunId: workflowRunID,
		AfterId:       afterID,
		Limit:         limit,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) SubmitHumanDecision(ctx context.Context, workflowRunID, decision, comment string) (*controlv1.SubmitHumanDecisionResponse, error) {
	resp, err := c.client.SubmitHumanDecision(ctx, &controlv1.SubmitHumanDecisionRequest{
		WorkflowRunId: workflowRunID,
		Decision:      decision,
		Comment:       comment,
	})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) RetryWorkflow(ctx context.Context, workflowRunID, reason string) (*controlv1.RetryWorkflowResponse, error) {
	resp, err := c.client.RetryWorkflow(ctx, &controlv1.RetryWorkflowRequest{WorkflowRunId: workflowRunID, Reason: reason})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) CancelWorkflow(ctx context.Context, workflowRunID, reason string) (*controlv1.CancelWorkflowResponse, error) {
	resp, err := c.client.CancelWorkflow(ctx, &controlv1.CancelWorkflowRequest{WorkflowRunId: workflowRunID, Reason: reason})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func (c *Client) BlockWorkflow(ctx context.Context, workflowRunID, reason string) (*controlv1.BlockWorkflowResponse, error) {
	resp, err := c.client.BlockWorkflow(ctx, &controlv1.BlockWorkflowRequest{WorkflowRunId: workflowRunID, Reason: reason})
	if err != nil {
		return nil, mapError(err)
	}
	return resp, nil
}

func mapError(err error) error {
	return MapStatusError(err)
}

func MapStatusError(err error) error {
	if err == nil {
		return nil
	}
	s, ok := status.FromError(err)
	if !ok {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	switch s.Code() {
	case codes.Unauthenticated:
		return fmt.Errorf("%w: %s", ErrUnauthorized, s.Message())
	case codes.PermissionDenied:
		return fmt.Errorf("%w: %s", ErrForbidden, s.Message())
	case codes.InvalidArgument, codes.FailedPrecondition:
		return fmt.Errorf("%w: %s", ErrInvalidInput, s.Message())
	case codes.NotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, s.Message())
	case codes.Canceled, codes.DeadlineExceeded:
		return context.DeadlineExceeded
	case codes.Unavailable, codes.ResourceExhausted, codes.Internal, codes.Unknown:
		return fmt.Errorf("%w: %s", ErrUnavailable, s.Message())
	default:
		return fmt.Errorf("%w: %s (code %s)", ErrUnavailable, s.Message(), s.Code())
	}
}
