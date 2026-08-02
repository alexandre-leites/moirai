package server

import (
	"context"
	"testing"
)

type fakeCommand struct {
	outputs [][]byte
	calls   [][]string
}

func (fake *fakeCommand) Run(_ context.Context, args ...string) ([]byte, error) {
	fake.calls = append(fake.calls, args)
	output := fake.outputs[0]
	fake.outputs = fake.outputs[1:]
	return output, nil
}

func TestGitHubIssueParsing(t *testing.T) {
	command := &fakeCommand{outputs: [][]byte{[]byte(`[{"number":42,"title":"Fix","body":"","url":"https://github.com/acme/demo/issues/42","labels":[{"name":"agent:ready"},{"name":"agent-priority:7"}],"createdAt":"2026-01-01T00:00:00Z","updatedAt":"2026-01-02T00:00:00Z"}]`)}}
	issues, err := NewGitHubCLI(command).ListIssues(context.Background(), "acme/demo")
	if err != nil {
		t.Fatal(err)
	}
	if len(issues) != 1 || !issues[0].Eligible || issues[0].Priority != 7 || issues[0].ExternalID != "42" {
		t.Fatalf("issues = %#v", issues)
	}
	if command.calls[0][0] != "issue" {
		t.Fatalf("command = %#v", command.calls)
	}
}

func TestGitHubPRCreationFindsCreatedPR(t *testing.T) {
	command := &fakeCommand{outputs: [][]byte{
		[]byte(`[]`),
		[]byte(`https://github.com/acme/demo/pull/7\n`),
		[]byte(`[{"number":7,"url":"https://github.com/acme/demo/pull/7","state":"OPEN","headRefOid":"abc123"}]`),
	}}
	pr, err := NewGitHubCLI(command).FindOrCreatePR(context.Background(), "acme/demo", "agent/demo", "main", "Title", "Body")
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
	green := checksResult([]struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}{{Status: "COMPLETED", Conclusion: "SUCCESS"}})
	failed := checksResult([]struct {
		Status     string `json:"status"`
		Conclusion string `json:"conclusion"`
	}{{Status: "COMPLETED", Conclusion: "FAILURE"}})
	if green != checksGreen || failed != checksFailed {
		t.Fatalf("checks = %d, %d", green, failed)
	}
}
