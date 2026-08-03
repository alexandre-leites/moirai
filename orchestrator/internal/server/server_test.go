package server

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"unicode/utf8"

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

// An agent-declared block is the one terminal payload the orchestrator reads
// rather than only stores. The payload is agent-supplied, so the whole surface
// is pinned here: what counts as a declaration, what a declaration composes
// into, and what must fall back to the ordinary `failed` outcome rather than
// fail the event.
//
// This half matters most. Misreading a crash as a deliberate block is worse
// than the bug being fixed: it files a real failure under "a human decided to
// stop", where nobody is looking for it.
func TestAgentBlockReasonReadsOnlyAGenuineDeclaration(t *testing.T) {
	for name, payload := range map[string]string{
		"a crash with no block marker":  `{"status":"failed","exitCode":1,"error":"agent exited 1"}`,
		"an empty payload":              `{}`,
		"a payload that is not object":  `"blocked"`,
		"a payload that is a list":      `[{"blocked":true}]`,
		"a null blocked field":          `{"blocked":null,"summary":"x"}`,
		"a false blocked field":         `{"blocked":false,"summary":"x"}`,
		"a stringly-typed blocked flag": `{"blocked":"true","summary":"x"}`,
		"a numeric blocked flag":        `{"blocked":1,"summary":"x"}`,
		"a blocked object":              `{"blocked":{"yes":true}}`,
		// A bare `status: "blocked"` is what a hand-built payload looks like.
		// The runner sets the flag alongside it, and the flag is the contract.
		"a status without the flag": `{"status":"blocked","summary":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			if reason, blocked := agentBlockReason(payload); blocked {
				t.Fatalf("agentBlockReason(%s) = (%q, true); a run that did not declare a block must stay failed", payload, reason)
			}
		})
	}
}

func TestAgentBlockReasonComposesTheAgentsAccount(t *testing.T) {
	reason, blocked := agentBlockReason(`{"status":"blocked","blocked":true,"exitCode":0,"summary":"the deployment credential is missing","remainingWork":["obtain DEPLOY_KEY","re-run the migration"]}`)
	if !blocked {
		t.Fatal("a payload marked blocked:true was not read as a block")
	}
	want := "the agent reported itself blocked: the deployment credential is missing (remaining work: obtain DEPLOY_KEY; re-run the migration)"
	if reason != want {
		t.Fatalf("reason = %q, want %q", reason, want)
	}
}

// A declaration with nothing said is still a declaration: the run is blocked,
// and blocking_reason must not come out empty, because an empty one renders as
// "No reason was recorded" -- indistinguishable from the bug being fixed.
func TestAgentBlockReasonAlwaysCarriesAReason(t *testing.T) {
	for name, payload := range map[string]string{
		"no summary at all":            `{"blocked":true}`,
		"a blank summary":              `{"blocked":true,"summary":"   "}`,
		"a summary of the wrong type":  `{"blocked":true,"summary":42,"remainingWork":["ask a human"]}`,
		"remaining work of wrong type": `{"blocked":true,"remainingWork":"lots"}`,
		"remaining work of blanks":     `{"blocked":true,"remainingWork":["","  "]}`,
		"remaining work part-typed":    `{"blocked":true,"remainingWork":["ask a human",7]}`,
	} {
		t.Run(name, func(t *testing.T) {
			reason, blocked := agentBlockReason(payload)
			if !blocked {
				t.Fatalf("agentBlockReason(%s) did not read the declaration", payload)
			}
			if !strings.HasPrefix(reason, agentBlockPrefix) {
				t.Fatalf("reason = %q, want it to open with %q", reason, agentBlockPrefix)
			}
		})
	}
}

// The reason is agent prose on its way to a text column and an operator's
// screen. PostgreSQL rejects a NUL byte outright -- the terminal event would
// fail and the run would keep its project lock forever -- and an escape
// sequence in an operator-facing field is a terminal-injection vector.
func TestAgentBlockReasonSanitizesAgentProse(t *testing.T) {
	// The escapes are JSON, not Go: the raw string carries them through so the
	// payload really does contain an ANSI introducer and the NUL byte
	// PostgreSQL refuses on a text column.
	reason, blocked := agentBlockReason(`{"blocked":true,"summary":"line one \u001b[31mred\u0000\nline two","remainingWork":["tab\there"]}`)
	if !blocked {
		t.Fatal("the declaration was not read")
	}
	if strings.ContainsAny(reason, "\x00\x1b\n\t") {
		t.Fatalf("reason = %q still carries a control character", reason)
	}
	if !strings.Contains(reason, "line one") || !strings.Contains(reason, "line two") || !strings.Contains(reason, "tab here") {
		t.Fatalf("reason = %q lost the agent's words along with the control characters", reason)
	}
}

// The runner bounds the summary and the remaining work it sends, but the
// orchestrator does not get to assume a runner it does not control did so.
func TestAgentBlockReasonIsBounded(t *testing.T) {
	// Multi-byte throughout, so a byte-sliced bound would leave invalid UTF-8
	// and PostgreSQL would reject the write.
	entries := make([]string, 64)
	for index := range entries {
		entries[index] = strings.Repeat("汉", 500)
	}
	encoded, err := json.Marshal(map[string]any{"blocked": true, "summary": strings.Repeat("é", 8000), "remainingWork": entries})
	if err != nil {
		t.Fatal(err)
	}
	reason, blocked := agentBlockReason(string(encoded))
	if !blocked {
		t.Fatal("the declaration was not read")
	}
	if len(reason) > maxBlockingReasonBytes {
		t.Fatalf("reason is %d bytes, over the %d byte bound blocking_reason carries", len(reason), maxBlockingReasonBytes)
	}
	if !utf8.ValidString(reason) {
		t.Fatalf("reason %q is not valid UTF-8; PostgreSQL rejects it on a text column", reason)
	}
	if !strings.HasSuffix(reason, reasonTruncationMarker) {
		t.Fatalf("reason ends %q, want the truncation to be marked", reason[max(0, len(reason)-32):])
	}
}

// moirai_active_workflows says "runs that have not reached a terminal status",
// and the SQL behind it is generated from this one list. Routing an agent block
// to `blocked` only keeps that promise while `blocked` is on the list -- and
// the partial index in 020_metrics_indexes.sql excludes exactly these four.
func TestBlockedIsATerminalStatusTheActiveGaugeExcludes(t *testing.T) {
	if !terminalStatus("blocked") {
		t.Fatal("blocked is not terminal, so moirai_active_workflows counts an agent-declared block as work still in flight")
	}
	for _, state := range []string{"completed", "failed", "blocked", "cancelled"} {
		if !terminalStatus(state) || !strings.Contains(terminalStatusList, "'"+state+"'") {
			t.Fatalf("terminalStatuses/%s omits %q; the gauge and the metrics index would disagree", terminalStatusList, state)
		}
	}
}
