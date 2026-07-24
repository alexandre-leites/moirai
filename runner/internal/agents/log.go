package agents

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
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
