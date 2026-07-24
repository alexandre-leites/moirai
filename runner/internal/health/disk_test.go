package health

import (
	"errors"
	"testing"
)

func TestRequireFreeBytesChecksThresholdAndErrors(t *testing.T) {
	available := func(string) (uint64, error) { return 2048, nil }
	if err := RequireFreeBytes("/data", 1024, available); err != nil {
		t.Fatal(err)
	}
	if err := RequireFreeBytes("/data", 4096, available); err == nil {
		t.Fatal("expected insufficient disk error")
	}
	if err := RequireFreeBytes("/data", 1024, func(string) (uint64, error) { return 0, errors.New("unavailable") }); err == nil {
		t.Fatal("expected disk probe error")
	}
}
