package control

import (
	"errors"
	"sync"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

var ErrOfferStateConfiguration = errors.New("runner offer state configuration is invalid")

// DefaultOfferTimeout bounds how long an accepted offer may wait for its lease
// acknowledgement before the reservation is released. It matches the
// LOOP_RUNNER_OFFER_TIMEOUT default so an unconfigured runner still ages
// reservations out; callers that have the configured value pass it instead.
const DefaultOfferTimeout = 30 * time.Second

type OfferClient interface {
	AcceptOffer(string) error
	RejectOffer(string, string) error
	RenewLease(string, int64, time.Time) error
}

type Lease struct {
	JobID      string
	Generation int64
	Packet     taskpacket.Packet
	ExpiresAt  time.Time
}

type pendingOffer struct {
	jobID      string
	generation int64
	packet     taskpacket.Packet
	// reservedAt is when the acceptance reached the control stream, or when the
	// reservation was built while that send is still in flight. A reservation
	// has no lease yet, so it carries no orchestrator-issued expiry; the runner
	// ages it out against its own offer timeout instead.
	reservedAt time.Time
}

// Reservation is an offer the runner accepted and that is still waiting for the
// orchestrator's lease acknowledgement. It holds a capacity slot but has no
// execution, no lease, and no event stream.
type Reservation struct {
	JobID       string
	Generation  int64
	ExecutionID string
	ReservedAt  time.Time
}

type activeLease struct {
	lease            Lease
	renewalRequested bool
}

// OfferState tracks up to `capacity` concurrent job leases for a runner.
// Capacity 1 (the default) reproduces the original single-slot behavior.
type OfferState struct {
	client        OfferClient
	now           func() time.Time
	leaseDuration time.Duration
	renewalLead   time.Duration
	capacity      int

	mu      sync.Mutex
	pending map[string]*pendingOffer
	active  map[string]*activeLease
}

func NewOfferState(client OfferClient, now func() time.Time, leaseDuration, renewalLead time.Duration) (*OfferState, error) {
	return NewOfferStateWithCapacity(client, now, leaseDuration, renewalLead, 1)
}

func NewOfferStateWithCapacity(client OfferClient, now func() time.Time, leaseDuration, renewalLead time.Duration, capacity int) (*OfferState, error) {
	if client == nil || now == nil || leaseDuration <= 0 || renewalLead <= 0 || renewalLead >= leaseDuration || capacity < 1 {
		return nil, ErrOfferStateConfiguration
	}
	return &OfferState{
		client:        client,
		now:           now,
		leaseDuration: leaseDuration,
		renewalLead:   renewalLead,
		capacity:      capacity,
		pending:       map[string]*pendingOffer{},
		active:        map[string]*activeLease{},
	}, nil
}

func (s *OfferState) Admit(offer *runnerv1.JobOffer) (bool, error) {
	if offer == nil || offer.GetJobId() == "" || offer.GetLeaseGeneration() < 1 {
		return false, errors.New("job offer is invalid")
	}

	s.mu.Lock()
	busy := len(s.pending)+len(s.active) >= s.capacity
	s.mu.Unlock()
	if busy {
		return false, s.client.RejectOffer(offer.GetJobId(), "runner is busy")
	}

	packet, err := taskpacket.Parse([]byte(offer.GetTaskPacketJson()))
	if err != nil || packet.JobID != offer.GetJobId() {
		return false, s.client.RejectOffer(offer.GetJobId(), "task packet is invalid")
	}

	reservation := &pendingOffer{jobID: offer.GetJobId(), generation: offer.GetLeaseGeneration(), packet: packet, reservedAt: s.now()}
	s.mu.Lock()
	if len(s.pending)+len(s.active) >= s.capacity {
		s.mu.Unlock()
		return false, s.client.RejectOffer(offer.GetJobId(), "runner is busy")
	}
	s.pending[reservation.jobID] = reservation
	s.mu.Unlock()

	if err := s.client.AcceptOffer(offer.GetJobId()); err != nil {
		s.mu.Lock()
		if s.pending[reservation.jobID] == reservation {
			delete(s.pending, reservation.jobID)
		}
		s.mu.Unlock()
		return false, err
	}

	// The acknowledgement cannot arrive before the acceptance is on the wire,
	// so the wait starts here, not when the reservation was built: AcceptOffer
	// blocks on the control stream and a slow send must not spend the
	// reservation's own timeout. The reservation is still stamped at creation
	// so that an AcceptOffer which never returns cannot hold the slot forever
	// — that is the leak this expiry exists to prevent. The pointer identity
	// check keeps a send that outlived its own timeout from re-stamping a
	// newer reservation for the same job.
	s.mu.Lock()
	if s.pending[reservation.jobID] == reservation {
		reservation.reservedAt = s.now()
	}
	s.mu.Unlock()
	return true, nil
}

// ApplyAcknowledgement promotes a matching reservation to an active lease, or
// extends an existing lease, reporting whether the acknowledgement was applied.
// It never creates state it was not already holding, so an acknowledgement that
// arrives after ExpirePending released the reservation is stale and is rejected
// rather than resurrecting the slot.
func (s *OfferState) ApplyAcknowledgement(acknowledgement *runnerv1.LeaseAcknowledged) bool {
	if acknowledgement == nil || acknowledgement.GetJobId() == "" || acknowledgement.GetLeaseGeneration() < 1 {
		return false
	}
	expiresAt := time.UnixMilli(acknowledgement.GetExpiresAtUnixMs())
	if !expiresAt.After(s.now()) {
		return false
	}
	jobID := acknowledgement.GetJobId()

	s.mu.Lock()
	defer s.mu.Unlock()
	if pending, ok := s.pending[jobID]; ok && pending.generation == acknowledgement.GetLeaseGeneration() {
		s.active[jobID] = &activeLease{lease: Lease{
			JobID:      pending.jobID,
			Generation: pending.generation,
			Packet:     pending.packet,
			ExpiresAt:  expiresAt,
		}}
		delete(s.pending, jobID)
		return true
	}
	existing, ok := s.active[jobID]
	if !ok || existing.lease.Generation != acknowledgement.GetLeaseGeneration() || !expiresAt.After(existing.lease.ExpiresAt) {
		return false
	}
	existing.lease.ExpiresAt = expiresAt
	existing.renewalRequested = false
	return true
}

// RenewDue requests renewal for every active lease whose renewal window has
// arrived, returning the job IDs successfully renewed. It stops and returns
// the first error encountered, leaving leases not yet attempted untouched.
func (s *OfferState) RenewDue() ([]string, error) {
	now := s.now()
	var due []Lease
	s.mu.Lock()
	for _, entry := range s.active {
		if entry.renewalRequested || now.Before(entry.lease.ExpiresAt.Add(-s.renewalLead)) {
			continue
		}
		if !now.Before(entry.lease.ExpiresAt) {
			continue
		}
		due = append(due, entry.lease)
	}
	s.mu.Unlock()

	var renewed []string
	for _, lease := range due {
		if err := s.client.RenewLease(lease.JobID, lease.Generation, now.Add(s.leaseDuration)); err != nil {
			return renewed, err
		}
		s.mu.Lock()
		if entry, ok := s.active[lease.JobID]; ok && entry.lease.Generation == lease.Generation {
			entry.renewalRequested = true
			renewed = append(renewed, lease.JobID)
		}
		s.mu.Unlock()
	}
	return renewed, nil
}

func (s *OfferState) Abandon(jobID string, generation int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if pending, ok := s.pending[jobID]; ok && pending.generation == generation {
		delete(s.pending, jobID)
		return true
	}
	if entry, ok := s.active[jobID]; ok && entry.lease.Generation == generation {
		delete(s.active, jobID)
		return true
	}
	return false
}

// ExpirePending removes and returns every reservation that has waited longer
// than timeout for its lease acknowledgement. Without it a single lost
// acknowledgement — a withdrawn offer, a job handed to another runner, or a
// message dropped on a reconnect — holds a capacity slot forever, and Admit
// rejects every later offer as "runner is busy".
//
// A non-positive timeout falls back to DefaultOfferTimeout rather than
// disabling expiry, so a misconfigured caller cannot silently reinstate the
// leak.
//
// Expiry is deliberately local: the runner does not send an offer rejection.
// Once the acceptance is recorded the orchestrator's offer row is 'accepted',
// which its expire_offers sweep (status = 'offered' only) no longer matches, so
// a rejection for it is a FAILED_PRECONDITION that aborts the whole control
// stream. The job itself is reclaimed by the orchestrator's lease expiry
// against app.jobs.lease_expires_at, on the much longer scheduler offer TTL.
// Releasing the slot locally is therefore the only thing the runner can do,
// and the only thing it needs to do.
func (s *OfferState) ExpirePending(timeout time.Duration) []Reservation {
	if timeout <= 0 {
		timeout = DefaultOfferTimeout
	}
	deadline := s.now().Add(-timeout)
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []Reservation
	for jobID, reservation := range s.pending {
		if reservation.reservedAt.After(deadline) {
			continue
		}
		expired = append(expired, Reservation{
			JobID:       reservation.jobID,
			Generation:  reservation.generation,
			ExecutionID: reservation.packet.ExecutionID,
			ReservedAt:  reservation.reservedAt,
		})
		delete(s.pending, jobID)
	}
	return expired
}

// Expire removes and returns every active lease that is past its expiry.
// Reservations that have not been acknowledged yet carry no lease expiry and
// are aged out by ExpirePending instead.
func (s *OfferState) Expire() []Lease {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	var expired []Lease
	for jobID, entry := range s.active {
		if now.Before(entry.lease.ExpiresAt) {
			continue
		}
		expired = append(expired, entry.lease)
		delete(s.active, jobID)
	}
	return expired
}

func (s *OfferState) ActiveLease(jobID string) (Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.active[jobID]
	if !ok {
		return Lease{}, false
	}
	return entry.lease, true
}

func (s *OfferState) ActiveCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.active)
}

// ReservedCount reports how many capacity slots the runner currently holds:
// acknowledged leases plus offers accepted but not yet acknowledged. It is the
// quantity Admit compares against capacity, so callers that report availability
// must use it rather than ActiveCount.
func (s *OfferState) ReservedCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.pending) + len(s.active)
}

func (s *OfferState) Capacity() int {
	return s.capacity
}
