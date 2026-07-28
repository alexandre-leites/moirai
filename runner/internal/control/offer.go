package control

import (
	"errors"
	"sync"
	"time"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/taskpacket"
)

var ErrOfferStateConfiguration = errors.New("runner offer state configuration is invalid")

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

	reservation := &pendingOffer{jobID: offer.GetJobId(), generation: offer.GetLeaseGeneration(), packet: packet}
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
	return true, nil
}

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
	for jobID, entry := range s.active {
		if entry.renewalRequested || now.Before(entry.lease.ExpiresAt.Add(-s.renewalLead)) {
			continue
		}
		if !now.Before(entry.lease.ExpiresAt) {
			delete(s.active, jobID)
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

// Expire removes and returns every active lease that is past its expiry.
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

func (s *OfferState) Capacity() int {
	return s.capacity
}
