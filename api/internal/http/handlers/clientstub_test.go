package handlers

import (
	"context"
	"sync"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
)

// stubClient implements the per-resource orchestrator client interfaces the
// handlers depend on (runnerClient, runnerTokenClient, syncClient,
// workflowClient, authClient). Each hook is the canned answer for one RPC; a
// test sets only the hooks the handler under test reaches, so an unexpected
// call is a nil-hook panic rather than a silently plausible empty response.
//
// It also records the arguments every call carried, which is how the tests
// assert the handler forwarded path values, query parameters and body fields
// rather than only that it answered 200.
type stubClient struct {
	mu    sync.Mutex
	calls []stubCall

	listRunners    func(ctx context.Context) (*controlv1.ListRunnersResponse, error)
	setRunnerState func(ctx context.Context, runnerID, state string) (*controlv1.SetRunnerStateResponse, error)

	listTokens  func(ctx context.Context) (*controlv1.ListRunnerRegistrationTokensResponse, error)
	createToken func(ctx context.Context, allowedLabels []string) (*controlv1.CreateRunnerRegistrationTokenResponse, error)
	revokeToken func(ctx context.Context, tokenID string) (*controlv1.RevokeRunnerRegistrationTokenResponse, error)

	syncNow         func(ctx context.Context, projectID string) (*controlv1.SyncNowResponse, error)
	issueSyncStatus func(ctx context.Context) (*controlv1.IssueSyncStatusResponse, error)

	listWorkflows      func(ctx context.Context) (*controlv1.ListWorkflowsResponse, error)
	getWorkflow        func(ctx context.Context, id string) (*controlv1.GetWorkflowResponse, error)
	listWorkflowEvents func(ctx context.Context, id string, afterID int64, limit int32) (*controlv1.ListWorkflowEventsResponse, error)
	submitDecision     func(ctx context.Context, id, decision, comment string) (*controlv1.SubmitHumanDecisionResponse, error)
	retryWorkflow      func(ctx context.Context, id, reason string, resume bool) (*controlv1.RetryWorkflowResponse, error)
	cancelWorkflow     func(ctx context.Context, id, reason string) (*controlv1.CancelWorkflowResponse, error)
	blockWorkflow      func(ctx context.Context, id, reason string) (*controlv1.BlockWorkflowResponse, error)

	login         func(ctx context.Context, username, password string) (*controlv1.LoginResponse, error)
	logout        func(ctx context.Context) error
	whoAmI        func(ctx context.Context) (*controlv1.WhoAmIResponse, error)
	updateAccount func(ctx context.Context, req *controlv1.UpdateAccountRequest) (*controlv1.UpdateAccountResponse, error)
}

// stubCall is one recorded RPC: its name and the arguments the handler chose.
type stubCall struct {
	method string
	args   []any
}

func (s *stubClient) record(method string, args ...any) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.calls = append(s.calls, stubCall{method: method, args: args})
}

// recorded returns the calls made to one method, in order.
func (s *stubClient) recorded(method string) []stubCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []stubCall
	for _, call := range s.calls {
		if call.method == method {
			out = append(out, call)
		}
	}
	return out
}

func (s *stubClient) ListRunners(ctx context.Context) (*controlv1.ListRunnersResponse, error) {
	s.record("ListRunners")
	return s.listRunners(ctx)
}

func (s *stubClient) SetRunnerState(ctx context.Context, runnerID, state string) (*controlv1.SetRunnerStateResponse, error) {
	s.record("SetRunnerState", runnerID, state)
	return s.setRunnerState(ctx, runnerID, state)
}

func (s *stubClient) ListRunnerRegistrationTokens(ctx context.Context) (*controlv1.ListRunnerRegistrationTokensResponse, error) {
	s.record("ListRunnerRegistrationTokens")
	return s.listTokens(ctx)
}

func (s *stubClient) CreateRunnerRegistrationToken(ctx context.Context, allowedLabels []string) (*controlv1.CreateRunnerRegistrationTokenResponse, error) {
	s.record("CreateRunnerRegistrationToken", allowedLabels)
	return s.createToken(ctx, allowedLabels)
}

func (s *stubClient) RevokeRunnerRegistrationToken(ctx context.Context, tokenID string) (*controlv1.RevokeRunnerRegistrationTokenResponse, error) {
	s.record("RevokeRunnerRegistrationToken", tokenID)
	return s.revokeToken(ctx, tokenID)
}

func (s *stubClient) SyncNow(ctx context.Context, projectID string) (*controlv1.SyncNowResponse, error) {
	s.record("SyncNow", projectID)
	return s.syncNow(ctx, projectID)
}

func (s *stubClient) IssueSyncStatus(ctx context.Context) (*controlv1.IssueSyncStatusResponse, error) {
	s.record("IssueSyncStatus")
	return s.issueSyncStatus(ctx)
}

func (s *stubClient) ListWorkflows(ctx context.Context) (*controlv1.ListWorkflowsResponse, error) {
	s.record("ListWorkflows")
	return s.listWorkflows(ctx)
}

func (s *stubClient) GetWorkflow(ctx context.Context, id string) (*controlv1.GetWorkflowResponse, error) {
	s.record("GetWorkflow", id)
	return s.getWorkflow(ctx, id)
}

func (s *stubClient) ListWorkflowEvents(ctx context.Context, id string, afterID int64, limit int32) (*controlv1.ListWorkflowEventsResponse, error) {
	s.record("ListWorkflowEvents", id, afterID, limit)
	return s.listWorkflowEvents(ctx, id, afterID, limit)
}

func (s *stubClient) SubmitHumanDecision(ctx context.Context, id, decision, comment string) (*controlv1.SubmitHumanDecisionResponse, error) {
	s.record("SubmitHumanDecision", id, decision, comment)
	return s.submitDecision(ctx, id, decision, comment)
}

func (s *stubClient) RetryWorkflow(ctx context.Context, id, reason string, resume bool) (*controlv1.RetryWorkflowResponse, error) {
	s.record("RetryWorkflow", id, reason, resume)
	return s.retryWorkflow(ctx, id, reason, resume)
}

func (s *stubClient) CancelWorkflow(ctx context.Context, id, reason string) (*controlv1.CancelWorkflowResponse, error) {
	s.record("CancelWorkflow", id, reason)
	return s.cancelWorkflow(ctx, id, reason)
}

func (s *stubClient) BlockWorkflow(ctx context.Context, id, reason string) (*controlv1.BlockWorkflowResponse, error) {
	s.record("BlockWorkflow", id, reason)
	return s.blockWorkflow(ctx, id, reason)
}

func (s *stubClient) Login(ctx context.Context, username, password string) (*controlv1.LoginResponse, error) {
	s.record("Login", username, password)
	return s.login(ctx, username, password)
}

func (s *stubClient) Logout(ctx context.Context) error {
	s.record("Logout")
	return s.logout(ctx)
}

func (s *stubClient) WhoAmI(ctx context.Context) (*controlv1.WhoAmIResponse, error) {
	s.record("WhoAmI")
	return s.whoAmI(ctx)
}

func (s *stubClient) UpdateAccount(ctx context.Context, req *controlv1.UpdateAccountRequest) (*controlv1.UpdateAccountResponse, error) {
	s.record("UpdateAccount", req)
	return s.updateAccount(ctx, req)
}

// Compile-time proof the stub really is substitutable for the production
// client on every interface the handlers declare.
var (
	_ runnerClient      = (*stubClient)(nil)
	_ runnerTokenClient = (*stubClient)(nil)
	_ syncClient        = (*stubClient)(nil)
	_ workflowClient    = (*stubClient)(nil)
	_ authClient        = (*stubClient)(nil)
)
