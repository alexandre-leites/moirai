package control

import (
	"testing"
	"time"
)

func TestEventReporterFlushWaitsForInFlight(t *testing.T) {
	client := &blockingEventClient{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	reporter := newEventReporter(t, client, 1)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatal(err)
	}

	// Trigger async send
	_, _ = reporter.Emit(lease.JobID, lease.Generation, "started", nil)
	<-client.started // Wait for send to start

	// Call Flush, it should block
	flushed := make(chan struct{})
	go func() {
		_ = reporter.Flush()
		close(flushed)
	}()

	select {
	case <-flushed:
		t.Fatal("Flush() returned before release")
	case <-time.After(10 * time.Millisecond):
		// Expected to block
	}

	close(client.release) // Finish send

	select {
	case <-flushed:
		// Success
	case <-time.After(100 * time.Millisecond):
		t.Fatal("Flush() timed out")
	}
}
