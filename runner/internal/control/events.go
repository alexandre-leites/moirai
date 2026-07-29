package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
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

type EventClient interface {
	SendExecutionEvent(*runnerv1.ExecutionEvent) error
}

// leaseState carries the per-lease event sequence counter alongside the lease
// itself so a lease retained after expiry keeps numbering where it left off.
type leaseState struct {
	lease Lease
	next  int64
}

// EventReporter tracks execution-event sequencing for every concurrently
// active lease on the runner, keyed by job ID. A single flat `pending` queue
// preserves send order across leases and shares one `maxPending` budget.
//
// Terminal events (`completed`, `failed`, `cancelled`) are the one class the
// orchestrator cannot reconstruct, so they are privileged over log noise in
// two ways: they may evict a queued lower-priority event when the buffer is
// full, and they survive lease expiry both in the queue (Abandon) and at the
// source (Emit against an expired lease retained by Abandon).
type EventReporter struct {
	client            EventClient
	maxPending        int
	redactionPrefixes []string
	outbox            *eventOutbox

	// Logger reports discarded events. Assign before the reporter is shared
	// with other goroutines; nil falls back to slog.Default().
	Logger *slog.Logger

	// Metrics counts the events this reporter discards and publishes the depth
	// of its pending queue. This reporter is the only component that can see
	// either — it owns the queue — so it is where both are recorded. Assign
	// before the reporter is shared with other goroutines; nil falls back to
	// the process-wide recorder the metrics server exports.
	Metrics *metrics.Recorder

	mu sync.Mutex
	// leases holds leases the runner currently owns, keyed by job ID.
	leases map[string]*leaseState
	// expired holds leases whose expiry was observed while their execution was
	// still running, retained only so that execution's terminal event can still
	// be sequenced, persisted, and delivered. Cleared by Finish. It is keyed by
	// generation as well as job ID: the orchestrator can re-offer a job at the
	// next generation while the superseded execution is still winding down, and
	// that execution still owes a terminal event.
	expired map[expiredLeaseKey]*leaseState
	pending []*runnerv1.ExecutionEvent
	sending bool
}

type expiredLeaseKey struct {
	jobID      string
	generation int64
}

func NewEventReporter(client EventClient, maxPending int, prefixes []string, outboxPath string) (*EventReporter, error) {
	if client == nil || maxPending < 1 || !validRedactionPrefixes(prefixes) {
		return nil, ErrEventReporterConfiguration
	}
	reporter := &EventReporter{
		client:            client,
		maxPending:        maxPending,
		redactionPrefixes: append([]string(nil), prefixes...),
		leases:            map[string]*leaseState{},
		expired:           map[expiredLeaseKey]*leaseState{},
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
		return nil, err
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

// publishPendingLocked republishes the queue depth from the queue itself, so
// the gauge cannot drift from what the reporter actually holds. It is called at
// every point the queue changes, with mu held (or before the reporter is
// shared). Setting a Prometheus gauge is an atomic store against a child
// resolved at recorder construction, so this adds no lock to the emit path.
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

// Abandon releases a lease the runner no longer owns, typically because it
// expired while the stream was down. Queued log and progress output for the
// job is discarded, but its terminal events are kept: they stay fenced by
// their lease generation, so the orchestrator can still decide what to do with
// them, and dropping them here would destroy the run's only record of its
// outcome. The lease is retained so a still-running execution can report its
// terminal event; Finish clears that record.
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

func (r *EventReporter) Finish(jobID string, generation int64) bool {
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

// Emit queues one execution event for delivery. Every path that returns
// without queueing has discarded the event, and each records why against the
// runner's dropped-event counter: this is the only place in the runner that can
// tell a rejected event from a delivered one, and the counter is what makes a
// loss visible to a scrape rather than only to whoever reads the log.
func (r *EventReporter) Emit(jobID string, generation int64, eventType string, payload map[string]any) (int64, error) {
	if !validEventType(eventType) {
		r.recorder().RecordEventDropped(metrics.DropInvalid)
		return 0, errors.New("execution event type is invalid")
	}
	contents, err := marshalEventPayloadWithPrefixes(payload, r.redactionPrefixes)
	if err != nil {
		r.recorder().RecordEventDropped(metrics.DropInvalid)
		return 0, err
	}

	r.mu.Lock()
	state, err := r.emitStateLocked(jobID, generation, eventType)
	if err != nil {
		r.mu.Unlock()
		r.recorder().RecordEventDropped(metrics.DropNoLease)
		return 0, err
	}
	var evicted *runnerv1.ExecutionEvent
	evictedIndex := -1
	if len(r.pending) >= r.maxPending {
		if evicted, evictedIndex = r.makeRoomLocked(eventType); evicted == nil {
			r.mu.Unlock()
			r.recorder().RecordEventDropped(metrics.DropBufferFull)
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
		// The trade never completed, so put the evicted event back rather than
		// losing it to an emit that produced nothing.
		r.pending = r.pending[:len(r.pending)-1]
		if evicted != nil {
			r.pending = append(r.pending[:evictedIndex:evictedIndex], append([]*runnerv1.ExecutionEvent{evicted}, r.pending[evictedIndex:]...)...)
		}
		state.next = previousNext
		r.publishPendingLocked()
		r.mu.Unlock()
		r.recorder().RecordEventDropped(metrics.DropPersistFailed)
		return 0, err
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

// emitStateLocked resolves the lease an event must be sequenced against. A
// lease retained by Abandon accepts terminal events only: the runner no longer
// owns the lease, so further progress and log output belongs to whoever does,
// but the execution's outcome still has to be reported.
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

// knowsJobLocked reports whether any lease for the job is tracked, at any
// generation, so a mismatched generation is reported as stale rather than as an
// absent lease.
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

// makeRoomLocked frees one slot for eventType by removing the oldest queued
// event of the lowest priority below it, returning that event and the index it
// came from so the caller can restore it if the emit does not complete. Log and
// progress output is droppable for any lifecycle event; `started` is droppable
// for a terminal event; terminal events are never discarded. A nil event means
// nothing outranked could be freed.
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

// IsTerminalEventType reports whether an execution-event type carries the final
// outcome of an execution. Terminal events must never be silently dropped.
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
	chunks := splitUTF8Chunks(message, maxLogChunkBytes)
	sequences := make([]int64, 0, len(chunks))
	for index, chunk := range chunks {
		sequence, err := r.Emit(jobID, generation, "log", map[string]any{
			"message":    chunk,
			"chunkIndex": index,
			"chunkCount": len(chunks),
		})
		if err != nil {
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
		r.mu.Unlock()
		return nil
	}
	r.sending = true
	r.mu.Unlock()
	return r.flush()
}

func (r *EventReporter) flush() error {
	for {
		r.mu.Lock()
		if len(r.pending) == 0 {
			r.sending = false
			r.mu.Unlock()
			return nil
		}
		event := r.pending[0]
		r.mu.Unlock()

		if err := r.client.SendExecutionEvent(event); err != nil {
			r.mu.Lock()
			r.sending = false
			r.mu.Unlock()
			return err
		}

		r.mu.Lock()
		if len(r.pending) > 0 && r.pending[0] == event {
			r.pending = r.pending[1:]
			r.publishPendingLocked()
			if err := r.persistLocked(); err != nil {
				r.sending = false
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

func marshalEventPayloadWithPrefixes(payload map[string]any, prefixes []string) ([]byte, error) {
	if payload == nil {
		payload = map[string]any{}
	}
	contents, err := json.Marshal(redactPayloadWithPrefixes(payload, prefixes))
	if err != nil {
		return nil, fmt.Errorf("encode execution event payload: %w", err)
	}
	if len(contents) > maxExecutionEventPayloadBytes {
		return nil, errors.New("execution event payload is too large")
	}
	return contents, nil
}

func redactPayloadWithPrefixes(value any, prefixes []string) any {
	switch current := value.(type) {
	case map[string]any:
		redacted := make(map[string]any, len(current))
		for key, item := range current {
			if sensitivePayloadKey(key) {
				redacted[key] = "[REDACTED]"
				continue
			}
			redacted[key] = redactPayloadWithPrefixes(item, prefixes)
		}
		return redacted
	case []any:
		redacted := make([]any, len(current))
		for index, item := range current {
			redacted[index] = redactPayloadWithPrefixes(item, prefixes)
		}
		return redacted
	case []string:
		// Terminal payloads are built in Go, not decoded from JSON, so their
		// string lists arrive as []string and never match the []any arm. Every
		// one of them — changed files, commands run, the remaining work an
		// agent reports — is agent-authored text that can carry a credential,
		// so they are redacted rather than passed through by the default arm.
		redacted := make([]string, len(current))
		for index, item := range current {
			redacted[index] = redactKnownSecretValues(item, prefixes)
		}
		return redacted
	case string:
		return redactKnownSecretValues(current, prefixes)
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

func redactKnownSecretValues(value string, configured []string) string {
	prefixes := append([]string{"ghp_", "github_pat_", "glpat-", "sk-"}, configured...)
	for _, prefix := range prefixes {
		searched := 0
		for searched < len(value) {
			offset := strings.Index(value[searched:], prefix)
			if offset < 0 {
				break
			}
			start := searched + offset
			// A match that continues an identifier is part of a longer word, not
			// the start of a token. Without this, `sk-` matches inside ordinary
			// paths and commands — `task-runner.py`, `disk-usage.ts`, `make
			// task-build` — and redacting there corrupts the changed-file and
			// command lists a terminal payload carries.
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

// secretTokenByte reports whether a byte can appear inside a credential token,
// and therefore whether it continues one rather than delimiting it.
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
