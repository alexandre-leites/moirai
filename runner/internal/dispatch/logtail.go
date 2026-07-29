package dispatch

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/loop-engineering/runner/internal/pipeline"
	"github.com/loop-engineering/runner/internal/repository"
)

// maxLogTailBytes bounds the failure excerpt carried by a terminal event,
// measured as the JSON-encoded size rather than the raw one. Raw length is the
// wrong budget: the whole event payload is capped at 16 KiB encoded, and Go's
// encoder escapes `<`, `>`, and `&` to six bytes each — all three are common in
// real failure output (`<module>`, generics, XML fixtures), so a 2 KiB raw tail
// of them would consume 12 KiB of the payload and push the run onto the reduced
// payload path.
const maxLogTailBytes = 2048

// logTailWindowBytes is how much of a log file is read before sanitising. It is
// larger than the tail itself so that stripping control characters, and the
// escaping budget above, still leave enough material to fill it.
const logTailWindowBytes = 16 * maxLogTailBytes

// logTail returns a bounded excerpt explaining a failed run: the output of the
// pipeline command that failed when there is one, otherwise the end of the
// agent's own log. It is best effort — an unreadable log yields an empty tail
// rather than an error, since the run has already failed for another reason.
func logTail(workspace repository.Workspace, results []pipeline.Result) string {
	if output := failedPipelineOutput(results); output != "" {
		if tail := boundedLogTail(output); tail != "" {
			return tail
		}
	}
	return agentLogTail(workspace.Loop)
}

func failedPipelineOutput(results []pipeline.Result) string {
	for index := len(results) - 1; index >= 0; index-- {
		if results[index].ExitCode != 0 || results[index].TimedOut {
			return results[index].Output
		}
	}
	return ""
}

// agentLogTail prefers the agent's standard error, falling back to standard
// output when it carries nothing. Log file names are chosen by the backend
// (`opencode.stderr.log`, `docker-cli.stderr.log`, `<cli name>.stderr.log`), so
// they are matched by shape rather than by the configured backend name.
func agentLogTail(loop string) string {
	if loop == "" {
		return ""
	}
	for _, pattern := range []string{"*.stderr.log", "*.stdout.log"} {
		matches, err := filepath.Glob(filepath.Join(loop, pattern))
		if err != nil {
			continue
		}
		sort.Strings(matches)
		for index := len(matches) - 1; index >= 0; index-- {
			if tail := fileTail(matches[index]); tail != "" {
				return tail
			}
		}
	}
	return ""
}

func fileTail(path string) string {
	file, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || info.Size() == 0 {
		return ""
	}
	offset := int64(0)
	if info.Size() > logTailWindowBytes {
		offset = info.Size() - logTailWindowBytes
	}
	window := make([]byte, info.Size()-offset)
	read, err := file.ReadAt(window, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return ""
	}
	return boundedLogTail(string(window[:read]))
}

// boundedLogTail renders arbitrary process output as a payload-safe string: it
// drops terminal escape sequences and other control characters, replaces
// invalid UTF-8, and keeps the longest suffix whose JSON encoding fits in
// maxLogTailBytes.
func boundedLogTail(value string) string {
	sanitized := strings.TrimSpace(strings.ToValidUTF8(sanitizeLogText(value), ""))
	budget := maxLogTailBytes - jsonEncodedSize(truncationMarker)
	start := len(sanitized)
	for start > 0 {
		next := start - 1
		for next > 0 && !utf8.RuneStart(sanitized[next]) {
			next--
		}
		cost := jsonEncodedSize(sanitized[next:start])
		if budget < cost {
			return truncationMarker + sanitized[start:]
		}
		budget -= cost
		start = next
	}
	return sanitized
}

// jsonEncodedSize reports how many bytes value occupies inside a JSON string,
// mirroring encoding/json: quote and backslash are escaped to two bytes, the
// HTML-significant bytes and anything below 0x20 to six, and everything else —
// including the continuation bytes of a multi-byte rune — is copied verbatim.
func jsonEncodedSize(value string) int {
	size := 0
	for index := 0; index < len(value); index++ {
		switch character := value[index]; {
		case character == '"' || character == '\\' || character == '\n' || character == '\r' || character == '\t':
			size += 2
		case character < 0x20 || character == '<' || character == '>' || character == '&':
			size += 6
		default:
			size++
		}
	}
	return size
}

// sanitizeLogText removes ANSI escape sequences and control characters, keeping
// newlines and tabs. Escape sequences are the reason a 2 KiB excerpt could
// otherwise encode to six times its size as JSON.
func sanitizeLogText(value string) string {
	var builder strings.Builder
	builder.Grow(len(value))
	for index := 0; index < len(value); index++ {
		character := value[index]
		switch {
		case character == 0x1b:
			index = skipEscapeSequence(value, index)
		case character == '\n' || character == '\t':
			builder.WriteByte(character)
		case character < 0x20 || character == 0x7f:
		default:
			builder.WriteByte(character)
		}
	}
	return builder.String()
}

// skipEscapeSequence returns the index of the last byte of the escape sequence
// beginning at start, so the caller's loop resumes after it.
func skipEscapeSequence(value string, start int) int {
	index := start + 1
	if index >= len(value) {
		return index
	}
	switch value[index] {
	case '[': // CSI: parameter and intermediate bytes, then one final byte.
		index++
		for index < len(value) && value[index] >= 0x20 && value[index] <= 0x3f {
			index++
		}
		if index < len(value) && value[index] >= 0x40 && value[index] <= 0x7e {
			return index
		}
		return index - 1
	case ']': // OSC: a string terminated by BEL or ESC \.
		index++
		for index < len(value) {
			if value[index] == 0x07 {
				return index
			}
			if value[index] == 0x1b && index+1 < len(value) && value[index+1] == '\\' {
				return index + 1
			}
			index++
		}
		return index - 1
	default:
		return index
	}
}
