package orchestrator_test

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	"github.com/loop-engineering/api/internal/orchestrator"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

func TestMapErrorReturnsNilForNilInput(t *testing.T) {
	_ = orchestrator.ErrUnavailable
}

func TestWithSessionAddsOpaqueMetadataOnlyWhenPresent(t *testing.T) {
	ctx := orchestrator.WithSession(context.Background(), "session-token", "csrf-token")
	values, ok := metadata.FromOutgoingContext(ctx)
	if !ok || len(values.Get("x-loop-session")) != 1 || values.Get("x-loop-session")[0] != "session-token" {
		t.Fatalf("missing session metadata: %v", values)
	}
	if len(values.Get("x-loop-csrf")) != 1 || values.Get("x-loop-csrf")[0] != "csrf-token" {
		t.Fatalf("missing CSRF metadata: %v", values)
	}
	if _, ok := metadata.FromOutgoingContext(orchestrator.WithSession(context.Background(), "")); ok {
		t.Fatal("empty token must not add metadata")
	}
}

func TestDialRejectsEmptyEndpoint(t *testing.T) {
	_, err := orchestrator.Dial(context.Background(), "")
	if err == nil {
		t.Fatal("expected error for empty endpoint")
	}
}

func TestSentinelErrorsAreDefined(t *testing.T) {
	_ = orchestrator.ErrUnavailable
	_ = orchestrator.ErrUnauthorized
	_ = orchestrator.ErrForbidden
	_ = orchestrator.ErrInvalidInput
	_ = orchestrator.ErrNotFound
}

func TestMapStatusCodeToError(t *testing.T) {
	cases := []struct {
		code     codes.Code
		sentinel error
	}{
		{codes.Unauthenticated, orchestrator.ErrUnauthorized},
		{codes.PermissionDenied, orchestrator.ErrForbidden},
		{codes.InvalidArgument, orchestrator.ErrInvalidInput},
		{codes.FailedPrecondition, orchestrator.ErrInvalidInput},
		{codes.NotFound, orchestrator.ErrNotFound},
		{codes.Unavailable, orchestrator.ErrUnavailable},
	}
	for _, tc := range cases {
		err := orchestrator.MapStatusError(status.Error(tc.code, "test"))
		if err == nil {
			t.Fatalf("expected error for code %s", tc.code)
		}
		if !errors.Is(err, tc.sentinel) {
			t.Errorf("code %s: got %v, want errors.Is(%v)", tc.code, err, tc.sentinel)
		}
	}
}

func TestMapStatusCodeDeadlineExceeded(t *testing.T) {
	for _, code := range []codes.Code{codes.Canceled, codes.DeadlineExceeded} {
		err := orchestrator.MapStatusError(status.Error(code, "timeout"))
		if !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("code %s: expected context.DeadlineExceeded, got %v", code, err)
		}
	}
}

func TestClientHealthyReflectsConnectivityState(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	server := grpc.NewServer()
	go func() { _ = server.Serve(listener) }()
	defer server.Stop()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	client, err := orchestrator.Dial(ctx, listener.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer client.Close()

	if !client.Healthy() {
		t.Error("expected a freshly dialed client to report healthy")
	}
}
