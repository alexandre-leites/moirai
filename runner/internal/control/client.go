package control

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
)

var ErrNotConnected = errors.New("runner control stream is not connected")

type Stream interface {
	Send(*runnerv1.RunnerToOrchestrator) error
	Recv() (*runnerv1.OrchestratorToRunner, error)
}

type ControlService interface {
	RegisterRunner(context.Context, *runnerv1.RegisterRunnerRequest) (*runnerv1.RegisterRunnerResponse, error)
	Connect(context.Context) (Stream, error)
}

type Identity struct {
	RunnerID   string
	Credential string
}

type grpcService struct {
	client runnerv1.RunnerControlClient
}

func (s grpcService) RegisterRunner(ctx context.Context, request *runnerv1.RegisterRunnerRequest) (*runnerv1.RegisterRunnerResponse, error) {
	return s.client.RegisterRunner(ctx, request)
}

func (s grpcService) Connect(ctx context.Context) (Stream, error) {
	stream, err := s.client.Connect(ctx)
	if err != nil {
		return nil, err
	}
	return stream, nil
}

type Client struct {
	service      ControlService
	identity     Identity
	version      string
	mu           sync.Mutex
	stream       Stream
	streamCancel context.CancelFunc
}

func NewClient(service ControlService, identity Identity) (*Client, error) {
	if service == nil {
		return nil, errors.New("runner control service is required")
	}
	if identity.RunnerID == "" || identity.Credential == "" {
		return nil, errors.New("runner identity and credential are required")
	}
	return &Client{service: service, identity: identity}, nil
}

func NewClientFromGRPC(service runnerv1.RunnerControlClient, identity Identity) (*Client, error) {
	if service == nil {
		return nil, errors.New("runner control service is required")
	}
	return NewClient(NewGRPCService(service), identity)
}

func NewGRPCService(service runnerv1.RunnerControlClient) ControlService {
	return grpcService{client: service}
}

func Register(ctx context.Context, service ControlService, token, name string, labels []string, capacity int) (Identity, error) {
	if service == nil {
		return Identity{}, errors.New("runner control service is required")
	}
	if token == "" || name == "" {
		return Identity{}, errors.New("runner registration token and name are required")
	}
	if capacity < 1 {
		capacity = 1
	}
	response, err := service.RegisterRunner(ctx, &runnerv1.RegisterRunnerRequest{
		Token:           token,
		Name:            name,
		Labels:          labels,
		ProtocolVersion: "1.0",
		Capacity:        int32(capacity),
	})
	if err != nil {
		return Identity{}, err
	}
	if response.GetRunnerId() == "" || response.GetCredential() == "" {
		return Identity{}, errors.New("runner registration response is invalid")
	}
	return Identity{RunnerID: response.GetRunnerId(), Credential: response.GetCredential()}, nil
}

func RegisterWithGRPC(ctx context.Context, service runnerv1.RunnerControlClient, token, name string, labels []string, capacity int) (Identity, error) {
	if service == nil {
		return Identity{}, errors.New("runner control service is required")
	}
	return Register(ctx, grpcService{client: service}, token, name, labels, capacity)
}

func (c *Client) Connect(ctx context.Context) error {
	streamCtx, cancel := context.WithCancel(ctx)
	stream, err := c.service.Connect(streamCtx)
	if err != nil {
		cancel()
		return err
	}
	c.mu.Lock()
	previousCancel := c.streamCancel
	c.stream = stream
	c.streamCancel = cancel
	c.mu.Unlock()
	if previousCancel != nil {
		previousCancel()
	}
	return nil
}

func (c *Client) Disconnect() {
	c.mu.Lock()
	cancel := c.streamCancel
	c.stream = nil
	c.streamCancel = nil
	c.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

func (c *Client) SetVersion(version string) {
	c.mu.Lock()
	c.version = version
	c.mu.Unlock()
}

func (c *Client) Heartbeat(labels []string, busy bool) error {
	return c.send(&runnerv1.RunnerToOrchestrator{
		Message: &runnerv1.RunnerToOrchestrator_Heartbeat{
			Heartbeat: &runnerv1.Heartbeat{Labels: labels, Busy: busy, Version: c.version},
		},
	})
}

func (c *Client) AcceptOffer(jobID string) error {
	if jobID == "" {
		return errors.New("job ID is required")
	}
	return c.send(&runnerv1.RunnerToOrchestrator{
		Message: &runnerv1.RunnerToOrchestrator_OfferAccepted{
			OfferAccepted: &runnerv1.JobOfferAccepted{JobId: jobID},
		},
	})
}

func (c *Client) RejectOffer(jobID, reason string) error {
	if jobID == "" || len(reason) > 1024 {
		return errors.New("offer rejection is invalid")
	}
	return c.send(&runnerv1.RunnerToOrchestrator{
		Message: &runnerv1.RunnerToOrchestrator_OfferRejected{
			OfferRejected: &runnerv1.JobOfferRejected{JobId: jobID, Reason: reason},
		},
	})
}

func (c *Client) SetDraining(draining bool) error {
	return c.send(&runnerv1.RunnerToOrchestrator{
		Message: &runnerv1.RunnerToOrchestrator_RunnerDraining{
			RunnerDraining: &runnerv1.RunnerDraining{Draining: draining},
		},
	})
}

func (c *Client) RenewLease(jobID string, generation int64, expiresAt time.Time) error {
	if jobID == "" || generation < 1 || !expiresAt.After(time.Now()) {
		return errors.New("lease renewal is invalid")
	}
	return c.send(&runnerv1.RunnerToOrchestrator{
		Message: &runnerv1.RunnerToOrchestrator_LeaseRenewal{
			LeaseRenewal: &runnerv1.LeaseRenewal{
				JobId:                    jobID,
				LeaseGeneration:          generation,
				RequestedExpiresAtUnixMs: expiresAt.UnixMilli(),
			},
		},
	})
}

func (c *Client) SendExecutionEvent(event *runnerv1.ExecutionEvent) error {
	if event == nil || event.GetJobId() == "" || event.GetExecutionId() == "" || event.GetLeaseGeneration() < 1 || event.GetEventSequence() < 1 || event.GetType() == "" || len(event.GetPayloadJson()) > 16*1024 || !json.Valid([]byte(event.GetPayloadJson())) {
		return errors.New("execution event is invalid")
	}
	return c.send(&runnerv1.RunnerToOrchestrator{
		Message: &runnerv1.RunnerToOrchestrator_Event{Event: event},
	})
}

func (c *Client) Receive() (*runnerv1.OrchestratorToRunner, error) {
	c.mu.Lock()
	stream := c.stream
	c.mu.Unlock()
	if stream == nil {
		return nil, ErrNotConnected
	}
	return stream.Recv()
}

func (c *Client) send(message *runnerv1.RunnerToOrchestrator) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.stream == nil {
		return ErrNotConnected
	}
	message.RunnerId = c.identity.RunnerID
	message.Credential = c.identity.Credential
	return c.stream.Send(message)
}
