package dispatch

import (
	"context"
	"sync"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/control"
)

// offerExpiryClock is a race-safe manual clock. The control loop reads the
// clock from its own goroutines, so the test cannot advance a bare time.Time.
type offerExpiryClock struct {
	mu      sync.Mutex
	current time.Time
}

func (clock *offerExpiryClock) Now() time.Time {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	return clock.current
}

func (clock *offerExpiryClock) Advance(delta time.Duration) {
	clock.mu.Lock()
	defer clock.mu.Unlock()
	clock.current = clock.current.Add(delta)
}

func handleOffer(t *testing.T, loop *ControlLoop, offer *runnerv1.JobOffer) {
	t.Helper()
	message := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_Offer{Offer: offer}}
	if err := loop.Handle(context.Background(), message); err != nil {
		t.Fatalf("Handle(offer %s) error = %v", offer.GetJobId(), err)
	}
}

func offerResponses(t *testing.T, client *loopClient) (accepted, rejected []string) {
	t.Helper()
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]string(nil), client.accepted...), append([]string(nil), client.rejected...)
}

// TestControlLoopReleasesCapacityWhenOfferIsNeverAcknowledged is the issue #95
// acceptance criterion: with capacity 1, an offer whose acknowledgement never
// arrives must leave the runner accepting new offers within OfferTimeout, and
// the heartbeat must not advertise the runner as idle while that reservation
// still holds the slot.
func TestControlLoopReleasesCapacityWhenOfferIsNeverAcknowledged(t *testing.T) {
	client := &loopClient{}
	clock := &offerExpiryClock{current: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)}
	loop, err := NewControlLoopWithOutbox(client, &staticDispatcher{}, clock.Now, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}

	handleOffer(t, loop, loopOfferFor(t, "job-1", "execution-1", "project-1"))
	handleOffer(t, loop, loopOfferFor(t, "job-2", "execution-2", "project-2"))
	accepted, rejected := offerResponses(t, client)
	if len(accepted) != 1 || accepted[0] != "job-1" {
		t.Fatalf("accepted = %#v, want only job-1", accepted)
	}
	if len(rejected) != 1 || rejected[0] != "job-2:runner is busy" {
		t.Fatalf("rejected = %#v, want job-2 rejected as busy", rejected)
	}
	if !loop.Busy() {
		t.Fatal("heartbeat advertises the runner as idle while an unacknowledged reservation holds the only capacity slot")
	}

	clock.Advance(control.DefaultOfferTimeout + time.Second)
	if err := loop.Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	if loop.Busy() {
		t.Fatal("runner still reports busy after the reservation timed out")
	}
	handleOffer(t, loop, loopOfferFor(t, "job-3", "execution-3", "project-3"))
	accepted, rejected = offerResponses(t, client)
	if len(accepted) != 2 || accepted[1] != "job-3" {
		t.Fatalf("accepted = %#v, want job-3 admitted after the reservation timed out", accepted)
	}
	if len(rejected) != 1 {
		t.Fatalf("rejected = %#v, want no further rejection", rejected)
	}
}

// TestControlLoopHonorsConfiguredOfferTimeout proves the second acceptance
// criterion end to end: the value main.go copies out of
// config.Config.OfferTimeout, not the built-in default, decides when a
// reservation is released.
func TestControlLoopHonorsConfiguredOfferTimeout(t *testing.T) {
	client := &loopClient{}
	clock := &offerExpiryClock{current: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)}
	loop, err := NewControlLoopWithOutbox(client, &staticDispatcher{}, clock.Now, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}
	loop.OfferTimeout = 5 * time.Minute

	handleOffer(t, loop, loopOfferFor(t, "job-1", "execution-1", "project-1"))
	clock.Advance(control.DefaultOfferTimeout + time.Second)
	if err := loop.Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if !loop.Busy() {
		t.Fatal("reservation was released at the default timeout instead of the configured one")
	}

	clock.Advance(5 * time.Minute)
	if err := loop.Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
	if loop.Busy() {
		t.Fatal("reservation outlived the configured offer timeout")
	}
}

// TestControlLoopRejectsLeaseAcknowledgementForExpiredOffer covers step 2 of
// the issue: once a reservation has timed out, a late acknowledgement for it is
// stale and must not resurrect the reservation or start an execution.
func TestControlLoopRejectsLeaseAcknowledgementForExpiredOffer(t *testing.T) {
	client := &loopClient{}
	dispatcher := &staticDispatcher{}
	clock := &offerExpiryClock{current: time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)}
	loop, err := NewControlLoopWithOutbox(client, dispatcher, clock.Now, time.Minute, 15*time.Second, nil, "")
	if err != nil {
		t.Fatalf("NewControlLoopWithOutbox() error = %v", err)
	}

	handleOffer(t, loop, loopOfferFor(t, "job-1", "execution-1", "project-1"))
	clock.Advance(control.DefaultOfferTimeout + time.Second)
	if err := loop.Reconcile(); err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	acknowledgement := &runnerv1.OrchestratorToRunner{Message: &runnerv1.OrchestratorToRunner_LeaseAcknowledged{
		LeaseAcknowledged: &runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 1, ExpiresAtUnixMs: clock.Now().Add(time.Minute).UnixMilli()},
	}}
	if err := loop.Handle(context.Background(), acknowledgement); err != nil {
		t.Fatalf("Handle(acknowledgement) error = %v", err)
	}

	if _, active := loop.Offers.ActiveLease("job-1"); active {
		t.Fatal("late acknowledgement resurrected an expired offer reservation")
	}
	if loop.Busy() {
		t.Fatal("late acknowledgement re-consumed the capacity slot")
	}
	dispatcher.mu.Lock()
	calls := dispatcher.calls
	dispatcher.mu.Unlock()
	if calls != 0 {
		t.Fatalf("dispatcher calls = %d, want no execution for a stale acknowledgement", calls)
	}
	_, rejected := offerResponses(t, client)
	if len(rejected) != 0 {
		t.Fatalf("rejected = %#v, want the expiry handled locally without an offer rejection", rejected)
	}
}
