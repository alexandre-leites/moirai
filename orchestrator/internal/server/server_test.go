package server

import (
	"context"
	"encoding/json"
	"os"
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

// existing_path projects can neither sync issues nor deliver workflows: both
// operations resolve GitHub coordinates from repository_url, and a local
// path gives them nothing to work with. validateProject must reject the mode
// outright, at configuration time, rather than let the project run a
// workflow to completion and only then fail at delivery.
func TestValidateProjectRejectsExistingPath(t *testing.T) {
	_, _, err := validateProject(&controlv1.ProjectConfiguration{
		Name:                "demo",
		RepositoryMode:      "existing_path",
		LocalRepositoryPath: "/repositories/demo",
		DefaultBranch:       "main",
	})
	if err == nil {
		t.Fatal("expected existing_path to be rejected")
	}
	message := err.Error()
	if !strings.Contains(message, "repository_mode") || !strings.Contains(message, "existing_path") {
		t.Fatalf("error should name the field and mode, got: %v", message)
	}
}

// The same validation must run on UpdateProject, not just CreateProject, so
// an existing project can't be edited into an existing_path configuration
// either.
func TestValidateProjectRejectsExistingPathEvenWithOtherwiseValidFields(t *testing.T) {
	_, _, err := validateProject(&controlv1.ProjectConfiguration{
		Name:                 "demo",
		RepositoryMode:       "existing_path",
		LocalRepositoryPath:  "/repositories/demo",
		DefaultBranch:        "main",
		RequiredRunnerLabels: []string{"go"},
		PipelineSteps:        []*controlv1.PipelineStep{{Command: "go test ./...", TimeoutSeconds: 60, Position: 0}},
	})
	if err == nil {
		t.Fatal("expected existing_path to be rejected even when all other fields are valid")
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

// A hex-encoded 32-byte key is 64 characters, a multiple of 4, and so also
// parses as valid (but wrong-length, 48-byte) base64. configuredCipher must
// try both encodings and accept whichever actually yields 32 bytes rather
// than stopping at the first successful-but-wrong-length parse.
func TestConfiguredCipherAcceptsBase64OrHex(t *testing.T) {
	t.Setenv("LOOP_SECRET_KEY_FILE", "")

	base64Key := "AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA="
	hexKey := strings.Repeat("ab", 32) // 64 hex chars decoding to 32 bytes

	t.Run("base64", func(t *testing.T) {
		t.Setenv("LOOP_SECRET_KEY", base64Key)
		aead, err := configuredCipher()
		if err != nil {
			t.Fatalf("configuredCipher() error = %v", err)
		}
		if aead.NonceSize() <= 0 {
			t.Fatal("expected a usable AEAD")
		}
	})

	t.Run("hex", func(t *testing.T) {
		t.Setenv("LOOP_SECRET_KEY", hexKey)
		aead, err := configuredCipher()
		if err != nil {
			t.Fatalf("configuredCipher() error = %v; a valid 64-char hex key must be accepted", err)
		}
		if aead.NonceSize() <= 0 {
			t.Fatal("expected a usable AEAD")
		}
	})

	t.Run("invalid", func(t *testing.T) {
		t.Setenv("LOOP_SECRET_KEY", "not-a-valid-key")
		_, err := configuredCipher()
		if err == nil {
			t.Fatal("expected an error for a key that decodes to neither 32 bytes of base64 nor hex")
		}
		const want = "secret key must decode to 32 bytes from base64 or hex"
		if err.Error() != want {
			t.Fatalf("error = %q, want %q", err.Error(), want)
		}
	})
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

// Unicode format characters are the same lie as a terminal escape and reach
// further, because HTML escaping does not stop them: a right-to-left override
// makes the console render a reason as text other than the one stored. None of
// them is a control character as far as unicode.IsControl is concerned.
func TestAgentBlockReasonStripsBidiAndFormatCharacters(t *testing.T) {
	// U+202E right-to-left override, U+2066 isolate, U+200B zero-width space,
	// U+FEFF byte-order mark, U+00AD soft hyphen.
	for _, hidden := range []string{"\u202e", "\u2066", "\u200b", "\ufeff", "\u00ad"} {
		encoded, err := json.Marshal(map[string]any{"blocked": true, "summary": "credential " + hidden + "rotated"})
		if err != nil {
			t.Fatal(err)
		}
		reason, blocked := agentBlockReason(string(encoded))
		if !blocked {
			t.Fatalf("the declaration carrying %+q was not read", hidden)
		}
		if strings.Contains(reason, hidden) {
			t.Fatalf("reason = %+q kept %+q, so the console can render it as text other than what is stored", reason, hidden)
		}
		if !strings.Contains(reason, "credential") || !strings.Contains(reason, "rotated") {
			t.Fatalf("reason = %q lost the agent's words along with %+q", reason, hidden)
		}
	}
}

// The runner bounds the summary and the remaining work it sends, but the
// orchestrator does not get to assume a runner it does not control did so.
// Every case here is multi-byte throughout, because a byte-sliced bound would
// leave invalid UTF-8 and PostgreSQL rejects that on a text column.
func TestAgentBlockReasonIsBounded(t *testing.T) {
	long := func(count int) []string {
		entries := make([]string, count)
		for index := range entries {
			entries[index] = strings.Repeat("汉", 500)
		}
		return entries
	}
	for name, payload := range map[string]map[string]any{
		"one oversized summary":       {"blocked": true, "summary": strings.Repeat("é", 8000)},
		"one oversized entry":         {"blocked": true, "summary": "short", "remainingWork": long(1)},
		"many oversized entries":      {"blocked": true, "summary": "short", "remainingWork": long(64)},
		"an oversized summary and 64": {"blocked": true, "summary": strings.Repeat("é", 8000), "remainingWork": long(64)},
		"many short entries":          {"blocked": true, "remainingWork": strings.Split(strings.Repeat("汉汉汉 ", 200), " ")},
	} {
		t.Run(name, func(t *testing.T) {
			encoded, err := json.Marshal(payload)
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
			// An opened list must close. A reason trailing an unclosed
			// "(remaining work:" reads as malformed rather than as truncated.
			if strings.Contains(reason, remainingWorkLead) && !strings.HasSuffix(reason, ")") {
				t.Fatalf("reason ends %q with its remaining-work list unclosed", reason[max(0, len(reason)-40):])
			}
		})
	}
}

// A verbose agent must not be able to push its own list of remaining work out
// of the reason. Bounding the summary on its own did exactly that: the summary
// alone filled the budget, the loop broke on its first iteration, and the
// operator saw truncated prose with no sign that any remaining work was
// reported at all -- which is the actionable half of the account.
func TestAgentBlockReasonKeepsRemainingWorkBesideAVerboseSummary(t *testing.T) {
	encoded, err := json.Marshal(map[string]any{
		"blocked":       true,
		"summary":       strings.Repeat("prose ", 400),
		"remainingWork": []string{"obtain DEPLOY_KEY", "re-run the migration"},
	})
	if err != nil {
		t.Fatal(err)
	}
	reason, blocked := agentBlockReason(string(encoded))
	if !blocked {
		t.Fatal("the declaration was not read")
	}
	if !strings.Contains(reason, "prose") {
		t.Fatalf("reason = %q dropped the summary entirely", reason)
	}
	if !strings.Contains(reason, "obtain DEPLOY_KEY") {
		t.Fatalf("reason = %q lost the remaining work to a verbose summary", reason)
	}
	if len(reason) > maxBlockingReasonBytes {
		t.Fatalf("reason is %d bytes, over the %d byte bound", len(reason), maxBlockingReasonBytes)
	}
}

// moirai_active_workflows says "runs that have not reached a terminal status",
// and the SQL behind it is generated from `terminalStatuses`. Routing an agent
// block to `blocked` only keeps that promise while `blocked` is on that list.
//
// The migration is read rather than restated because it is an independent copy:
// `020_metrics_indexes.sql` hardcodes its own status list in a partial index,
// and a fifth terminal status added to the Go slice and not to the index leaves
// the gauge's count scanning the whole history instead of the in-flight set.
// Comparing the two is the only thing here that can fail on its own.
func TestBlockedIsATerminalStatusTheActiveGaugeExcludes(t *testing.T) {
	if !terminalStatus("blocked") {
		t.Fatal("blocked is not terminal, so moirai_active_workflows counts an agent-declared block as work still in flight")
	}
	migration, err := os.ReadFile("../../migrations/020_metrics_indexes.sql")
	if err != nil {
		t.Fatal(err)
	}
	_, definition, found := strings.Cut(string(migration), "workflow_runs_active_idx")
	if !found {
		t.Fatal("020_metrics_indexes.sql no longer defines workflow_runs_active_idx")
	}
	_, predicate, found := strings.Cut(definition, " WHERE ")
	if !found {
		t.Fatal("workflow_runs_active_idx is no longer partial, so it no longer tracks the terminal statuses at all")
	}
	predicate, _, _ = strings.Cut(predicate, ";")
	for _, state := range terminalStatuses {
		if !strings.Contains(predicate, "'"+state+"'") {
			t.Fatalf("workflow_runs_active_idx excludes %s but terminalStatuses has %q; the gauge's count no longer matches its index", predicate, state)
		}
	}
	if excluded := strings.Count(predicate, "'"); excluded != 2*len(terminalStatuses) {
		t.Fatalf("workflow_runs_active_idx excludes %s, which is not the %d statuses terminalStatuses lists", predicate, len(terminalStatuses))
	}
}
