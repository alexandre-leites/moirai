package dispatch

import (
	"errors"
	"testing"
)

func TestFailureFingerprintIsStableAndDoesNotExposeSecrets(t *testing.T) {
	first := FailureFingerprint("execution", errors.New("pipeline failed token=secret-value"))
	second := FailureFingerprint("execution", errors.New("pipeline failed token=other-value"))
	if first != second || first == "" {
		t.Fatalf("fingerprints = %q, %q", first, second)
	}
	if first == "pipeline:" || len(first) != len("pipeline:")+16 {
		t.Fatalf("fingerprint = %q", first)
	}
}
