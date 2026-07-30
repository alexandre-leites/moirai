package agents

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteLogMetadataPersistsChecksums(t *testing.T) {
	directory := t.TempDir()
	stdout := newBoundedLogWriter(&bytes.Buffer{})
	stderr := newBoundedLogWriter(&bytes.Buffer{})
	_, _ = stdout.Write([]byte("stdout"))
	_, _ = stderr.Write([]byte("stderr"))
	writeLogMetadata(directory, "agent", stdout, stderr)
	contents, err := os.ReadFile(filepath.Join(directory, "agent.log-metadata.json"))
	if err != nil || !strings.Contains(string(contents), stdout.checksum()) || !strings.Contains(string(contents), stderr.checksum()) {
		t.Fatalf("metadata = %q, %v", contents, err)
	}
}

func TestBoundedLogWriterTruncatesWithoutChangingProcessWriteCount(t *testing.T) {
	var output bytes.Buffer
	writer := newBoundedLogWriter(&output)
	contents := []byte(strings.Repeat("x", maxLogBytes+1))
	n, err := writer.Write(contents)
	if err != nil || n != len(contents) || output.Len() != maxLogBytes || !writer.truncated || writer.checksum() == "" {
		t.Fatalf("log writer = %d/%v/%d/%t/%q", n, err, output.Len(), writer.truncated, writer.checksum())
	}
}

// A continuation extends the execution's log rather than starting a second one,
// so the maxLogBytes bound applies to the execution. An attempt that already hit
// it stays reported as truncated even if the next writes nothing: the flag
// describes the execution's log, and clearing it would claim a complete log
// where a truncated one is on disk.
func TestOpenAgentLogSeedsTheBoundAndTruncationFlagFromTheExistingLog(t *testing.T) {
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, "agent.stdout.log"), make([]byte, maxLogBytes), 0o600); err != nil {
		t.Fatalf("seed log: %v", err)
	}
	file, writer, err := openAgentLog(directory, "agent.stdout.log")
	if err != nil {
		t.Fatalf("openAgentLog() error = %v", err)
	}
	defer file.Close()
	if writer.written != maxLogBytes {
		t.Fatalf("written = %d, want the existing %d", writer.written, maxLogBytes)
	}
	if !writer.truncated {
		t.Fatal("truncated = false; a log already at the bound must stay reported as truncated")
	}
	// The bound is the execution's, so a continuation writing on top of a full
	// log persists nothing further while still reporting the full byte count.
	if _, err := writer.Write([]byte("more output")); err != nil {
		t.Fatalf("Write() error = %v", err)
	}
	contents, err := os.ReadFile(filepath.Join(directory, "agent.stdout.log"))
	if err != nil {
		t.Fatalf("read log: %v", err)
	}
	if len(contents) != maxLogBytes || writer.written != maxLogBytes {
		t.Fatalf("log grew past the execution bound: file %d, written %d", len(contents), writer.written)
	}
}
