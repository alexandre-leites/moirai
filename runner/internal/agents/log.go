package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const maxLogBytes = 4 << 20

type boundedLogWriter struct {
	writer    io.Writer
	hash      hashWriter
	written   int
	truncated bool
}

type hashWriter interface {
	Write([]byte) (int, error)
	Sum([]byte) []byte
}

func newBoundedLogWriter(writer io.Writer) *boundedLogWriter {
	return &boundedLogWriter{writer: writer, hash: sha256.New()}
}

func (writer *boundedLogWriter) Write(contents []byte) (int, error) {
	_, _ = writer.hash.Write(contents)
	if writer.written >= maxLogBytes {
		writer.truncated = true
		return len(contents), nil
	}
	remaining := maxLogBytes - writer.written
	persisted := contents
	if len(persisted) > remaining {
		persisted = persisted[:remaining]
		writer.truncated = true
	}
	n, err := writer.writer.Write(persisted)
	writer.written += n
	if err != nil {
		return n, err
	}
	return len(contents), nil
}

func (writer *boundedLogWriter) checksum() string {
	return hex.EncodeToString(writer.hash.Sum(nil))
}

// openAgentLog opens one of an execution's agent log files for writing.
//
// It appends rather than truncates, because an execution can now invoke its
// agent more than once: the goal gate's continuation loop re-engages the same
// agent in the same workspace, and truncating would erase the attempt that
// explains why the continuation was issued at all. The returned writer is
// seeded from what the file already holds so the byte count, the truncation
// flag, and the maxLogBytes bound describe the whole execution rather than only
// its last attempt — the bound in particular, since a per-attempt bound would
// let a continuing execution write a multiple of it.
//
// The checksum the metadata records is over the bytes on disk. For a log that
// was never truncated that is also everything the agent produced; past the
// bound the two part company, since only the retained prefix can be re-read.
//
// Seeding is best effort: a log that cannot be re-read still gets its new
// output, and the metadata then describes what this process actually wrote.
func openAgentLog(directory, name string) (*os.File, *boundedLogWriter, error) {
	path := filepath.Join(directory, name)
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return nil, nil, fmt.Errorf("create agent log %s: %w", name, err)
	}
	writer := newBoundedLogWriter(file)
	if existing, err := os.ReadFile(path); err == nil && len(existing) > 0 {
		_, _ = writer.hash.Write(existing)
		writer.written = len(existing)
		// An earlier attempt that already hit the bound stays reported as
		// truncated: the flag describes the execution's log, and a later
		// attempt writing nothing must not clear it.
		writer.truncated = writer.written >= maxLogBytes
	}
	return file, writer, nil
}

func writeLogMetadata(directory, name string, stdout, stderr *boundedLogWriter) {
	contents, err := json.Marshal(struct {
		StdoutChecksum  string `json:"stdoutChecksum"`
		StderrChecksum  string `json:"stderrChecksum"`
		StdoutBytes     int    `json:"stdoutBytes"`
		StderrBytes     int    `json:"stderrBytes"`
		StdoutTruncated bool   `json:"stdoutTruncated"`
		StderrTruncated bool   `json:"stderrTruncated"`
	}{stdout.checksum(), stderr.checksum(), stdout.written, stderr.written, stdout.truncated, stderr.truncated})
	if err == nil {
		_ = os.WriteFile(filepath.Join(directory, name+".log-metadata.json"), append(contents, '\n'), 0o600)
	}
}
