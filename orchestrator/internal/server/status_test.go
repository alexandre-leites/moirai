package server

import "testing"

func TestParseStatusAcceptsEveryKnownValue(t *testing.T) {
	for status := range knownStatuses {
		parsed, ok := ParseStatus(status.String())
		if !ok {
			t.Fatalf("ParseStatus(%q) = (_, false), want true", status)
		}
		if parsed != status {
			t.Fatalf("ParseStatus(%q) = %q, want %q", status, parsed, status)
		}
	}
}

func TestParseStatusRejectsUnknownValues(t *testing.T) {
	for _, value := range []string{"", "implementing", "reviewing", "Offered", "running"} {
		if _, ok := ParseStatus(value); ok {
			t.Fatalf("ParseStatus(%q) = (_, true), want false: %q is not a status any writer produces", value, value)
		}
	}
}

// terminalStatus is the predicate every terminal-state check in this package
// shares (see status.go). This pins that it agrees with knownStatuses's
// terminal/non-terminal split, since a status added to one list and not
// reflected here would silently miscount as active or as terminal.
func TestTerminalStatusMatchesTerminalStatuses(t *testing.T) {
	for status := range knownStatuses {
		want := false
		for _, terminal := range terminalStatuses {
			if status == terminal {
				want = true
				break
			}
		}
		if got := terminalStatus(status.String()); got != want {
			t.Fatalf("terminalStatus(%q) = %v, want %v", status, got, want)
		}
	}
	if terminalStatus("not-a-real-status") {
		t.Fatal("terminalStatus treated an unknown value as terminal")
	}
}
