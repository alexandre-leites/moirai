package control

import (
	"encoding/json"
	"errors"
	"testing"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

type offerClient struct {
	accepted []string
	rejected []string
	renewed  []renewal
	err      error
	onAccept func()
}

type renewal struct {
	jobID      string
	generation int64
	expiresAt  time.Time
}

func (c *offerClient) AcceptOffer(jobID string) error {
	if c.onAccept != nil {
		c.onAccept()
	}
	if c.err != nil {
		return c.err
	}
	c.accepted = append(c.accepted, jobID)
	return nil
}

func (c *offerClient) RejectOffer(jobID, reason string) error {
	if c.err != nil {
		return c.err
	}
	c.rejected = append(c.rejected, jobID+":"+reason)
	return nil
}

func (c *offerClient) RenewLease(jobID string, generation int64, expiresAt time.Time) error {
	if c.err != nil {
		return c.err
	}
	c.renewed = append(c.renewed, renewal{jobID: jobID, generation: generation, expiresAt: expiresAt})
	return nil
}

func TestOfferStateAdmitsOneSafeOfferAndAppliesAuthoritativeAcknowledgement(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	offer := validOffer(t, "job-1", 2)

	admitted, err := state.Admit(offer)
	if err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v), want (true, nil)", admitted, err)
	}
	if len(client.accepted) != 1 || client.accepted[0] != "job-1" {
		t.Fatalf("accepted = %#v", client.accepted)
	}
	if _, active := state.ActiveLease("job-1"); active {
		t.Fatal("offer became active before acknowledgement")
	}

	expiresAt := now.Add(time.Minute)
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 2, ExpiresAtUnixMs: expiresAt.UnixMilli()}) {
		t.Fatal("ApplyAcknowledgement() rejected matching acknowledgement")
	}
	lease, active := state.ActiveLease("job-1")
	if !active || lease.Generation != 2 || !lease.ExpiresAt.Equal(expiresAt) || lease.Packet.JobID != "job-1" {
		t.Fatalf("active lease = %#v, %v", lease, active)
	}
}

func TestOfferStateRejectsInvalidAndConcurrentOffers(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	invalid := validOffer(t, "job-invalid", 1)
	invalid.TaskPacketJson = `{"jobId":"job-invalid"}`

	admitted, err := state.Admit(invalid)
	if err != nil || admitted {
		t.Fatalf("invalid Admit() = (%v, %v)", admitted, err)
	}
	if len(client.rejected) != 1 || client.rejected[0] != "job-invalid:task packet is invalid" {
		t.Fatalf("rejections = %#v", client.rejected)
	}
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("first Admit() = (%v, %v)", admitted, err)
	}
	if admitted, err := state.Admit(validOffer(t, "job-2", 1)); err != nil || admitted {
		t.Fatalf("second Admit() = (%v, %v)", admitted, err)
	}
	if len(client.accepted) != 1 || len(client.rejected) != 2 || client.rejected[1] != "job-2:runner is busy" {
		t.Fatalf("accepted = %#v, rejections = %#v", client.accepted, client.rejected)
	}
}

func TestOfferStateIgnoresStaleAcknowledgementsAndAbandonsLostLease(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	if admitted, err := state.Admit(validOffer(t, "job-1", 3)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}
	expiresAt := now.Add(time.Minute)
	if state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 2, ExpiresAtUnixMs: expiresAt.UnixMilli()}) {
		t.Fatal("accepted stale generation")
	}
	if state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 3, ExpiresAtUnixMs: now.UnixMilli()}) {
		t.Fatal("accepted expired lease")
	}
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 3, ExpiresAtUnixMs: expiresAt.UnixMilli()}) {
		t.Fatal("rejected valid acknowledgement")
	}
	if state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 3, ExpiresAtUnixMs: expiresAt.Add(-time.Second).UnixMilli()}) {
		t.Fatal("accepted stale expiry acknowledgement")
	}
	if !state.Abandon("job-1", 3) {
		t.Fatal("Abandon() did not clear matching active lease")
	}
	if _, active := state.ActiveLease("job-1"); active {
		t.Fatal("lease remained active after abandonment")
	}
}

func TestOfferStateRenewsOncePerAcknowledgementAndExpires(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}
	expiresAt := now.Add(time.Minute)
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 1, ExpiresAtUnixMs: expiresAt.UnixMilli()}) {
		t.Fatal("ApplyAcknowledgement() rejected lease")
	}
	now = now.Add(44 * time.Second)
	if renewed, err := state.RenewDue(); err != nil || len(renewed) != 0 {
		t.Fatalf("early RenewDue() = (%v, %v)", renewed, err)
	}
	now = now.Add(time.Second)
	renewed, err := state.RenewDue()
	if err != nil || len(renewed) != 1 || renewed[0] != "job-1" {
		t.Fatalf("due RenewDue() = (%v, %v)", renewed, err)
	}
	if len(client.renewed) != 1 || !client.renewed[0].expiresAt.Equal(now.Add(time.Minute)) {
		t.Fatalf("renewals = %#v", client.renewed)
	}
	if renewed, err := state.RenewDue(); err != nil || len(renewed) != 0 {
		t.Fatalf("duplicate RenewDue() = (%v, %v)", renewed, err)
	}
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 1, ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}) {
		t.Fatal("renewal acknowledgement was rejected")
	}
	now = now.Add(time.Minute)
	expired := state.Expire()
	if len(expired) != 1 || expired[0].JobID != "job-1" {
		t.Fatalf("Expire() = %#v", expired)
	}
	if _, active := state.ActiveLease("job-1"); active {
		t.Fatal("expired lease remained active")
	}
}

func TestOfferStateRenewDueLeavesExpiredLeaseForExpiryHandling(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	state := newOfferState(t, &offerClient{}, &now)
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 1, ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}) {
		t.Fatal("ApplyAcknowledgement() rejected lease")
	}
	now = now.Add(time.Minute)
	if renewed, err := state.RenewDue(); err != nil || len(renewed) != 0 {
		t.Fatalf("RenewDue() = (%v, %v)", renewed, err)
	}
	if _, active := state.ActiveLease("job-1"); !active {
		t.Fatal("RenewDue() removed expired lease before expiry handling")
	}
}

func TestOfferStatePropagatesControlFailures(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	client := &offerClient{err: errors.New("disconnected")}
	state := newOfferState(t, client, &now)
	if _, err := state.Admit(validOffer(t, "job-1", 1)); !errors.Is(err, client.err) {
		t.Fatalf("Admit() error = %v", err)
	}
	client.err = nil
	if admitted, err := state.Admit(validOffer(t, "job-2", 1)); err != nil || !admitted {
		t.Fatalf("retry Admit() = (%v, %v), want (true, nil)", admitted, err)
	}
}

func TestOfferStateAcceptsOutsideStateLock(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	client.onAccept = func() {
		if _, active := state.ActiveLease("job-1"); active {
			t.Fatal("lease became active before acknowledgement")
		}
	}
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v), want (true, nil)", admitted, err)
	}
}

func TestOfferStateWithCapacityAdmitsConcurrentOffersUpToCapacity(t *testing.T) {
	now := time.Date(2026, 7, 24, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state, err := NewOfferStateWithCapacity(client, func() time.Time { return now }, time.Minute, 15*time.Second, 2)
	if err != nil {
		t.Fatalf("NewOfferStateWithCapacity() error = %v", err)
	}

	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("first Admit() = (%v, %v)", admitted, err)
	}
	if admitted, err := state.Admit(validOffer(t, "job-2", 1)); err != nil || !admitted {
		t.Fatalf("second Admit() = (%v, %v)", admitted, err)
	}
	if admitted, err := state.Admit(validOffer(t, "job-3", 1)); err != nil || admitted {
		t.Fatalf("third Admit() = (%v, %v), want rejected at capacity", admitted, err)
	}
	if state.ActiveCount() != 0 {
		t.Fatalf("ActiveCount() = %d before acknowledgement", state.ActiveCount())
	}

	expiresAt := now.Add(time.Minute)
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 1, ExpiresAtUnixMs: expiresAt.UnixMilli()}) {
		t.Fatal("ApplyAcknowledgement() rejected job-1")
	}
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-2", LeaseGeneration: 1, ExpiresAtUnixMs: expiresAt.UnixMilli()}) {
		t.Fatal("ApplyAcknowledgement() rejected job-2")
	}
	if state.ActiveCount() != 2 {
		t.Fatalf("ActiveCount() = %d, want 2 concurrent leases", state.ActiveCount())
	}
	if _, active := state.ActiveLease("job-1"); !active {
		t.Fatal("job-1 is not active")
	}
	if _, active := state.ActiveLease("job-2"); !active {
		t.Fatal("job-2 is not active")
	}
}

func TestOfferStateExpiresUnacknowledgedReservationAndFreesCapacity(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}
	if state.ReservedCount() != 1 || state.ActiveCount() != 0 {
		t.Fatalf("reserved = %d, active = %d, want an unacknowledged reservation holding the slot", state.ReservedCount(), state.ActiveCount())
	}

	now = now.Add(DefaultOfferTimeout - time.Second)
	if expired := state.ExpirePending(DefaultOfferTimeout); len(expired) != 0 {
		t.Fatalf("ExpirePending() = %#v before the timeout elapsed", expired)
	}

	now = now.Add(time.Second)
	expired := state.ExpirePending(DefaultOfferTimeout)
	if len(expired) != 1 || expired[0].JobID != "job-1" || expired[0].Generation != 1 || expired[0].ExecutionID != "execution-1" {
		t.Fatalf("ExpirePending() = %#v", expired)
	}
	if state.ReservedCount() != 0 {
		t.Fatalf("ReservedCount() = %d after expiry", state.ReservedCount())
	}
	if admitted, err := state.Admit(validOffer(t, "job-2", 1)); err != nil || !admitted {
		t.Fatalf("Admit() after expiry = (%v, %v), want the freed slot reused", admitted, err)
	}
	if len(client.rejected) != 0 {
		t.Fatalf("rejected = %#v, want expiry handled locally without an offer rejection", client.rejected)
	}
}

func TestOfferStateRejectsAcknowledgementForExpiredReservation(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	if admitted, err := state.Admit(validOffer(t, "job-1", 4)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}
	now = now.Add(DefaultOfferTimeout)
	if expired := state.ExpirePending(DefaultOfferTimeout); len(expired) != 1 {
		t.Fatalf("ExpirePending() = %#v", expired)
	}

	acknowledgement := &runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 4, ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}
	if state.ApplyAcknowledgement(acknowledgement) {
		t.Fatal("acknowledgement resurrected an expired reservation")
	}
	if state.ReservedCount() != 0 || state.ActiveCount() != 0 {
		t.Fatalf("reserved = %d, active = %d after a stale acknowledgement", state.ReservedCount(), state.ActiveCount())
	}
	if _, active := state.ActiveLease("job-1"); active {
		t.Fatal("expired reservation became an active lease")
	}
}

func TestOfferStateKeepsAcknowledgedLeaseThroughPendingExpirySweep(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-1", LeaseGeneration: 1, ExpiresAtUnixMs: now.Add(time.Hour).UnixMilli()}) {
		t.Fatal("ApplyAcknowledgement() rejected the normal acknowledgement path")
	}

	now = now.Add(10 * DefaultOfferTimeout)
	if expired := state.ExpirePending(DefaultOfferTimeout); len(expired) != 0 {
		t.Fatalf("ExpirePending() = %#v, want acknowledged leases untouched", expired)
	}
	if _, active := state.ActiveLease("job-1"); !active {
		t.Fatal("pending expiry removed an acknowledged lease")
	}
}

func TestOfferStateExpiresOnlyTheReservationsPastTheTimeout(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state, err := NewOfferStateWithCapacity(client, func() time.Time { return now }, time.Minute, 15*time.Second, 3)
	if err != nil {
		t.Fatalf("NewOfferStateWithCapacity() error = %v", err)
	}
	if admitted, err := state.Admit(validOffer(t, "job-old", 1)); err != nil || !admitted {
		t.Fatalf("Admit(job-old) = (%v, %v)", admitted, err)
	}
	now = now.Add(20 * time.Second)
	if admitted, err := state.Admit(validOffer(t, "job-new", 1)); err != nil || !admitted {
		t.Fatalf("Admit(job-new) = (%v, %v)", admitted, err)
	}

	now = now.Add(11 * time.Second)
	expired := state.ExpirePending(DefaultOfferTimeout)
	if len(expired) != 1 || expired[0].JobID != "job-old" {
		t.Fatalf("ExpirePending() = %#v, want only the reservation past the timeout", expired)
	}
	if state.ReservedCount() != 1 {
		t.Fatalf("ReservedCount() = %d, want the younger reservation kept", state.ReservedCount())
	}
	if !state.ApplyAcknowledgement(&runnerv1.LeaseAcknowledged{JobId: "job-new", LeaseGeneration: 1, ExpiresAtUnixMs: now.Add(time.Minute).UnixMilli()}) {
		t.Fatal("the surviving reservation could no longer be acknowledged")
	}
}

func TestOfferStateStartsTheAcknowledgementWaitWhenTheAcceptanceIsSent(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	// A control-stream send that blocks must not spend the reservation's own
	// wait: the acknowledgement cannot arrive until the acceptance lands.
	client.onAccept = func() { now = now.Add(20 * time.Second) }
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}

	now = now.Add(DefaultOfferTimeout - time.Second)
	if expired := state.ExpirePending(DefaultOfferTimeout); len(expired) != 0 {
		t.Fatalf("ExpirePending() = %#v, want the wait measured from the acceptance", expired)
	}
	now = now.Add(time.Second)
	if expired := state.ExpirePending(DefaultOfferTimeout); len(expired) != 1 {
		t.Fatalf("ExpirePending() = %#v, want expiry a full timeout after the acceptance", expired)
	}
}

func TestOfferStateExpirePendingFallsBackToTheDefaultTimeout(t *testing.T) {
	now := time.Date(2026, 7, 29, 12, 0, 0, 0, time.UTC)
	client := &offerClient{}
	state := newOfferState(t, client, &now)
	if admitted, err := state.Admit(validOffer(t, "job-1", 1)); err != nil || !admitted {
		t.Fatalf("Admit() = (%v, %v)", admitted, err)
	}
	now = now.Add(DefaultOfferTimeout)
	if expired := state.ExpirePending(0); len(expired) != 1 {
		t.Fatalf("ExpirePending(0) = %#v, want the default timeout applied instead of no expiry", expired)
	}
}

func newOfferState(t *testing.T, client OfferClient, now *time.Time) *OfferState {
	t.Helper()
	state, err := NewOfferState(client, func() time.Time { return *now }, time.Minute, 15*time.Second)
	if err != nil {
		t.Fatalf("NewOfferState() error = %v", err)
	}
	return state
}

func validOffer(t *testing.T, jobID string, generation int64) *runnerv1.JobOffer {
	t.Helper()
	packet := taskpacket.Packet{
		ProtocolVersion: taskpacket.ProtocolVersion,
		JobID:           jobID,
		ExecutionID:     "execution-1",
		Role:            taskpacket.RoleDeveloper,
		Objective:       "Implement issue",
		Issue:           taskpacket.Issue{ExternalID: "7", Title: "Title", Body: "Body"},
		Repository: taskpacket.Repository{
			ProjectID:     "project-1",
			Mode:          "managed_clone",
			URL:           "git@example.com:project/repository.git",
			DefaultBranch: "main",
			Branch:        "agent/issue-7/run-1",
		},
		PromptPath:     ".loop/prompt.md",
		ExpectedOutput: ".loop/result.json",
		TimeoutSeconds: 600,
		Constraints:    taskpacket.Constraints{MayModifyFiles: true},
	}
	contents, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	return &runnerv1.JobOffer{JobId: jobID, LeaseGeneration: generation, TaskPacketJson: string(contents)}
}
