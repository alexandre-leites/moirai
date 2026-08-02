package server

import (
	"context"
	"strings"
	"testing"

	controlv1 "github.com/loop-engineering/contracts/gen/control/v1"
)

func TestValidateProject(t *testing.T) {
	project, steps, err := validateProject(&controlv1.ProjectConfiguration{
		Name:                 " demo ",
		RepositoryMode:       "managed_clone",
		RepositoryUrl:        "https://example.test/demo.git",
		DefaultBranch:        "main",
		RequiredRunnerLabels: []string{"go", "docker"},
		PipelineSteps:        []*controlv1.PipelineStep{{Command: "go test ./...", TimeoutSeconds: 60, Position: 0, Required: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if project.GetName() != "demo" || len(steps) != 1 || project.GetRequiredRunnerLabels()[0] != "go" {
		t.Fatalf("unexpected project: %#v", project)
	}
}

func TestValidateProjectRejectsInvalidConfiguration(t *testing.T) {
	_, _, err := validateProject(&controlv1.ProjectConfiguration{
		Name:           "demo",
		RepositoryMode: "managed_clone",
		DefaultBranch:  "main",
	})
	if err == nil {
		t.Fatal("expected invalid project error")
	}
}

func TestPasswordHashCompatibility(t *testing.T) {
	hash, err := passwordHash("Correct1!")
	if err != nil {
		t.Fatal(err)
	}
	matched, err := passwordMatches("Correct1!", hash)
	if err != nil || !matched {
		t.Fatalf("password did not match: %v", err)
	}
	matched, err = passwordMatches("Wrong1!x", hash)
	if err != nil || matched {
		t.Fatalf("wrong password matched: %v", err)
	}
	if validPassword("password") {
		t.Fatal("weak password accepted")
	}
}

func TestDeveloperPacket(t *testing.T) {
	packet, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "")
	if err != nil {
		t.Fatal(err)
	}
	if packet["role"] != "developer" || packet["executionId"] != "1b5f4a4d-2345-4ff2-a014-189531caf2d7-implement" {
		t.Fatalf("unexpected packet: %#v", packet)
	}
}

// The delivery step opens a pull request from the agent branch, which only
// exists on the remote if the runner was allowed to push it. The runner grants
// that to a developer packet and refuses it for a planner, so these two
// constraints are what stand between a scheduled job and a deliverable branch.
func TestDeveloperPacketMayModifyAndPush(t *testing.T) {
	packet, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "")
	if err != nil {
		t.Fatal(err)
	}
	constraints, ok := packet["constraints"].(map[string]bool)
	if !ok {
		t.Fatalf("constraints = %#v", packet["constraints"])
	}
	if !constraints["mayModifyFiles"] || !constraints["mayPush"] {
		t.Fatal("the dispatched execution cannot write or publish code, so delivery can never open a pull request")
	}
	if constraints["mayMerge"] {
		t.Fatal("merging is the orchestrator's decision; the runner rejects a packet that claims it")
	}
}

func TestProjectCredentialCipherAndValidation(t *testing.T) {
	t.Setenv("LOOP_SECRET_KEY", "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA=")
	t.Setenv("LOOP_SECRET_KEY_FILE", "")
	aead, err := configuredCipher()
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, aead.NonceSize())
	ciphertext := aead.Seal(nil, nonce, []byte("secret"), nil)
	plaintext, err := aead.Open(nil, nonce, ciphertext, nil)
	if err != nil || string(plaintext) != "secret" {
		t.Fatalf("plaintext=%q err=%v", plaintext, err)
	}
	if kind, err := validateCredential("agent:OPENROUTER_API_KEY", ".config/token"); err != nil || kind != "agent:OPENROUTER_API_KEY" {
		t.Fatalf("credential=%q err=%v", kind, err)
	}
	if _, err := validateCredential("github_token", ".config/token"); err == nil {
		t.Fatal("git token file path accepted")
	}
	if _, err := validateCredential("agent:OPENROUTER_API_KEY", "."); err == nil {
		t.Fatal("current directory file path accepted")
	}
	if kind, err := environmentCredentialKind("GITHUB_TOKEN"); err != nil || kind != "github_token" {
		t.Fatalf("kind=%q err=%v", kind, err)
	}
}

// The API gateway reports the orchestrator build on its public health endpoint
// and has no session to present, so this call must succeed without one.
func TestGetSystemVersionNeedsNoSession(t *testing.T) {
	response, err := (&Server{version: "922d6e6f0a6c"}).GetSystemVersion(context.Background(), &controlv1.GetSystemVersionRequest{})
	if err != nil {
		t.Fatalf("GetSystemVersion() error = %v; the public health endpoint reports an empty version", err)
	}
	if response.GetVersion() != "922d6e6f0a6c" {
		t.Fatalf("Version = %q", response.GetVersion())
	}
}

func TestReservedCredentialNamesAreRefused(t *testing.T) {
	for _, name := range []string{"PATH", "HOME", "LD_PRELOAD", "GIT_SSH_COMMAND", "NODE_OPTIONS"} {
		if _, err := validateCredential("agent:"+name, ""); err == nil {
			t.Fatalf("validateCredential accepted %s, which redirects the agent's toolchain rather than carrying a secret", name)
		}
		if _, err := environmentCredentialKind(name); err == nil {
			t.Fatalf("environmentCredentialKind accepted %s", name)
		}
	}
	if _, err := validateCredential("agent:OPENROUTER_API_KEY", ""); err != nil {
		t.Fatalf("an ordinary credential name was refused: %v", err)
	}
}

func TestIssueLabelsGateEligibility(t *testing.T) {
	if _, eligible := issuePriority([]string{"agent:ready"}); !eligible {
		t.Fatal("agent:ready did not make the issue eligible")
	}
	for _, label := range []string{"agent:blocked", "agent:delivered"} {
		priority, eligible := issuePriority([]string{"agent:ready", "agent-priority:7", label})
		if eligible {
			t.Fatalf("%s did not stop autonomous work on the issue", label)
		}
		if priority != 7 {
			t.Fatalf("priority = %d, want 7", priority)
		}
	}
}

func TestGitHubErrorsRedactTokens(t *testing.T) {
	for _, secret := range []string{"ghp_0123456789abcdefghij", "github_pat_0123456789abcdefghij"} {
		if got := redactSecrets("failed using " + secret); strings.Contains(got, secret) {
			t.Fatalf("redactSecrets left the token in %q; it reaches operator-visible error fields", got)
		}
	}
}

func TestEventSeverityMarksFailures(t *testing.T) {
	if eventSeverity("failed") != "error" || eventSeverity("cancelled") != "warning" || eventSeverity("log") != "info" {
		t.Fatal("runner event severities are wrong")
	}
}

func TestEventAndIdentifierValidation(t *testing.T) {
	if !validEventType("completed") || validEventType("retry") || !terminalEvent("failed") || terminalEvent("progress") {
		t.Fatal("event validation is incorrect")
	}
	if !validID("1b5f4a4d-2345-4ff2-a014-189531caf2d7") || validID("not-a-uuid") {
		t.Fatal("identifier validation is incorrect")
	}
}
