package workflow

import "fmt"

type State string

const (
	Offered             State = "offered"
	Implementing        State = "implementing"
	PRCreated           State = "pr_created"
	WaitingGitHubChecks State = "waiting_github_checks"
	Merging             State = "merging"
	Completed           State = "completed"
	Failed              State = "failed"
	Blocked             State = "blocked"
	Cancelled           State = "cancelled"
)

func (state State) Terminal() bool {
	switch state {
	case Completed, Failed, Blocked, Cancelled:
		return true
	default:
		return false
	}
}

func (state State) CanTransitionTo(next State) bool {
	switch state {
	case Offered:
		return next == Implementing || next == Failed || next == Blocked || next == Cancelled
	case Implementing:
		return next == PRCreated || next == Failed || next == Blocked || next == Cancelled
	case PRCreated:
		return next == WaitingGitHubChecks || next == Failed || next == Blocked || next == Cancelled
	case WaitingGitHubChecks:
		return next == Merging || next == Failed || next == Blocked || next == Cancelled
	case Merging:
		return next == Completed || next == Failed || next == Blocked || next == Cancelled
	default:
		return false
	}
}

func (state State) Transition(next State) (State, error) {
	if !state.CanTransitionTo(next) {
		return state, fmt.Errorf("invalid workflow transition: %s to %s", state, next)
	}
	return next, nil
}

func RunnerResult(state State, succeeded bool) (State, error) {
	if state != Implementing {
		return state, fmt.Errorf("runner result requires %s state", Implementing)
	}
	if succeeded {
		return state.Transition(PRCreated)
	}
	return state.Transition(Failed)
}

func ChecksResult(state State, green bool) (State, error) {
	if state != WaitingGitHubChecks {
		return state, fmt.Errorf("checks result requires %s state", WaitingGitHubChecks)
	}
	if !green {
		return state, nil
	}
	return state.Transition(Merging)
}

func MergeResult(state State, succeeded bool) (State, error) {
	if state != Merging {
		return state, fmt.Errorf("merge result requires %s state", Merging)
	}
	if succeeded {
		return state.Transition(Completed)
	}
	return state.Transition(Failed)
}

func ManualRetry(state State) (State, error) {
	switch state {
	case Failed, Blocked, Cancelled:
		return Offered, nil
	default:
		return state, fmt.Errorf("manual retry requires failed, blocked, or cancelled state")
	}
}
