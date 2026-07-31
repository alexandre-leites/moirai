package control

import (
	"errors"
	"testing"
	"time"
)

func TestEventReporterFlushWaitsForDeliveryFixed(t *testing.T) {
	// Setup a client that returns an error, forcing events to queue
	transportErr := errors.New("disconnected")
	client := &eventClient{err: transportErr}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}

	// Emit events while disconnected. They will be queued.
	_, _ = reporter.Emit(lease.JobID, lease.Generation, "started", nil)
	_, _ = reporter.Emit(lease.JobID, lease.Generation, "progress", nil)

	if reporter.Pending() != 2 {
		t.Fatalf("expected 2 pending events, got %d", reporter.Pending())
	}

	// Trigger async flush in background. It should block because r.sending is true.
	flushFinished := make(chan error, 1)
	go func() {
		flushFinished <- reporter.Flush()
	}()

	// Give flush a moment to start
	time.Sleep(50 * time.Millisecond)

	// Clear error to allow delivery
	client.err = nil

	// Now unblock the async flush by simulating a delivery
	// The reporter needs to be able to actually flush now
	// This might still block if flush() logic requires something else
	
	select {
	case err := <-flushFinished:
		if err != nil {
			t.Fatalf("Flush() error = %v", err)
		}
	case <-time.After(1 * time.Second):
		t.Fatal("Flush() timed out")
	}

	if reporter.Pending() != 0 {
		t.Fatalf("expected 0 pending events after Flush(), got %d", reporter.Pending())
	}
}
