package orchestrator

import (
	"context"
	"errors"
	"fmt"

	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/connectivity"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

var (
	ErrUnavailable  = errors.New("orchestrator unavailable")
	ErrUnauthorized = errors.New("orchestrator rejected request: unauthorized")
	ErrInvalidInput = errors.New("orchestrator rejected request: invalid input")
	ErrNotFound     = errors.New("orchestrator resource not found")
)

type Client struct {
	conn   *grpc.ClientConn
	client controlv1.ControlPlaneClient
}

func WithSession(ctx context.Context, sessionToken string, csrfToken ...string) context.Context {
	if sessionToken == "" {
		return ctx
	}
	values := []string{"x-loop-session", sessionToken}
	if len(csrfToken) > 0 && csrfToken[0] != "" {
		values = append(values, "x-loop-csrf", csrfToken[0])
	}
	return metadata.AppendToOutgoingContext(ctx, values...)
}

func Dial(ctx context.Context, endpoint string) (*Client, error) {
	if endpoint == "" {
		return nil, fmt.Errorf("orchestrator endpoint is required")
	}
	conn, err := grpc.DialContext(
		ctx,
		endpoint,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
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

func (c *Client) ListWorkflows(ctx context.Context) (*controlv1.ListWorkflowsResponse, error) {
	resp, err := c.client.ListWorkflows(ctx, &controlv1.ListWorkflowsRequest{})
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
	case codes.Unauthenticated, codes.PermissionDenied:
		return fmt.Errorf("%w: %s", ErrUnauthorized, s.Message())
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
