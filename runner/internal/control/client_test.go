package control

import (
	"context"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
)

type fakeService struct {
	registration *runnerv1.RegisterRunnerRequest
	stream       *fakeStream
	context      context.Context
}

func (s *fakeService) RegisterRunner(_ context.Context, request *runnerv1.RegisterRunnerRequest) (*runnerv1.RegisterRunnerResponse, error) {
	s.registration = request
	return &runnerv1.RegisterRunnerResponse{RunnerId: "runner-1", Credential: "credential-1"}, nil
}

func (s *fakeService) Connect(ctx context.Context) (Stream, error) {
	s.context = ctx
	return s.stream, nil
}

type fakeStream struct {
	sent     []*runnerv1.RunnerToOrchestrator
	received []*runnerv1.OrchestratorToRunner
}

func (s *fakeStream) Send(message *runnerv1.RunnerToOrchestrator) error {
	s.sent = append(s.sent, message)
	return nil
}

func (s *fakeStream) Recv() (*runnerv1.OrchestratorToRunner, error) {
	if len(s.received) == 0 {
		return nil, errors.New("no response")
	}
	message := s.received[0]
	s.received = s.received[1:]
	return message, nil
}

func TestRegisterAndConnectSendsAuthenticatedLeaseMessages(t *testing.T) {
	service := &fakeService{stream: &fakeStream{received: []*runnerv1.OrchestratorToRunner{{
		Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{
			LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 1},
		},
	}}}}
	identity, err := Register(context.Background(), service, "token", "runner", []string{"docker"}, 3)
	if err != nil {
		t.Fatalf("Register() error = %v", err)
	}
	if service.registration.GetProtocolVersion() != "1.0" {
		t.Fatalf("protocol version = %q", service.registration.GetProtocolVersion())
	}
	if service.registration.GetCapacity() != 3 {
		t.Fatalf("capacity = %d, want 3", service.registration.GetCapacity())
	}
	client, err := NewClient(service, identity)
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatalf("Connect() error = %v", err)
	}
	if err := client.AcceptOffer("job-1"); err != nil {
		t.Fatalf("AcceptOffer() error = %v", err)
	}
	if err := client.RenewLease("job-1", 1, time.Now().Add(time.Minute)); err != nil {
		t.Fatalf("RenewLease() error = %v", err)
	}
	if err := client.SendExecutionEvent(&runnerv1.ExecutionEvent{JobId: "job-1", ExecutionId: "execution-1", LeaseGeneration: 1, EventSequence: 1, Type: "started", PayloadJson: `{}`}); err != nil {
		t.Fatalf("SendExecutionEvent() error = %v", err)
	}
	if len(service.stream.sent) != 3 {
		t.Fatalf("sent messages = %d, want 3", len(service.stream.sent))
	}
	for _, message := range service.stream.sent {
		if message.GetRunnerId() != identity.RunnerID || message.GetCredential() != identity.Credential {
			t.Fatalf("message identity = (%q, %q)", message.GetRunnerId(), message.GetCredential())
		}
	}
	if service.stream.sent[0].GetOfferAccepted().GetJobId() != "job-1" {
		t.Fatalf("accepted job = %q", service.stream.sent[0].GetOfferAccepted().GetJobId())
	}
	if service.stream.sent[1].GetLeaseRenewal().GetLeaseGeneration() != 1 {
		t.Fatalf("lease generation = %d", service.stream.sent[1].GetLeaseRenewal().GetLeaseGeneration())
	}
	if service.stream.sent[2].GetEvent().GetExecutionId() != "execution-1" {
		t.Fatalf("execution event = %#v", service.stream.sent[2].GetEvent())
	}
	response, err := client.Receive()
	if err != nil {
		t.Fatalf("Receive() error = %v", err)
	}
	if response.GetLeaseAcknowledged().GetJobId() != "job-1" {
		t.Fatalf("acknowledged job = %q", response.GetLeaseAcknowledged().GetJobId())
	}
}

func TestClientDisconnectCancelsStreamContext(t *testing.T) {
	service := &fakeService{stream: &fakeStream{}}
	client, err := NewClient(service, Identity{RunnerID: "runner-1", Credential: "credential-1"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.Connect(context.Background()); err != nil {
		t.Fatal(err)
	}
	client.Disconnect()
	select {
	case <-service.context.Done():
	case <-time.After(time.Second):
		t.Fatal("Disconnect() did not cancel the stream context")
	}
}

func TestClientRejectsInvalidOrDisconnectedOperations(t *testing.T) {
	service := &fakeService{stream: &fakeStream{}}
	client, err := NewClient(service, Identity{RunnerID: "runner-1", Credential: "credential-1"})
	if err != nil {
		t.Fatalf("NewClient() error = %v", err)
	}
	if err := client.Heartbeat(nil, false); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("Heartbeat() error = %v, want ErrNotConnected", err)
	}
	if err := client.AcceptOffer(""); err == nil {
		t.Fatal("AcceptOffer() accepted an empty job ID")
	}
	if err := client.RejectOffer("job-1", string(make([]byte, 1025))); err == nil {
		t.Fatal("RejectOffer() accepted an oversized reason")
	}
	if err := client.RenewLease("job-1", 0, time.Now().Add(time.Minute)); err == nil {
		t.Fatal("RenewLease() accepted an invalid generation")
	}
}
