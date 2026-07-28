package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"

	runnerv1 "github.com/loop-engineering/contracts/gen/runner/v1"
)

const maxExecutionEventPayloadBytes = 16 * 1024
const maxLogChunkBytes = 6 * 1024

var ErrEventReporterConfiguration = errors.New("runner event reporter configuration is invalid")
var ErrNoActiveEventLease = errors.New("runner event lease is not active")
var ErrStaleEventLease = errors.New("runner event lease is stale")

type EventClient interface {
	SendExecutionEvent(*runnerv1.ExecutionEvent) error
}

// EventReporter tracks execution-event sequencing for every concurrently
// active lease on the runner, keyed by job ID. A single flat `pending` queue
// preserves send order across leases and shares one `maxPending` budget.
type EventReporter struct {
	client            EventClient
	maxPending        int
	redactionPrefixes []string
	outbox            *eventOutbox

	mu      sync.Mutex
	leases  map[string]Lease
	next    map[string]int64
	pending []*runnerv1.ExecutionEvent
	sending bool
}

func NewEventReporter(client EventClient, maxPending int) (*EventReporter, error) {
	return NewEventReporterWithRedaction(client, maxPending, nil)
}

func NewEventReporterWithRedaction(client EventClient, maxPending int, prefixes []string) (*EventReporter, error) {
	return NewEventReporterWithOutbox(client, maxPending, prefixes, "")
}

func NewEventReporterWithOutbox(client EventClient, maxPending int, prefixes []string, outboxPath string) (*EventReporter, error) {
	if client == nil || maxPending < 1 || !validRedactionPrefixes(prefixes) {
		return nil, ErrEventReporterConfiguration
	}
	reporter := &EventReporter{
		client:            client,
		maxPending:        maxPending,
		redactionPrefixes: append([]string(nil), prefixes...),
		leases:            map[string]Lease{},
		next:              map[string]int64{},
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
	return reporter, nil
}

func (r *EventReporter) Begin(lease Lease) error {
	if lease.JobID == "" || lease.Generation < 1 || lease.Packet.ExecutionID == "" {
		return errors.New("event lease is invalid")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if existing, ok := r.leases[lease.JobID]; ok && existing.Generation != lease.Generation {
		return ErrStaleEventLease
	}
	if _, ok := r.leases[lease.JobID]; !ok {
		r.leases[lease.JobID] = lease
		r.next[lease.JobID] = 0
	}
	return nil
}

func (r *EventReporter) Abandon(jobID string, generation int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.leases[jobID]
	if !ok || existing.Generation != generation {
		return false
	}
	delete(r.leases, jobID)
	delete(r.next, jobID)
	r.pending = withoutJobEvents(r.pending, jobID)
	_ = r.persistLocked()
	return true
}

func (r *EventReporter) Finish(jobID string, generation int64) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	existing, ok := r.leases[jobID]
	if !ok || existing.Generation != generation {
		return false
	}
	delete(r.leases, jobID)
	delete(r.next, jobID)
	return true
}

func (r *EventReporter) Emit(jobID string, generation int64, eventType string, payload map[string]any) (int64, error) {
	if !validEventType(eventType) {
		return 0, errors.New("execution event type is invalid")
	}
	contents, err := marshalEventPayloadWithPrefixes(payload, r.redactionPrefixes)
	if err != nil {
		return 0, err
	}

	r.mu.Lock()
	lease, ok := r.leases[jobID]
	if !ok {
		r.mu.Unlock()
		return 0, ErrNoActiveEventLease
	}
	if lease.Generation != generation {
		r.mu.Unlock()
		return 0, ErrStaleEventLease
	}
	if len(r.pending) >= r.maxPending {
		r.mu.Unlock()
		return 0, errors.New("execution event buffer is full")
	}
	previousNext := r.next[jobID]
	sequence := previousNext + 1
	r.next[jobID] = sequence
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
		r.next[jobID] = previousNext
		r.mu.Unlock()
		return 0, err
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

func withoutJobEvents(events []*runnerv1.ExecutionEvent, jobID string) []*runnerv1.ExecutionEvent {
	if len(events) == 0 {
		return events
	}
	kept := make([]*runnerv1.ExecutionEvent, 0, len(events))
	for _, event := range events {
		if event.GetJobId() != jobID {
			kept = append(kept, event)
		}
	}
	return kept
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
	if len(r.leases) == 0 && len(r.pending) == 0 {
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

func marshalEventPayload(payload map[string]any) ([]byte, error) {
	return marshalEventPayloadWithPrefixes(payload, nil)
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

func redactPayload(value any) any {
	return redactPayloadWithPrefixes(value, nil)
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
		for {
			start := strings.Index(value, prefix)
			if start < 0 {
				break
			}
			end := start + len(prefix)
			for end < len(value) && ((value[end] >= 'a' && value[end] <= 'z') || (value[end] >= 'A' && value[end] <= 'Z') || (value[end] >= '0' && value[end] <= '9') || value[end] == '_' || value[end] == '-') {
				end++
			}
			value = value[:start] + "[REDACTED]" + value[end:]
		}
	}
	return value
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
