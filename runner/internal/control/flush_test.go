package control

import (
	"errors"
	"testing"
	"time"
)

// Flush is a delivery barrier: it must not report success while another
// goroutine is still sending. Regression test for issue #152, where it returned
// nil immediately and callers shut the runner down with events still in flight.
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
	go func() {
		_, _ = reporter.Emit(lease.JobID, lease.Generation, "started", nil)
	}()
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

// The other half of the barrier: what Flush does with events that queued while
// the transport was down. It must surface the transport error rather than
// report a delivery that did not happen, leave the events queued for the next
// attempt, and drain them once the transport recovers.
func TestEventReporterFlushSurfacesTransportErrorAndDrainsOnRecovery(t *testing.T) {
	client := &eventClient{err: errors.New("disconnected")}
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}

	// Emit sends synchronously when nothing else is in flight, so both attempts
	// fail against the downed transport and the events stay queued.
	if _, err := reporter.Emit(lease.JobID, lease.Generation, "started", nil); err == nil {
		t.Fatal("Emit() reported success while the transport was down")
	}
	_, _ = reporter.Emit(lease.JobID, lease.Generation, "progress", nil)
	if pending := reporter.Pending(); pending != 2 {
		t.Fatalf("Pending() = %d, want 2 queued events", pending)
	}

	if err := reporter.Flush(); err == nil {
		t.Fatal("Flush() returned nil while the transport was down")
	}
	if pending := reporter.Pending(); pending != 2 {
		t.Fatalf("Pending() = %d after a failed Flush, want the events still queued", pending)
	}

	// No send is in flight at this point — Emit and Flush each return only after
	// their synchronous flush completes — so recovering the transport here does
	// not race the reporter.
	client.err = nil

	if err := reporter.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
	if pending := reporter.Pending(); pending != 0 {
		t.Fatalf("Pending() = %d after a successful Flush, want 0", pending)
	}
	if delivered := len(client.events); delivered != 2 {
		t.Fatalf("delivered %d events, want 2", delivered)
	}
}
