// Package textutil holds small, generic string/value-formatting helpers
// that have no relationship to the orchestrator's Server type: they take
// and return plain strings, times and nullable scalars, never a database
// handle or a gRPC type. They used to live as free functions inside package
// server -- see issue #285.
package textutil

import (
	"strconv"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// Truncate returns value cut to at most length bytes. It does not avoid
// splitting a multi-byte rune -- callers that need UTF-8-safe truncation of
// untrusted text should not use this helper (see boundedReason in
// orchestrator/internal/server for that case).
func Truncate(value string, length int) string {
	if len(value) > length {
		return value[:length]
	}
	return value
}

// ParseInt converts a decimal string to an int64, rejecting the whole value
// on any malformed input. fmt.Sscan was tried here previously, but it scans
// a leading numeric prefix and reports success even when trailing
// characters remain (e.g. "123abc" silently becomes 123) and leaves its
// destination holding whatever it held before the call when nothing scans
// (e.g. "abc"), which is a second way to fail without returning an error.
// strconv.ParseInt requires the entire string to be a valid integer.
func ParseInt(value string) (int64, error) {
	return strconv.ParseInt(value, 10, 64)
}

// Timestamp renders value the one way every RPC response in this service
// carries a timestamp: UTC, RFC 3339 with nanosecond precision.
func Timestamp(value time.Time) string { return value.UTC().Format(time.RFC3339Nano) }

// StringValue dereferences a *string, reading nil as "".
func StringValue(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

// TextValue reads a nullable sqlc-generated pgtype.Text the same way
// StringValue reads a *string: empty when NULL.
func TextValue(value pgtype.Text) string {
	if !value.Valid {
		return ""
	}
	return value.String
}

// CoalesceText mirrors SQL's COALESCE over two nullable columns read
// separately: the first valid value wins, and both absent yields "".
func CoalesceText(preferred, fallback pgtype.Text) string {
	if preferred.Valid {
		return preferred.String
	}
	return TextValue(fallback)
}
