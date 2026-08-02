package server

import (
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

func TestPlannerPacket(t *testing.T) {
	packet, err := plannerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "")
	if err != nil {
		t.Fatal(err)
	}
	if packet["role"] != "planner" || packet["executionId"] != "1b5f4a4d-2345-4ff2-a014-189531caf2d7-plan" {
		t.Fatalf("unexpected packet: %#v", packet)
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

func TestEventAndIdentifierValidation(t *testing.T) {
	if !validEventType("completed") || validEventType("retry") || !terminalEvent("failed") || terminalEvent("progress") {
		t.Fatal("event validation is incorrect")
	}
	if !validID("1b5f4a4d-2345-4ff2-a014-189531caf2d7") || validID("not-a-uuid") {
		t.Fatal("identifier validation is incorrect")
	}
}
