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

type OfferState struct {
	client        OfferClient
	now           func() time.Time
	leaseDuration time.Duration
	renewalLead   time.Duration

	mu               sync.Mutex
	pending          *pendingOffer
	active           *Lease
	renewalRequested bool
}

func NewOfferState(client OfferClient, now func() time.Time, leaseDuration, renewalLead time.Duration) (*OfferState, error) {
	if client == nil || now == nil || leaseDuration <= 0 || renewalLead <= 0 || renewalLead >= leaseDuration {
		return nil, ErrOfferStateConfiguration
	}
	return &OfferState{
		client:        client,
		now:           now,
		leaseDuration: leaseDuration,
		renewalLead:   renewalLead,
	}, nil
}

func (s *OfferState) Admit(offer *runnerv1.JobOffer) (bool, error) {
	if offer == nil || offer.GetJobId() == "" || offer.GetLeaseGeneration() < 1 {
		return false, errors.New("job offer is invalid")
	}

	s.mu.Lock()
	busy := s.pending != nil || s.active != nil
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
	if s.pending != nil || s.active != nil {
		s.mu.Unlock()
		return false, s.client.RejectOffer(offer.GetJobId(), "runner is busy")
	}
	s.pending = reservation
	s.mu.Unlock()

	if err := s.client.AcceptOffer(offer.GetJobId()); err != nil {
		s.mu.Lock()
		if s.pending == reservation {
			s.pending = nil
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

	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil && s.pending.jobID == acknowledgement.GetJobId() && s.pending.generation == acknowledgement.GetLeaseGeneration() {
		s.active = &Lease{
			JobID:      s.pending.jobID,
			Generation: s.pending.generation,
			Packet:     s.pending.packet,
			ExpiresAt:  expiresAt,
		}
		s.pending = nil
		s.renewalRequested = false
		return true
	}
	if s.active == nil || s.active.JobID != acknowledgement.GetJobId() || s.active.Generation != acknowledgement.GetLeaseGeneration() || !expiresAt.After(s.active.ExpiresAt) {
		return false
	}
	s.active.ExpiresAt = expiresAt
	s.renewalRequested = false
	return true
}

func (s *OfferState) RenewDue() (bool, error) {
	now := s.now()
	s.mu.Lock()
	if s.active == nil || s.renewalRequested || now.Before(s.active.ExpiresAt.Add(-s.renewalLead)) {
		s.mu.Unlock()
		return false, nil
	}
	if !now.Before(s.active.ExpiresAt) {
		s.active = nil
		s.renewalRequested = false
		s.mu.Unlock()
		return false, nil
	}
	lease := *s.active
	s.mu.Unlock()

	if err := s.client.RenewLease(lease.JobID, lease.Generation, now.Add(s.leaseDuration)); err != nil {
		return false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active != nil && s.active.JobID == lease.JobID && s.active.Generation == lease.Generation {
		s.renewalRequested = true
		return true, nil
	}
	return false, nil
}

func (s *OfferState) Abandon(jobID string, generation int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil && s.pending.jobID == jobID && s.pending.generation == generation {
		s.pending = nil
		return true
	}
	if s.active != nil && s.active.JobID == jobID && s.active.Generation == generation {
		s.active = nil
		s.renewalRequested = false
		return true
	}
	return false
}

func (s *OfferState) Expire() *Lease {
	now := s.now()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil || now.Before(s.active.ExpiresAt) {
		return nil
	}
	lease := *s.active
	s.active = nil
	s.renewalRequested = false
	return &lease
}

func (s *OfferState) ActiveLease() (Lease, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.active == nil {
		return Lease{}, false
	}
	return *s.active, true
}
