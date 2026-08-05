package server

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/jackc/pgx/v5/pgtype"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	controlv1 "github.com/alexandre-leites/moirai/contracts/gen/control/v1"
	"github.com/loop-engineering/orchestrator/internal/idgen"
	"github.com/loop-engineering/orchestrator/internal/secrethash"
	"github.com/loop-engineering/orchestrator/internal/textutil"
)

// captureLogs swaps the default slog logger for one that writes to the
// returned buffer, restoring the previous logger on test cleanup.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	previous := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(previous) })
	return &buf
}

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
	hash, err := secrethash.PasswordHash("Correct1!")
	if err != nil {
		t.Fatal(err)
	}
	matched, err := secrethash.PasswordMatches("Correct1!", hash)
	if err != nil || !matched {
		t.Fatalf("password did not match: %v", err)
	}
	matched, err = secrethash.PasswordMatches("Wrong1!x", hash)
	if err != nil || matched {
		t.Fatalf("wrong password matched: %v", err)
	}
	if secrethash.ValidPassword("password") {
		t.Fatal("weak password accepted")
	}
}

func TestDeveloperPacket(t *testing.T) {
	packet, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "", 3600, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if packet["role"] != "developer" || packet["executionId"] != "1b5f4a4d-2345-4ff2-a014-189531caf2d7-implement" {
		t.Fatalf("unexpected packet: %#v", packet)
	}
}

// #276: every execution used to be dispatched with a hardcoded
// timeoutSeconds of 0, which the runner's task packet validation treated as
// "no deadline" -- a wedged agent then held its project lock forever, since
// the lease sweep only reclaims a job whose runner has stopped, not one whose
// agent process never returns. developerPacket must always carry the caller's
// resolved, positive timeout.
func TestDeveloperPacketSendsRealTimeout(t *testing.T) {
	packet, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "", 1800, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if packet["timeoutSeconds"] != 1800 {
		t.Fatalf("timeoutSeconds = %#v, want 1800", packet["timeoutSeconds"])
	}
}

func TestDeveloperPacketRejectsNonPositiveTimeout(t *testing.T) {
	if _, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "", 0, nil, nil); err == nil {
		t.Fatal("developerPacket accepted a zero timeout")
	}
}

func TestExecutionTimeoutSecondsFallsBackToDefault(t *testing.T) {
	if got := executionTimeoutSeconds(0); got != defaultExecutionTimeoutSeconds {
		t.Fatalf("executionTimeoutSeconds(0) = %d, want %d", got, defaultExecutionTimeoutSeconds)
	}
	if got := executionTimeoutSeconds(120); got != 120 {
		t.Fatalf("executionTimeoutSeconds(120) = %d, want 120", got)
	}
}

// The delivery step opens a pull request from the agent branch, which only
// exists on the remote if the runner was allowed to push it. The runner grants
// that to a developer packet and refuses it for a planner, so these two
// constraints are what stand between a scheduled job and a deliverable branch.
func TestDeveloperPacketMayModifyAndPush(t *testing.T) {
	packet, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "", 3600, nil, nil)
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

// A GitHub HTTPS managed clone must carry the GITHUB_TOKEN environment
// reference, or the runner has no credential to authenticate the clone with
// and git fails trying to prompt for one (#391).
func TestDeveloperPacketDeclaresGitHubTokenForGitHubClone(t *testing.T) {
	packet, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://github.com/acme/repo.git", "", "main", "agent/test", "", 3600, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	refs, ok := packet["environmentRefs"].([]map[string]string)
	if !ok || len(refs) != 1 || refs[0]["name"] != "GITHUB_TOKEN" || refs[0]["secretRef"] != "github_token" {
		t.Fatalf("environmentRefs = %#v, want the GITHUB_TOKEN reference", packet["environmentRefs"])
	}
}

// A clone the runner can already authenticate another way (an SSH key it
// declares, or a code host its git already trusts) declares nothing, so a
// project that never needed a GitHub token is not suddenly required to have
// one.
func TestDeveloperPacketDeclaresNoTokenForNonGitHubClone(t *testing.T) {
	packet, err := developerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://gitlab.example/acme/repo.git", "", "main", "agent/test", "", 3600, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if refs, ok := packet["environmentRefs"].([]map[string]string); !ok || len(refs) != 0 {
		t.Fatalf("environmentRefs = %#v, want none for a non-GitHub clone", packet["environmentRefs"])
	}
}

func TestRepositoryEnvironmentRefs(t *testing.T) {
	tests := []struct {
		name, mode, url string
		want            string
	}{
		{name: "github https managed clone", mode: "managed_clone", url: "https://github.com/acme/repo.git", want: "GITHUB_TOKEN"},
		{name: "github https without .git", mode: "managed_clone", url: "https://github.com/acme/repo", want: "GITHUB_TOKEN"},
		{name: "github ssh managed clone", mode: "managed_clone", url: "git@github.com:acme/repo.git", want: ""},
		{name: "non github https", mode: "managed_clone", url: "https://gitlab.com/acme/repo.git", want: ""},
		{name: "github over other scheme", mode: "managed_clone", url: "ssh://git@github.com/acme/repo.git", want: ""},
		{name: "existing path", mode: "existing_path", url: "https://github.com/acme/repo.git", want: ""},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			refs := repositoryEnvironmentRefs(test.mode, test.url)
			if test.want == "" {
				if len(refs) != 0 {
					t.Fatalf("repositoryEnvironmentRefs(%q, %q) = %#v, want none", test.mode, test.url, refs)
				}
				return
			}
			if len(refs) != 1 || refs[0]["name"] != test.want {
				t.Fatalf("repositoryEnvironmentRefs(%q, %q) = %#v, want name %q", test.mode, test.url, refs, test.want)
			}
		})
	}
}

// A planner packet (#351) must never be allowed to modify files or push --
// the runner's own taskpacket.Validate refuses that combination independently,
// but plannerPacket must not rely solely on the runner catching its mistake.
func TestPlannerPacketMayNotModifyOrPush(t *testing.T) {
	packet, err := plannerPacket("1b5f4a4d-2345-4ff2-a014-189531caf2d7", "2b5f4a4d-2345-4ff2-a014-189531caf2d7", "42", "Fix scheduler", "", "managed_clone", "https://example.test/repo.git", "", "main", "agent/test", "", 3600)
	if err != nil {
		t.Fatal(err)
	}
	if packet["role"] != "planner" || packet["executionId"] != "1b5f4a4d-2345-4ff2-a014-189531caf2d7-plan" {
		t.Fatalf("unexpected packet: %#v", packet)
	}
	constraints, ok := packet["constraints"].(map[string]bool)
	if !ok {
		t.Fatalf("constraints = %#v", packet["constraints"])
	}
	if constraints["mayModifyFiles"] || constraints["mayPush"] || constraints["mayMerge"] {
		t.Fatal("a planner packet must not be allowed to modify files, push, or merge")
	}
}

// planFromPayload is what folds a planner execution's terminal payload into
// the developer packet dispatched afterward: the summary first, then each
// remaining-work entry, in the order promptFor renders packet.Plan under
// "# CURRENT PLAN".
func TestPlanFromPayload(t *testing.T) {
	plan := planFromPayload(`{"status":"completed","summary":"Add a cache","remainingWork":["Write the struct","Wire it up"]}`)
	want := []string{"Add a cache", "Write the struct", "Wire it up"}
	if len(plan) != len(want) {
		t.Fatalf("plan = %#v, want %#v", plan, want)
	}
	for i := range want {
		if plan[i] != want[i] {
			t.Fatalf("plan = %#v, want %#v", plan, want)
		}
	}
	if plan := planFromPayload(`{"status":"completed"}`); len(plan) != 0 {
		t.Fatalf("plan = %#v, want empty for a payload with no summary or remaining work", plan)
	}
	if plan := planFromPayload(`not json`); plan != nil {
		t.Fatalf("plan = %#v, want nil for a payload that does not parse", plan)
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
	response, err := (&ControlServer{Core: &Core{version: "922d6e6f0a6c"}}).GetSystemVersion(context.Background(), &controlv1.GetSystemVersionRequest{})
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
	if !idgen.ValidID("1b5f4a4d-2345-4ff2-a014-189531caf2d7") || idgen.ValidID("not-a-uuid") {
		t.Fatal("identifier validation is incorrect")
	}
}

// literalUnicodeEscape builds the six-character JSON `\uXXXX` escape sequence
// for the given four hex digits out of individual bytes, rather than as a Go
// string literal spelling the escape directly, purely so this source file
// never contains that literal backslash-u sequence itself.
func literalUnicodeEscape(hex string) string {
	return string([]byte{0x5c, 'u'}) + hex
}

// TestSanitizeEventPayloadLeavesOrdinaryPayloadsByteForByteAlone pins the
// fast path: a payload with no `\u` escape at all -- the overwhelming
// majority of runner events -- must come back identical, not merely
// equivalent, so an ordinary event's stored bytes (and object key order)
// never change.
func TestSanitizeEventPayloadLeavesOrdinaryPayloadsByteForByteAlone(t *testing.T) {
	payload := `{"summary":"plain text","remainingWork":["a","b"],"exitCode":0}`
	if got := sanitizeEventPayload(payload); got != payload {
		t.Fatalf("sanitizeEventPayload(%q) = %q, want it unchanged", payload, got)
	}
}

// TestSanitizeEventPayloadReplacesANULByteAtAnyDepth is the core of #302: a
// NUL byte reaches persistExecutionEvent as a backslash-u-0000 escape (the only
// spelling json.Valid accepts for it -- a raw NUL byte fails json.Valid
// outright and never reaches this function), and Postgres's jsonb refuses to
// store it verbatim. This pins that sanitizeEventPayload finds and replaces
// it wherever it sits: a top-level field, nested inside an object several
// levels deep, inside an array element, and inside an object key -- not just
// the top-level string field a narrower fix might have special-cased.
func TestSanitizeEventPayloadReplacesANULByteAtAnyDepth(t *testing.T) {
	nul := literalUnicodeEscape("0000")
	payload := `{"summary":"a` + nul + `b","result":{"raw":"c` + nul + `d","deep":["e` + nul + `f",{"k` + nul + `ey":"g` + nul + `h"}]}}`

	got := sanitizeEventPayload(payload)

	if !json.Valid([]byte(got)) {
		t.Fatalf("sanitizeEventPayload(%q) = %q, not valid JSON", payload, got)
	}
	if strings.ContainsRune(got, 0) || strings.Contains(got, nul) {
		t.Fatalf("sanitizeEventPayload(%q) = %q, still carries a NUL byte or its escape", payload, got)
	}
	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("re-decoding the sanitized payload: %v", err)
	}
	if !strings.Contains(decoded["summary"].(string), "a") || !strings.HasSuffix(decoded["summary"].(string), "b") {
		t.Fatalf("summary lost content around the replaced NUL: %q", decoded["summary"])
	}
	result := decoded["result"].(map[string]any)
	if raw, _ := result["raw"].(string); !strings.HasPrefix(raw, "c") || !strings.HasSuffix(raw, "d") {
		t.Fatalf("result.raw lost content around the replaced NUL: %q", raw)
	}
	deep := result["deep"].([]any)
	if s, _ := deep[0].(string); !strings.HasPrefix(s, "e") || !strings.HasSuffix(s, "f") {
		t.Fatalf("result.deep[0] lost content around the replaced NUL: %q", s)
	}
	nested := deep[1].(map[string]any)
	if len(nested) != 1 {
		t.Fatalf("result.deep[1] has %d keys, want the one NUL-bearing key sanitized in place: %v", len(nested), nested)
	}
	for k, v := range nested {
		if strings.ContainsRune(k, 0) {
			t.Fatalf("object key %q still carries a NUL byte", k)
		}
		if s, _ := v.(string); !strings.HasPrefix(s, "g") || !strings.HasSuffix(s, "h") {
			t.Fatalf("result.deep[1] value lost content around the replaced NUL: %q", s)
		}
	}
}

// TestSanitizeEventPayloadReplacesAnUnpairedSurrogate pins the other
// documented case: a lone UTF-16 surrogate half inside a `\uXXXX` escape is,
// like a NUL byte, legal as far as json.Valid is concerned but rejected by
// jsonb ("invalid input syntax for type json ... Unicode low surrogate must
// follow a high surrogate"). encoding/json already folds it to the Unicode
// replacement character on decode, and re-marshalling produces valid UTF-8,
// so sanitizeEventPayload fixes this case for free once it decodes the
// payload at all.
func TestSanitizeEventPayloadReplacesAnUnpairedSurrogate(t *testing.T) {
	lone := literalUnicodeEscape("d800")
	payload := `{"summary":"a` + lone + `b"}`

	got := sanitizeEventPayload(payload)

	if !json.Valid([]byte(got)) {
		t.Fatalf("sanitizeEventPayload(%q) = %q, not valid JSON", payload, got)
	}
	if strings.Contains(got, lone) {
		t.Fatalf("sanitizeEventPayload(%q) = %q, still carries the unpaired surrogate escape", payload, got)
	}
}

// TestSanitizeEventPayloadPreservesLargeIntegers guards against a decode
// path that looks correct but silently corrupts data: unmarshalling into
// `any` with encoding/json's default number handling turns every JSON number
// into a float64, which cannot represent every int64 exactly. sanitizeEventPayload
// must use json.Decoder.UseNumber so a large integer elsewhere in the same
// payload that also happens to need NUL sanitization survives the round trip
// unrounded.
func TestSanitizeEventPayloadPreservesLargeIntegers(t *testing.T) {
	nul := literalUnicodeEscape("0000")
	payload := `{"summary":"a` + nul + `b","count":9007199254740993}`

	got := sanitizeEventPayload(payload)

	var decoded map[string]any
	if err := json.Unmarshal([]byte(got), &decoded); err != nil {
		t.Fatalf("re-decoding the sanitized payload: %v", err)
	}
	if !strings.Contains(got, "9007199254740993") {
		t.Fatalf("sanitizeEventPayload(%q) = %q, large integer was rounded through float64", payload, got)
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
		if !strings.Contains(predicate, "'"+state.String()+"'") {
			t.Fatalf("workflow_runs_active_idx excludes %s but terminalStatuses has %q; the gauge's count no longer matches its index", predicate, state)
		}
	}
	if excluded := strings.Count(predicate, "'"); excluded != 2*len(terminalStatuses) {
		t.Fatalf("workflow_runs_active_idx excludes %s, which is not the %d statuses terminalStatuses lists", predicate, len(terminalStatuses))
	}
}

func TestDatabaseErrorKeepsWireMessageOpaqueButLogsCause(t *testing.T) {
	logs := captureLogs(t)

	cause := errors.New("pq: duplicate key value violates unique constraint \"projects_name_key\"")
	err := databaseError(cause)

	if err == nil {
		t.Fatal("expected a non-nil error")
	}
	if got, want := status.Code(err), codes.Internal; got != want {
		t.Fatalf("code = %v, want %v", got, want)
	}
	// The client-facing message must stay exactly this constant: an operator
	// or console must never learn the underlying cause from the wire.
	const wantMessage = "database operation failed"
	if got := status.Convert(err).Message(); got != wantMessage {
		t.Fatalf("message = %q, want %q", got, wantMessage)
	}
	if strings.Contains(status.Convert(err).Message(), "constraint") {
		t.Fatal("the real cause leaked onto the wire")
	}
	// But the real cause must have been logged server-side.
	if !strings.Contains(logs.String(), "duplicate key value violates unique constraint") {
		t.Fatalf("expected the underlying cause to be logged, got: %s", logs.String())
	}
}

func TestDatabaseErrorPassesThroughExistingStatusErrors(t *testing.T) {
	logs := captureLogs(t)

	notFound := status.Error(codes.NotFound, "project is unknown")
	if got := databaseError(notFound); got != notFound {
		t.Fatalf("expected an existing status error to pass through unchanged, got %v", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("expected no log for an error that already carries a gRPC status, got: %s", logs.String())
	}
}

func TestDatabaseErrorNilIsNil(t *testing.T) {
	if err := databaseError(nil); err != nil {
		t.Fatalf("databaseError(nil) = %v, want nil", err)
	}
}

func TestDatabaseErrorDoesNotLogContextCancellation(t *testing.T) {
	logs := captureLogs(t)

	canceled := databaseError(context.Canceled)
	if got, want := status.Code(canceled), codes.Canceled; got != want {
		t.Fatalf("code = %v, want %v", got, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("a canceled context must not be logged as a database failure, got: %s", logs.String())
	}

	deadline := databaseError(context.DeadlineExceeded)
	if got, want := status.Code(deadline), codes.DeadlineExceeded; got != want {
		t.Fatalf("code = %v, want %v", got, want)
	}
	if logs.Len() != 0 {
		t.Fatalf("a deadline-exceeded context must not be logged as a database failure, got: %s", logs.String())
	}
}

func TestConfigurationErrorIsDistinctFromDatabaseError(t *testing.T) {
	logs := captureLogs(t)

	cause := errors.New("unexpected end of JSON input")
	err := configurationError(cause)

	if got := status.Convert(err).Message(); got == "database operation failed" {
		t.Fatal("a malformed JSON column must not report \"database operation failed\"")
	}
	if !strings.Contains(logs.String(), "unexpected end of JSON input") {
		t.Fatalf("expected the underlying cause to be logged, got: %s", logs.String())
	}
}

func TestScanRunnerRowValuesRoutesMalformedLabelsThroughConfigurationError(t *testing.T) {
	logs := captureLogs(t)

	_, err := scanRunnerRowValues("runner-1", "demo", true, false, "online", "{not valid json", pgtype.Timestamptz{}, "1.0.0")
	if err == nil {
		t.Fatal("expected an error for malformed labels JSON")
	}
	if got := status.Convert(err).Message(); got == "database operation failed" {
		t.Fatal("malformed runner labels must not report \"database operation failed\"")
	}
	if logs.Len() == 0 {
		t.Fatal("expected the malformed-labels cause to be logged")
	}
}

func TestConfiguredCipherDistinguishesMisconfiguredFromUnset(t *testing.T) {
	logs := captureLogs(t)

	t.Setenv("LOOP_SECRET_KEY", "value")
	t.Setenv("LOOP_SECRET_KEY_FILE", "also-set")
	_, err := configuredCipher()
	if err == nil {
		t.Fatal("expected an error when both LOOP_SECRET_KEY and _FILE are set")
	}
	const want = "LOOP_SECRET_KEY is misconfigured"
	if err.Error() != want {
		t.Fatalf("error = %q, want %q", err.Error(), want)
	}
	if logs.Len() == 0 {
		t.Fatal("expected the misconfiguration cause to be logged")
	}
}

func TestPasswordMatchesLogsUnparseableHashWithoutChangingItsResult(t *testing.T) {
	logs := captureLogs(t)

	matches, err := secrethash.PasswordMatches("any-password", "not-a-valid-scrypt-hash")
	if err != nil {
		t.Fatalf("passwordMatches error = %v, want nil (must stay indistinguishable from a wrong password)", err)
	}
	if matches {
		t.Fatal("expected no match for an unparseable hash")
	}
	if logs.Len() == 0 {
		t.Fatal("expected the unparseable-hash cause to be logged for an operator")
	}
}

// TestJsonLabelsPropagatesItsMarshalError pins #286: jsonLabels used to
// discard json.Marshal's error and hand the caller an empty string, which is
// not valid JSON but was still fed into the jsonb containment check that
// authorizes runner registration and into the labels stored on the runner
// row itself. []string cannot actually fail to marshal (every element is a
// valid, already-in-memory Go string), so the failure path here is
// correct-by-construction rather than reachable from production input; this
// test instead pins the two properties that make it safe to keep infallible:
// the happy path still produces the exact JSON encoding callers persist, and
// the function's shape is (string, error) so a hypothetical future failure
// cannot be silently reintroduced by a caller that stops checking it.
func TestJsonLabelsPropagatesItsMarshalError(t *testing.T) {
	encoded, err := jsonLabels([]string{"linux", "docker"})
	if err != nil {
		t.Fatalf("jsonLabels error = %v, want nil for a valid label set", err)
	}
	if encoded != `["linux","docker"]` {
		t.Fatalf("jsonLabels = %q, want the exact json.Marshal encoding", encoded)
	}

	empty, err := jsonLabels(nil)
	if err != nil {
		t.Fatalf("jsonLabels(nil) error = %v, want nil", err)
	}
	if empty != "null" {
		t.Fatalf("jsonLabels(nil) = %q, want the literal JSON null json.Marshal(nil) produces", empty)
	}
}

// TestParseIntRejectsMalformedInput pins #287: parseInt used to be built on
// fmt.Sscan, which scans a leading numeric prefix and reports success even
// when characters remain unconsumed ("123abc" silently became 123), and on
// total failure ("abc") left its destination holding whatever the caller's
// variable held before the call rather than a defined value. ListWorkflowEvents
// built its pagination cursor straight from this result while discarding the
// error, so either failure mode could hand a client a wrong cursor and no
// signal that anything was wrong. strconv.ParseInt must reject both forms
// outright.
func TestParseIntRejectsMalformedInput(t *testing.T) {
	for _, value := range []string{"123abc", "abc", "", "12.5", " 123", "123 ", "0x1F"} {
		if _, err := textutil.ParseInt(value); err == nil {
			t.Fatalf("textutil.ParseInt(%q): want an error, got none", value)
		}
	}
}

func TestParseIntAcceptsValidIntegers(t *testing.T) {
	for value, want := range map[string]int64{"0": 0, "123": 123, "9223372036854775807": 9223372036854775807} {
		got, err := textutil.ParseInt(value)
		if err != nil {
			t.Fatalf("textutil.ParseInt(%q): unexpected error %v", value, err)
		}
		if got != want {
			t.Fatalf("textutil.ParseInt(%q) = %d, want %d", value, got, want)
		}
	}
}

// TestSyncErrorMessageStripsGRPCStatusWrapper pins #287: SyncNow used to
// report a per-project failure with err.Error(), which for anything already
// wrapped by databaseError/configurationError rendered the gRPC status
// wrapper verbatim ("rpc error: code = Internal desc = database operation
// failed") on ProjectSyncResult.Error -- a field an operator reads directly,
// not another gRPC hop. syncErrorMessage must strip that wrapper down to the
// bare message.
func TestSyncErrorMessageStripsGRPCStatusWrapper(t *testing.T) {
	logs := captureLogs(t)
	wrapped := databaseError(errors.New("pq: connection reset"))

	got := syncErrorMessage(wrapped)

	if strings.Contains(got, "rpc error") {
		t.Fatalf("syncErrorMessage(%v) = %q, want the gRPC status wrapper stripped", wrapped, got)
	}
	const want = "database operation failed"
	if got != want {
		t.Fatalf("syncErrorMessage = %q, want %q", got, want)
	}
	_ = logs // databaseError already has its own logging test; this just avoids polluting test output.
}

// TestSyncErrorMessagePassesThroughPlainErrors is the adversarial half of the
// fix above: a plain Go error (e.g. a malformed repository URL) never went
// through status.Error, so it must come out exactly as its own message
// rather than something mangled by treating it as a status.
func TestSyncErrorMessagePassesThroughPlainErrors(t *testing.T) {
	plain := errors.New("repository URL is invalid")

	if got := syncErrorMessage(plain); got != plain.Error() {
		t.Fatalf("syncErrorMessage(%v) = %q, want %q unchanged", plain, got, plain.Error())
	}
}

// TestLoginsDummyHashForUnknownUsernamesStillParses pins the username-
// enumeration defence Login runs for a username that does not exist: it
// still calls passwordMatches against this exact hardcoded scrypt hash, so an
// unknown username costs the same wall-clock time as a wrong password for a
// real one. That defence is silent by construction (Login returns the same
// "login was rejected" either way), so the only way to pin it at all is to
// confirm the literal hash it depends on still parses as valid scrypt$...
// input and never matches -- if this hash ever bit-rots (a typo on edit,
// wrong field count), passwordMatches would start returning a parse error
// nothing here would otherwise catch, and the dummy comparison would stop
// costing anything close to a real one.
func TestLoginsDummyHashForUnknownUsernamesStillParses(t *testing.T) {
	logs := captureLogs(t)
	matches, err := secrethash.PasswordMatches("whatever-password", "scrypt$16384$8$1$AAAAAAAAAAAAAAAAAAAAAA$AAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAAA")
	if err != nil {
		t.Fatalf("Login's dummy hash failed to parse: %v", err)
	}
	if matches {
		t.Fatal("Login's dummy hash matched an arbitrary password")
	}
	if logs.Len() != 0 {
		t.Fatalf("a hash that parses fine logged something unexpected: %s", logs.String())
	}
}
