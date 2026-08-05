package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sort"
	"strings"
	"sync"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
	"github.com/loop-engineering/runner/internal/metrics"
)

const maxExecutionEventPayloadBytes = 16 * 1024
const maxLogChunkBytes = 6 * 1024

var ErrEventReporterConfiguration = errors.New("runner event reporter configuration is invalid")
var ErrNoActiveEventLease = errors.New("runner event lease is not active")
var ErrStaleEventLease = errors.New("runner event lease is stale")
var ErrEventBufferFull = errors.New("execution event buffer is full")

var ErrEventOutboxUnavailable = errors.New("execution event outbox is unavailable")

var ErrInvalidExecutionEvent = errors.New("execution event is invalid")

func DropReason(err error) string {
	switch {
	case errors.Is(err, ErrEventBufferFull):
		return metrics.DropBufferFull
	case errors.Is(err, ErrNoActiveEventLease), errors.Is(err, ErrStaleEventLease):
		return metrics.DropNoLease
	case errors.Is(err, ErrEventOutboxUnavailable):
		return metrics.DropPersistFailed
	case errors.Is(err, ErrInvalidExecutionEvent):
		return metrics.DropInvalid
	default:
		return metrics.DropUnknown
	}
}

type EventClient interface {
	SendExecutionEvent(*runnerv1.ExecutionEvent) error
}

type leaseState struct {
	lease Lease
	next  int64
}

type EventReporter struct {
	client          EventClient
	maxPending      int
	redaction       *redactionSet
	outbox          *eventOutbox
	maxPayloadBytes int
	logChunkBytes   int

	Logger *slog.Logger

	Metrics *metrics.Recorder

	mu sync.Mutex
	// cond waits for sending to finish during Flush
	cond    *sync.Cond
	leases  map[string]*leaseState
	expired map[expiredLeaseKey]*leaseState
	pending []*runnerv1.ExecutionEvent
	sending bool
}

type expiredLeaseKey struct {
	jobID      string
	generation int64
}

func NewEventReporter(client EventClient, maxPending int, prefixes []string, outboxPath string) (*EventReporter, error) {
	return NewEventReporterWithLimits(client, maxPending, prefixes, outboxPath, maxExecutionEventPayloadBytes, maxLogChunkBytes)
}

func NewEventReporterWithLimits(client EventClient, maxPending int, prefixes []string, outboxPath string, maxPayloadBytes, logChunkBytes int) (*EventReporter, error) {
	if client == nil || maxPending < 1 || maxPayloadBytes < 1 || logChunkBytes < 1 || !validRedactionPrefixes(prefixes) {
		return nil, ErrEventReporterConfiguration
	}
	reporter := &EventReporter{
		client:          client,
		maxPending:      maxPending,
		redaction:       newRedactionSet(prefixes),
		leases:          map[string]*leaseState{},
		expired:         map[expiredLeaseKey]*leaseState{},
		maxPayloadBytes: maxPayloadBytes,
		logChunkBytes:   logChunkBytes,
	}
	reporter.cond = sync.NewCond(&reporter.mu)
	if _, err := marshalEventPayloadWithLimit(map[string]any{"message": "", "chunkIndex": int64(^uint64(0) >> 1), "chunkCount": int64(^uint64(0) >> 1)}, reporter.redaction, maxPayloadBytes); err != nil {
		return nil, ErrEventReporterConfiguration
	}
	if outboxPath == "" {
		return reporter, nil
	}
	outbox, err := newEventOutbox(outboxPath)
	if err != nil {
		return nil, err
	}
	pending, err := outbox.Load()
	if err != nil {
		quarantine := outbox.path + ".corrupt"
		if renameErr := os.Rename(outbox.path, quarantine); renameErr != nil {
			return nil, fmt.Errorf("quarantine corrupt event outbox: %w", renameErr)
		}
		slog.Warn("quarantined corrupt execution event outbox", "path", quarantine, "error", err)
	}
	if len(pending) > maxPending {
		return nil, errors.New("event outbox exceeds pending event limit")
	}
	reporter.outbox = outbox
	reporter.pending = pending
	reporter.publishPendingLocked()
	return reporter, nil
}

func (r *EventReporter) recorder() *metrics.Recorder {
	if r != nil && r.Metrics != nil {
		return r.Metrics
	}
	return metrics.Default()
}

func (r *EventReporter) PublishMetrics() {
	if r == nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	r.publishPendingLocked()
}

func (r *EventReporter) publishPendingLocked() {
	r.recorder().SetPendingEvents(len(r.pending))
}

func (r *EventReporter) Begin(lease Lease) error {
	if lease.JobID == "" || lease.Generation < 1 || lease.Packet.ExecutionID == "" {
		return errors.New("event lease is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.leases[lease.JobID]; ok {
		if existing.lease.Generation != lease.Generation {
			return ErrStaleEventLease
		}
		return nil
	}
	r.leases[lease.JobID] = &leaseState{lease: lease}
	return nil
}

func (r *EventReporter) Abandon(jobID string, generation int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.leases[jobID]
	if !ok || existing.lease.Generation != generation {
		return false
	}
	delete(r.leases, jobID)
	r.expired[expiredLeaseKey{jobID: jobID, generation: generation}] = existing
	queued := len(r.pending)
	r.pending = withoutNonTerminalJobEvents(r.pending, jobID)
	r.recorder().RecordEventsDropped(metrics.DropLeaseExpired, queued-len(r.pending))
	r.publishPendingLocked()
	if err := r.persistLocked(); err != nil {
		r.logger().Warn("could not rewrite the execution event outbox after lease expiry", "job_id", jobID, "lease_generation", generation, "error", err)
	}
	return true
}

func (r *EventReporter) RecoverTerminal(lease Lease, eventType string, payload map[string]any) (int64, error) {
	if err := r.Begin(lease); err != nil {
		return 0, err
	}
	r.mu.Lock()
	r.leases[lease.JobID].next = int64(^uint64(0)>>1) - 1
	r.mu.Unlock()
	sequence, err := r.Emit(lease.JobID, lease.Generation, eventType, payload)
	r.Finish(lease.JobID, lease.Generation)
	return sequence, err
}

func (r *EventReporter) Finish(jobID string, generation int64) bool {
	// The job is over on every path through here, including the ones that
	// report no matching lease, so its credentials stop being redacted only
	// once nothing more can be emitted for it.
	r.redaction.release(jobID)
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, ok := r.expired[expiredLeaseKey{jobID: jobID, generation: generation}]; ok {
		delete(r.expired, expiredLeaseKey{jobID: jobID, generation: generation})
		return true
	}
	existing, ok := r.leases[jobID]
	if !ok || existing.lease.Generation != generation {
		return false
	}
	delete(r.leases, jobID)
	return true
}

func (r *EventReporter) Emit(jobID string, generation int64, eventType string, payload map[string]any) (int64, error) {
	if !validEventType(eventType) {
		return 0, fmt.Errorf("%w: type %q", ErrInvalidExecutionEvent, eventType)
	}
	contents, err := marshalEventPayloadWithLimit(payload, r.redaction, r.maxPayloadBytes)
	if err != nil {
		return 0, err
	}

	r.mu.Lock()
	state, err := r.emitStateLocked(jobID, generation, eventType)
	if err != nil {
		r.mu.Unlock()
		return 0, err
	}
	var evicted *runnerv1.ExecutionEvent
	evictedIndex := -1
	if len(r.pending) >= r.maxPending {
		if evicted, evictedIndex = r.makeRoomLocked(eventType); evicted == nil {
			r.mu.Unlock()
			return 0, ErrEventBufferFull
		}
	}
	lease := state.lease
	previousNext := state.next
	sequence := previousNext + 1
	state.next = sequence
	event := &runnerv1.ExecutionEvent{
		JobId:           lease.JobID,
		ExecutionId:     lease.Packet.ExecutionID,
		LeaseGeneration: lease.Generation,
		EventSequence:   sequence,
		Type:            eventType,
		PayloadJson:     string(contents),
	}
	r.pending = append(r.pending, event)
	if err := r.persistLocked(); err != nil {
		r.pending = r.pending[:len(r.pending)-1]
		if evicted != nil {
			r.pending = append(r.pending[:evictedIndex:evictedIndex], append([]*runnerv1.ExecutionEvent{evicted}, r.pending[evictedIndex:]...)...)
		}
		state.next = previousNext
		r.publishPendingLocked()
		r.mu.Unlock()
		return 0, fmt.Errorf("%w: %w", ErrEventOutboxUnavailable, err)
	}
	r.publishPendingLocked()
	if evicted != nil {
		r.recorder().RecordEventDropped(metrics.DropEvicted)
		r.logger().Warn("discarded a queued execution event to make room for a higher-priority event",
			"job_id", evicted.GetJobId(),
			"execution_id", evicted.GetExecutionId(),
			"discarded_type", evicted.GetType(),
			"discarded_sequence", evicted.GetEventSequence(),
			"event_type", eventType,
		)
	}
	flush := !r.sending
	if flush {
		r.sending = true
	}
	r.mu.Unlock()
	if !flush {
		return event.EventSequence, nil
	}
	return event.EventSequence, r.flush()
}

func (r *EventReporter) emitStateLocked(jobID string, generation int64, eventType string) (*leaseState, error) {
	if state, ok := r.leases[jobID]; ok && state.lease.Generation == generation {
		return state, nil
	}
	if state, ok := r.expired[expiredLeaseKey{jobID: jobID, generation: generation}]; ok {
		if !IsTerminalEventType(eventType) {
			return nil, ErrNoActiveEventLease
		}
		return state, nil
	}
	if r.knowsJobLocked(jobID) {
		return nil, ErrStaleEventLease
	}
	return nil, ErrNoActiveEventLease
}

func (r *EventReporter) knowsJobLocked(jobID string) bool {
	if _, ok := r.leases[jobID]; ok {
		return true
	}
	for key := range r.expired {
		if key.jobID == jobID {
			return true
		}
	}
	return false
}

func (r *EventReporter) makeRoomLocked(eventType string) (*runnerv1.ExecutionEvent, int) {
	priority := eventPriority(eventType)
	victim := -1
	for index, queued := range r.pending {
		candidate := eventPriority(queued.GetType())
		if candidate >= priority {
			continue
		}
		if victim < 0 || candidate < eventPriority(r.pending[victim].GetType()) {
			victim = index
		}
	}
	if victim < 0 {
		return nil, -1
	}
	discarded := r.pending[victim]
	r.pending = append(r.pending[:victim:victim], r.pending[victim+1:]...)
	return discarded, victim
}

func IsTerminalEventType(eventType string) bool {
	switch eventType {
	case "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func eventPriority(eventType string) int {
	switch {
	case IsTerminalEventType(eventType):
		return 2
	case eventType == "started":
		return 1
	default:
		return 0
	}
}

func withoutNonTerminalJobEvents(events []*runnerv1.ExecutionEvent, jobID string) []*runnerv1.ExecutionEvent {
	if len(events) == 0 {
		return events
	}
	kept := make([]*runnerv1.ExecutionEvent, 0, len(events))
	for _, event := range events {
		if event.GetJobId() != jobID || IsTerminalEventType(event.GetType()) {
			kept = append(kept, event)
		}
	}
	return kept
}

func (r *EventReporter) logger() *slog.Logger {
	if r != nil && r.Logger != nil {
		return r.Logger
	}
	return slog.Default()
}

func (r *EventReporter) EmitLog(jobID string, generation int64, message string) ([]int64, error) {
	chunks := r.logChunks(message)
	sequences := make([]int64, 0, len(chunks))
	for index, chunk := range chunks {
		sequence, err := r.Emit(jobID, generation, "log", map[string]any{
			"message":    chunk,
			"chunkIndex": index,
			"chunkCount": len(chunks),
		})
		if err != nil {
			lost := len(chunks) - index
			if sequence > 0 {
				lost--
			}
			r.recorder().RecordEventsDropped(DropReason(err), lost)
			return sequences, err
		}
		sequences = append(sequences, sequence)
	}
	return sequences, nil
}

func (r *EventReporter) Flush() error {
	r.mu.Lock()
	if len(r.leases) == 0 && len(r.expired) == 0 && len(r.pending) == 0 {
		r.mu.Unlock()
		return ErrNoActiveEventLease
	}
	if r.sending {
		// Wait until sending is false
		for r.sending {
			r.cond.Wait()
		}
		r.mu.Unlock()
		return nil
	}
	r.sending = true
	r.mu.Unlock()
	return r.flush()
}

func (r *EventReporter) flush() error {
	defer func() {
		r.mu.Lock()
		r.sending = false
		r.cond.Broadcast()
		r.mu.Unlock()
	}()

	for {
		r.mu.Lock()
		if len(r.pending) == 0 {
			r.mu.Unlock()
			return nil
		}
		event := r.pending[0]
		r.mu.Unlock()

		if err := r.client.SendExecutionEvent(event); err != nil {
			return err
		}

		r.mu.Lock()
		if len(r.pending) > 0 && r.pending[0] == event {
			r.pending = r.pending[1:]
			r.publishPendingLocked()
			if err := r.persistLocked(); err != nil {
				r.mu.Unlock()
				return err
			}
		}
		r.mu.Unlock()
	}
}

func (r *EventReporter) persistLocked() error {
	if r.outbox == nil {
		return nil
	}
	return r.outbox.Save(r.pending)
}

func (r *EventReporter) Pending() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.pending)
}

func validEventType(eventType string) bool {
	switch eventType {
	case "started", "progress", "log", "completed", "failed", "cancelled":
		return true
	default:
		return false
	}
}

func (r *EventReporter) logChunks(message string) []string {
	chunks := make([]string, 0, len(message)/r.logChunkBytes+1)
	for len(message) > 0 {
		end := r.logChunkBytes
		if end > len(message) {
			end = len(message)
		}
		for end > 0 && end < len(message) && (message[end]&0xc0) == 0x80 {
			end--
		}
		if end == 0 {
			end = 1
			for end < len(message) && (message[end]&0xc0) == 0x80 {
				end++
			}
		}
		for end > 0 {
			chunk := message[:end]
			if _, err := marshalEventPayloadWithLimit(map[string]any{"message": chunk, "chunkIndex": int64(^uint64(0) >> 1), "chunkCount": int64(^uint64(0) >> 1)}, r.redaction, r.maxPayloadBytes); err == nil {
				chunks = append(chunks, chunk)
				message = message[end:]
				break
			}
			end--
			for end > 0 && end < len(message) && (message[end]&0xc0) == 0x80 {
				end--
			}
		}
		if end == 0 {
			return append(chunks, "")
		}
	}
	if len(chunks) == 0 {
		return []string{""}
	}
	return chunks
}

func marshalEventPayloadWithLimit(payload map[string]any, redaction *redactionSet, maxPayloadBytes int) ([]byte, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	prefixes, literals := redaction.snapshot()
	contents, err := json.Marshal(redactPayloadWithPrefixes(payload, prefixes, literals))
	if err != nil {
		return nil, fmt.Errorf("%w: encode payload: %w", ErrInvalidExecutionEvent, err)
	}
	if len(contents) > maxPayloadBytes {
		return nil, fmt.Errorf("%w: payload is %d bytes, over the %d byte limit", ErrInvalidExecutionEvent, len(contents), maxPayloadBytes)
	}
	return contents, nil
}

func redactPayloadWithPrefixes(value any, prefixes []string, literals []string) any {
	switch current := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(current))
		for key, item := range current {
			if sensitivePayloadKey(key) {
				redacted[key] = "[REDACTED]"
				continue
			}
			redacted[key] = redactPayloadWithPrefixes(item, prefixes, literals)
		}
		return redacted
	case []any:
		redacted := make([]any, len(current))
		for index, item := range current {
			redacted[index] = redactPayloadWithPrefixes(item, prefixes, literals)
		}
		return redacted
	case []string:
		redacted := make([]string, len(current))
		for index, item := range current {
			redacted[index] = redactKnownSecretValues(item, prefixes, literals)
		}
		return redacted
	case string:
		return redactKnownSecretValues(current, prefixes, literals)
	default:
		return current
	}
}

func validRedactionPrefixes(prefixes []string) bool {
	for _, prefix := range prefixes {
		if prefix == "" || strings.IndexFunc(prefix, func(r rune) bool { return r < 0x21 || r > 0x7e }) >= 0 {
			return false
		}
	}
	return true
}

func redactKnownSecretValues(value string, configured []string, literals []string) string {
	// Literals first, and by plain substring: a resolved credential is redacted
	// because it is *this deployment's* secret, not because it happens to look
	// like one. A provider key that matches no known prefix -- most of them --
	// is invisible to the scan below and would otherwise stream out verbatim
	// the moment a harness echoed its own configuration.
	for _, literal := range literals {
		value = strings.ReplaceAll(value, literal, "[REDACTED]")
	}
	prefixes := append([]string{"ghp_", "github_pat_", "glpat-", "sk-"}, configured...)
	for _, prefix := range prefixes {
		searched := 0
		for searched < len(value) {
			offset := strings.Index(value[searched:], prefix)
			if offset < 0 {
				break
			}
			start := searched + offset
			if start > 0 && secretTokenByte(value[start-1]) {
				searched = start + len(prefix)
				continue
			}
			end := start + len(prefix)
			for end < len(value) && secretTokenByte(value[end]) {
				end++
			}
			value = value[:start] + "[REDACTED]" + value[end:]
			searched = start + len("[REDACTED]")
		}
	}
	return value
}

func secretTokenByte(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= 'A' && character <= 'Z') ||
		(character >= '0' && character <= '9') || character == '_' || character == '-'
}

func sensitivePayloadKey(key string) bool {
	key = strings.ToLower(key)
	return strings.Contains(key, "token") || strings.Contains(key, "secret") || strings.Contains(key, "credential") || strings.Contains(key, "password") || strings.Contains(key, "authorization")
}

func splitUTF8Chunks(value string, maximum int) []string {
	if maximum < 1 {
		panic("maximum log chunk size must be positive")
	}
	if value == "" {
		return []string{""}
	}
	chunks := make([]string, 0, len(value)/maximum+1)
	for len(value) > maximum {
		end := maximum
		for end > 0 && (value[end]&0xc0) == 0x80 {
			end--
		}
		if end == 0 {
			end = maximum
			for end < len(value) && (value[end]&0xc0) == 0x80 {
				end++
			}
		}
		chunks = append(chunks, value[:end])
		value = value[end:]
	}
	return append(chunks, value)
}

// redactionSet is what must never appear in an event payload: the prefixes an
// operator configured, plus the literal credential values of the executions
// currently running.
//
// The literals are held rather than listed because they arrive with a job and
// have to be gone when it ends -- and because they are registered *before* the
// value can reach a harness, which is the only ordering that makes redaction
// mean anything. A value shorter than this minimum is not registered: redacting
// "1" or "true" would blank most of a log and hide nothing worth hiding.
const minimumRedactableSecret = 8

type redactionSet struct {
	mu       sync.RWMutex
	prefixes []string
	literals map[string]int
	byJob    map[string][]string
}

func newRedactionSet(prefixes []string) *redactionSet {
	return &redactionSet{
		prefixes: append([]string(nil), prefixes...),
		literals: map[string]int{},
		byJob:    map[string][]string{},
	}
}

func (s *redactionSet) snapshot() ([]string, []string) {
	if s == nil {
		return nil, nil
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	if len(s.literals) == 0 {
		return s.prefixes, nil
	}
	literals := make([]string, 0, len(s.literals))
	for literal := range s.literals {
		literals = append(literals, literal)
	}
	// Longest first: a refresh token that contains an access token as a prefix
	// must be redacted whole rather than leaving its tail in the log.
	sort.Slice(literals, func(i, j int) bool { return len(literals[i]) > len(literals[j]) })
	return s.prefixes, literals
}

// hold registers values against the job that resolved them. Refcounted:
// two concurrent executions may resolve the same deployment credential, and the
// first to finish must not un-redact it for the other.
func (s *redactionSet) hold(jobID string, values []string) {
	if s == nil || jobID == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, value := range values {
		if len(value) < minimumRedactableSecret {
			continue
		}
		s.literals[value]++
		s.byJob[jobID] = append(s.byJob[jobID], value)
	}
}

func (s *redactionSet) release(jobID string) {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	held := s.byJob[jobID]
	delete(s.byJob, jobID)
	for _, value := range held {
		if s.literals[value] <= 1 {
			delete(s.literals, value)
			continue
		}
		s.literals[value]--
	}
}

// RedactSecrets keeps a job's resolved credential values out of every event
// this reporter sends, until Finish reports that job over.
//
// Registered by the dispatcher the moment the values are resolved, which is
// before any of them can reach a harness -- redaction that starts after the
// agent has already echoed its configuration protects nothing. Release happens
// in Finish rather than when the execution returns, because the terminal event
// carrying the agent's own summary is built after that.
func (r *EventReporter) RedactSecrets(jobID string, values []string) {
	if r == nil {
		return
	}
	r.redaction.hold(jobID, values)
}
