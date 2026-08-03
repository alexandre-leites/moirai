package server

import (
	"context"
	"strconv"
	"strings"
	"testing"
)

type fakeCommand struct {
	outputs [][]byte
	calls   [][]string
}

func (fake *fakeCommand) Run(_ context.Context, _ string, args ...string) ([]byte, error) {
	fake.calls = append(fake.calls, args)
	output := fake.outputs[0]
	fake.outputs = fake.outputs[1:]
	return output, nil
}

func noToken(context.Context, string, string) (string, error) { return "", nil }

func TestGitHubIssueParsing(t *testing.T) {
	command := &fakeCommand{outputs: [][]byte{[]byte(`[{"number":42,"title":"Fix","body":"","url":"https://github.com/acme/demo/issues/42","state":"OPEN","labels":[{"name":"agent:ready"},{"name":"agent-priority:7"}],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z"}]`)}}
	issues, err := NewGitHubCLI(command, noToken).ListTasks(context.Background(), "p1", "s1", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !issues[0].Eligible || issues[0].Priority != 7 || issues[0].ExternalID != "42" || issues[0].State != "open" {
		t.Fatalf("issues = %#v", issues)
	}
	if command.calls[0][0] != "issue" {
		t.Fatalf("command = %#v", command.calls)
	}
	var sawStateAll, sawJSONState bool
	for i, arg := range command.calls[0] {
		if arg == "--state" && i+1 < len(command.calls[0]) && command.calls[0][i+1] == "all" {
			sawStateAll = true
		}
		if arg == "--json" && i+1 < len(command.calls[0]) && strings.Contains(command.calls[0][i+1], "state") {
			sawJSONState = true
		}
	}
	if !sawStateAll {
		t.Fatalf("ListIssues did not request --state all: %#v", command.calls[0])
	}
	if !sawJSONState {
		t.Fatalf("ListIssues did not request the state JSON field: %#v", command.calls[0])
	}
}

// TestGitHubIssueParsingReconcilesClosedIssues verifies that a closed issue
// (only reachable now that ListIssues fetches --state all) is decoded as
// state="closed" and forced ineligible regardless of its labels: an issue
// with no open work left to do must not stay schedulable just because
// nobody removed agent:ready from it before closing it.
func TestGitHubIssueParsingReconcilesClosedIssues(t *testing.T) {
	command := &fakeCommand{outputs: [][]byte{[]byte(`[{"number":42,"title":"Fix","body":"","url":"https://github.com/acme/demo/issues/42","state":"CLOSED","labels":[{"name":"agent:ready"}],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z"}]`)}}
	issues, err := NewGitHubCLI(command, noToken).ListTasks(context.Background(), "p1", "s1", "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || issues[0].State != "closed" || issues[0].Eligible {
		t.Fatalf("issues = %#v", issues)
	}
}

// TestGitHubIssueParsingLimit verifies ListIssues requests a limit far above
// the old hardcoded 100, and logs (rather than silently truncating) when the
// response comes back exactly at that cap -- gh issue list gives no other
// way to distinguish "that was the whole list" from "the limit was hit".
func TestGitHubIssueParsingLimit(t *testing.T) {
	command := &fakeCommand{outputs: [][]byte{[]byte(`[]`)}}
	if _, err := NewGitHubCLI(command, noToken).ListTasks(context.Background(), "p1", "s1", "acme/demo"); err != nil {
		t.Fatal(err)
	}
	for i, arg := range command.calls[0] {
		if arg == "--limit" {
			if i+1 >= len(command.calls[0]) {
				t.Fatalf("--limit had no value: %#v", command.calls[0])
			}
			if command.calls[0][i+1] != strconv.Itoa(githubIssueListLimit) {
				t.Fatalf("--limit = %s, want %d (the old hardcoded 100 truncates any project with more open issues)", command.calls[0][i+1], githubIssueListLimit)
			}
			return
		}
	}
	t.Fatalf("ListIssues did not pass --limit: %#v", command.calls[0])
}

func TestGitHubPRCreationFindsCreatedPR(t *testing.T) {
	command := &fakeCommand{outputs: [][]byte{
		[]byte(`[]`),
		[]byte(`https://github.com/acme/demo/pull/7\n`),
		[]byte(`[{"number":7,"url":"https://github.com/acme/demo/pull/7","state":"OPEN","headRefOid":"abc123"}]`),
	}}
	pr, err := NewGitHubCLI(command, noToken).FindOrCreatePR(context.Background(), "p1", "acme/demo", "agent/demo", "main", "Title", "Body")
	if err != nil {
		t.Fatal(err)
	}
	if pr.Number != "7" || pr.HeadSHA != "abc123" || len(command.calls) != 3 || command.calls[1][0] != "pr" || command.calls[1][1] != "create" {
		t.Fatalf("pr=%#v calls=%#v", pr, command.calls)
	}
}

func TestRepositoryAndCheckParsing(t *testing.T) {
	repository, err := repositoryRef("https://github.com/acme/demo.git")
	if err != nil || repository != "acme/demo" {
		t.Fatalf("repository = %q, %v", repository, err)
	}
	green := checksResult([]checkRun{{Status: "COMPLETED", Conclusion: "SUCCESS"}})
	failed := checksResult([]checkRun{{Status: "COMPLETED", Conclusion: "FAILURE"}})
	if green != checksGreen || failed != checksFailed {
		t.Fatalf("checks = %d, %d", green, failed)
	}
}

// Every case here previously reduced to checksGreen, and checksGreen is the
// orchestrator's authorisation to squash-merge agent-written code.
func TestChecksResultNeverGuessesGreen(t *testing.T) {
	for name, checks := range map[string][]checkRun{
		"no checks reported yet":       {},
		"unrecognised entry shape":     {{}},
		"completed with no conclusion": {{Status: "COMPLETED"}},
		"queued check run":             {{Status: "QUEUED"}},
		"legacy status context failed": {{State: "FAILURE"}},
		"legacy status context error":  {{State: "ERROR"}},
		"legacy status context queued": {{State: "PENDING"}},
		"one green one unknown":        {{Status: "COMPLETED", Conclusion: "SUCCESS"}, {Status: "QUEUED"}},
		"one green one failing":        {{Status: "COMPLETED", Conclusion: "SUCCESS"}, {State: "FAILURE"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := checksResult(checks); got == checksGreen {
				t.Fatal("checksResult reported green; the pull request would be merged without verified checks")
			}
		})
	}
}

func TestChecksResultAcceptsSettledSuccess(t *testing.T) {
	for name, checks := range map[string][]checkRun{
		"completed check run":          {{Status: "COMPLETED", Conclusion: "SUCCESS"}},
		"skipped and neutral":          {{Status: "COMPLETED", Conclusion: "SKIPPED"}, {Status: "COMPLETED", Conclusion: "NEUTRAL"}},
		"legacy status context passed": {{State: "SUCCESS"}},
	} {
		t.Run(name, func(t *testing.T) {
			if got := checksResult(checks); got != checksGreen {
				t.Fatalf("checksResult = %d, want green", got)
			}
		})
	}
}

func TestChecksDecodesLegacyStatusContexts(t *testing.T) {
	command := &fakeCommand{outputs: [][]byte{[]byte(`{"statusCheckRollup":[{"__typename":"StatusContext","context":"ci/jenkins","state":"FAILURE"}]}`)}}
	state, err := NewGitHubCLI(command, noToken).Checks(context.Background(), "p1", "acme/demo", "7")
	if err != nil {
		t.Fatal(err)
	}
	if state != checksFailed {
		t.Fatalf("Checks() = %d, want failed", state)
	}
}
