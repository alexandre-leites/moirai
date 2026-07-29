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

// maxLogTailBytes bounds the failure excerpt carried by a terminal event. The
// encoded event payload is capped at 16 KiB, and an agent log is unbounded, so
// the tail is sanitised (which removes the escape sequences that would other-
// wise inflate several-fold as JSON) and then cut to this many bytes.
const maxLogTailBytes = 2048

// logTailWindowBytes is how much of a log file is read before sanitising. It is
// larger than the tail itself so that stripping control characters still leaves
// enough material to fill it.
const logTailWindowBytes = 8 * maxLogTailBytes

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
// invalid UTF-8, and keeps only the final maxLogTailBytes bytes.
func boundedLogTail(value string) string {
	sanitized := strings.TrimSpace(strings.ToValidUTF8(sanitizeLogText(value), ""))
	if len(sanitized) <= maxLogTailBytes {
		return sanitized
	}
	start := len(sanitized) - (maxLogTailBytes - len(truncationMarker))
	for start < len(sanitized) && !utf8.RuneStart(sanitized[start]) {
		start++
	}
	return truncationMarker + sanitized[start:]
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
