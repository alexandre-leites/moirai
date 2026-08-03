package server

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// The strings below are not invented: they are the literal shape gh (v2.45.0)
// prints for a 404 and a 401 (confirmed by running `gh api` against a
// nonexistent repo and with a bad token respectively), and for a DNS failure
// (confirmed by pointing GH_HOST at an unresolvable host). Guessing at error
// text that gh does not actually produce would make this classifier
// worthless the first time it met a real outage.
func TestIsTransientGitHubError(t *testing.T) {
	deadline, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-deadline.Done()

	transient := map[string]error{
		"gh invocation timed out": fmt.Errorf("gh pr view 7: %w", deadline.Err()),
		"parent context canceled": fmt.Errorf("gh pr view 7: %w", context.Canceled),
		"dns/network failure":     errors.New("gh api repos/x/y: error connecting to api.github.com\ncheck your internet connection or https://githubstatus.com"),
		"dial tcp failure":        errors.New(`gh api repos/x/y: Get "https://api.github.com/repos/x/y": dial tcp: lookup api.github.com: no such host`),
		"connection reset":        errors.New("gh api repos/x/y: connection reset by peer"),
		"i/o timeout":             errors.New("gh api repos/x/y: Get \"https://api.github.com/repos/x/y\": context deadline exceeded (Client.Timeout exceeded while awaiting headers): i/o timeout"),
		"server error 500":        errors.New("gh pr view 7: gh: Internal Server Error (HTTP 500)"),
		"server error 502":        errors.New("gh pr merge 7: gh: Bad Gateway (HTTP 502)"),
		"server error 503":        errors.New("gh pr view 7: gh: Service Unavailable (HTTP 503)"),
		"rate limited":            errors.New("gh api repos/x/y: gh: API rate limit exceeded for user ID 123. (HTTP 403)"),
		"secondary rate limit":    errors.New("gh pr create: gh: You have exceeded a secondary rate limit (HTTP 403)"),
		"abuse detection":         errors.New("gh pr create: gh: abuse detection mechanism triggered (HTTP 403)"),
		"429 too many requests":   errors.New("gh api repos/x/y: gh: Too Many Requests (HTTP 429)"),
	}
	for name, err := range transient {
		t.Run(name, func(t *testing.T) {
			if !isTransientGitHubError(err) {
				t.Fatalf("isTransientGitHubError(%q) = false, want true", err.Error())
			}
		})
	}

	terminal := map[string]error{
		"not found":            errors.New(`gh pr list: exit status 1: {"message":"Not Found","status":"404"} gh: Not Found (HTTP 404)`),
		"bad credentials":      errors.New(`gh pr list: exit status 1: {"message":"Bad credentials","status":"401"} gh: Bad credentials (HTTP 401)`),
		"permission denied":    errors.New("gh pr merge 7: gh: Resource not accessible by integration (HTTP 403)"),
		"merge conflict":       errors.New("gh pr merge 7: exit status 1: X Pull request #7 is not mergeable: the merge commit could not be cleanly created."),
		"unresolvable pr":      errors.New("GraphQL: Could not resolve to a PullRequest with the number of 999999. (repository.pullRequest)"),
		"unrecognised failure": errors.New("gh pr merge 7: exit status 1: something gh has never said before"),
	}
	for name, err := range terminal {
		t.Run(name, func(t *testing.T) {
			if isTransientGitHubError(err) {
				t.Fatalf("isTransientGitHubError(%q) = true, want false", err.Error())
			}
		})
	}

	if isTransientGitHubError(nil) {
		t.Fatal("isTransientGitHubError(nil) = true, want false")
	}
}
