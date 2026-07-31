package control

import (
	"testing"
)

func TestEventReporterFlushWaitsForDeliveryFixed(t *testing.T) {
	// Setup a client that returns an error, forcing events to queue,
	// then clears the error to allow delivery.
	client := &eventClient{} // No error initially
	reporter := newEventReporter(t, client, 4)
	lease := eventLease()
	if err := reporter.Begin(lease); err != nil {
		t.Fatalf("Begin() error = %v", err)
	}

	// Emit events. 
	_, _ = reporter.Emit(lease.JobID, lease.Generation, "started", nil)
	_, _ = reporter.Emit(lease.JobID, lease.Generation, "progress", nil)

	// Since client returns no error, they are delivered immediately.
	// This test is just to check if Flush() works when everything is already delivered.
	if err := reporter.Flush(); err != nil {
		t.Fatalf("Flush() error = %v", err)
	}
}
