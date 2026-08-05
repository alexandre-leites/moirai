package control

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	runnerv1 "github.com/alexandre-leites/moirai/contracts/gen/runner/v1"
)

const eventOutboxVersion = 1

type eventOutboxFile struct {
	Version int                        `json:"version"`
	Events  []*runnerv1.ExecutionEvent `json:"events"`
}

type eventOutbox struct {
	path string
}

func newEventOutbox(path string) (*eventOutbox, error) {
	if path == "" || !filepath.IsAbs(path) {
		return nil, errors.New("event outbox path must be absolute")
	}
	return &eventOutbox{path: filepath.Clean(path)}, nil
}

func (outbox *eventOutbox) Load() ([]*runnerv1.ExecutionEvent, error) {
	contents, err := os.ReadFile(outbox.path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("read event outbox: %w", err)
	}
	var stored eventOutboxFile
	if err := json.Unmarshal(contents, &stored); err != nil {
		return nil, fmt.Errorf("parse event outbox: %w", err)
	}
	if stored.Version != eventOutboxVersion {
		return nil, errors.New("event outbox version is invalid")
	}
	if len(stored.Events) == 0 {
		return nil, errors.New("event outbox is empty")
	}
	for index, event := range stored.Events {
		if !validStoredEvent(event) {
			return nil, fmt.Errorf("event outbox event %d is invalid", index)
		}
		if index > 0 && event.GetEventSequence() <= stored.Events[index-1].GetEventSequence() && event.GetExecutionId() == stored.Events[index-1].GetExecutionId() {
			return nil, errors.New("event outbox sequence is invalid")
		}
	}
	return stored.Events, nil
}

func (outbox *eventOutbox) Save(events []*runnerv1.ExecutionEvent) error {
	if len(events) == 0 {
		if err := os.Remove(outbox.path); err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("remove event outbox: %w", err)
		}
		return nil
	}
	contents, err := json.Marshal(eventOutboxFile{Version: eventOutboxVersion, Events: events})
	if err != nil {
		return fmt.Errorf("encode event outbox: %w", err)
	}
	if err := os.MkdirAll(filepath.Dir(outbox.path), 0o700); err != nil {
		return fmt.Errorf("create event outbox directory: %w", err)
	}
	temporary, err := os.CreateTemp(filepath.Dir(outbox.path), ".events-*.tmp")
	if err != nil {
		return fmt.Errorf("create event outbox temporary file: %w", err)
	}
	temporaryPath := temporary.Name()
	defer temporary.Close()
	defer os.Remove(temporaryPath)
	if err := temporary.Chmod(0o600); err != nil {
		return fmt.Errorf("secure event outbox temporary file: %w", err)
	}
	if _, err := temporary.Write(append(contents, '\n')); err != nil {
		return fmt.Errorf("write event outbox: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		return fmt.Errorf("sync event outbox: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("close event outbox: %w", err)
	}
	if err := os.Rename(temporaryPath, outbox.path); err != nil {
		return fmt.Errorf("replace event outbox: %w", err)
	}
	return nil
}

func validStoredEvent(event *runnerv1.ExecutionEvent) bool {
	return event != nil && event.GetJobId() != "" && event.GetExecutionId() != "" && event.GetLeaseGeneration() > 0 && event.GetEventSequence() > 0 && validEventType(event.GetType()) && len(event.GetPayloadJson()) <= maxExecutionEventPayloadBytes && json.Valid([]byte(event.GetPayloadJson()))
}
