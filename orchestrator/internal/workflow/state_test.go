package workflow

import "testing"

func TestTerminal(t *testing.T) {
	for _, state := range []State{Completed, Failed, Blocked, Cancelled} {
		if !state.Terminal() {
			t.Errorf("%s should be terminal", state)
		}
	}
	for _, state := range []State{Offered, Implementing, PRCreated, WaitingGitHubChecks, Merging} {
		if state.Terminal() {
			t.Errorf("%s should not be terminal", state)
		}
	}
}

func TestTransitionValidation(t *testing.T) {
	for _, test := range []struct {
		from State
		to   State
	}{
		{Offered, Implementing},
		{Implementing, PRCreated},
		{PRCreated, WaitingGitHubChecks},
		{WaitingGitHubChecks, Merging},
		{Merging, Completed},
		{Implementing, Failed},
		{WaitingGitHubChecks, Blocked},
		{Merging, Cancelled},
	} {
		got, err := test.from.Transition(test.to)
		if err != nil || got != test.to {
			t.Errorf("%s to %s = %s, %v", test.from, test.to, got, err)
		}
	}

	for _, test := range []struct {
		from State
		to   State
	}{
		{Offered, Completed},
		{PRCreated, Merging},
		{Completed, Offered},
		{Failed, Offered},
		{State("unknown"), Offered},
	} {
		if _, err := test.from.Transition(test.to); err == nil {
			t.Errorf("%s to %s succeeded", test.from, test.to)
		}
	}
}

func TestRunnerResult(t *testing.T) {
	for _, test := range []struct {
		succeeded bool
		want      State
	}{
		{true, PRCreated},
		{false, Failed},
	} {
		got, err := RunnerResult(Implementing, test.succeeded)
		if err != nil || got != test.want {
			t.Errorf("RunnerResult(%t) = %s, %v", test.succeeded, got, err)
		}
	}
	if _, err := RunnerResult(Offered, true); err == nil {
		t.Error("runner result outside implementing succeeded")
	}
}

func TestChecksResult(t *testing.T) {
	got, err := ChecksResult(WaitingGitHubChecks, true)
	if err != nil || got != Merging {
		t.Errorf("green checks = %s, %v", got, err)
	}
	got, err = ChecksResult(WaitingGitHubChecks, false)
	if err != nil || got != WaitingGitHubChecks {
		t.Errorf("non-green checks = %s, %v", got, err)
	}
	if _, err := ChecksResult(PRCreated, true); err == nil {
		t.Error("checks result outside waiting state succeeded")
	}
}

func TestMergeResult(t *testing.T) {
	got, err := MergeResult(Merging, true)
	if err != nil || got != Completed {
		t.Errorf("successful merge = %s, %v", got, err)
	}
	got, err = MergeResult(Merging, false)
	if err != nil || got != Failed {
		t.Errorf("failed merge = %s, %v", got, err)
	}
	if _, err := MergeResult(WaitingGitHubChecks, true); err == nil {
		t.Error("merge result outside merging state succeeded")
	}
}

func TestManualRetry(t *testing.T) {
	for _, state := range []State{Failed, Blocked, Cancelled} {
		got, err := ManualRetry(state)
		if err != nil || got != Offered {
			t.Errorf("ManualRetry(%s) = %s, %v", state, got, err)
		}
	}
	for _, state := range []State{Completed, Offered, Implementing, PRCreated, WaitingGitHubChecks, Merging} {
		if _, err := ManualRetry(state); err == nil {
			t.Errorf("ManualRetry(%s) succeeded", state)
		}
	}
}
