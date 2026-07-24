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
