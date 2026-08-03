package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/loop-engineering/orchestrator/internal/db"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Command interface {
	Run(context.Context, string, ...string) ([]byte, error)
}

type execCommand struct{}

// githubTimeout bounds a single gh invocation. Delivery runs inline on the
// runner's control stream, so a gh that never returns would stall that runner's
// heartbeats and lease renewals as well as the workflow.
const githubTimeout = 60 * time.Second

func (execCommand) Run(parent context.Context, token string, args ...string) ([]byte, error) {
	ctx, cancel := context.WithTimeout(parent, githubTimeout)
	defer cancel()
	command := exec.CommandContext(ctx, "gh", args...)
	// A per-project token wins; the global file is the fallback for projects
	// that have not configured one. An empty token with no file leaves gh to
	// its own auth, which is the pre-existing behaviour for local development.
	if token == "" {
		if file := os.Getenv("LOOP_GITHUB_TOKEN_FILE"); file != "" {
			value, err := os.ReadFile(file)
			if err != nil {
				return nil, fmt.Errorf("read GitHub token: %w", err)
			}
			token = strings.TrimSpace(string(value))
		}
	}
	if token != "" {
		command.Env = append(os.Environ(), "GH_TOKEN="+token)
	}
	// Stdout is captured on its own rather than combined with stderr: callers
	// parse this as JSON, and gh writes upgrade notices and deprecation
	// warnings to stderr, which would land inside the document being decoded.
	// Leaving command.Stderr nil lets Output capture it into a bounded buffer,
	// which matters because the detail is truncated to 1 KiB before storage.
	output, err := command.Output()
	if err != nil {
		// A command exec.CommandContext kills for exceeding githubTimeout exits
		// non-zero (typically "signal: killed"), which os/exec reports as a
		// plain *exec.ExitError -- it does not itself wrap ctx.Err(), even
		// though the context is what caused the kill (verified against
		// go1.25's os/exec: Wait only surfaces the context error when the
		// process happens to exit success after being canceled, never for the
		// killed-with-nonzero-status case this timeout always produces).
		// Substituting ctx.Err() here is what lets isTransientGitHubError
		// (delivery.go) tell a timed-out gh invocation apart from an ordinary
		// command failure via errors.Is, instead of guessing from "signal:
		// killed" string text that says nothing about the cause.
		if ctxErr := ctx.Err(); ctxErr != nil {
			return nil, fmt.Errorf("gh %s: %w", strings.Join(args, " "), ctxErr)
		}
		var exit *exec.ExitError
		var detail string
		if errors.As(err, &exit) {
			detail = strings.TrimSpace(string(exit.Stderr))
		}
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, redactSecrets(detail))
	}
	return output, nil
}

var secretPattern = regexp.MustCompile(`gh[pousr]_[A-Za-z0-9]{16,}|github_pat_[A-Za-z0-9_]{16,}`)

// redactSecrets strips GitHub token literals out of text that is on its way to
// a user-visible field. gh failures are stored in issue_sync_state.last_error
// and workflow_runs.blocking_reason, both of which any authenticated session
// can read back, so an error that happens to echo the token would publish it to
// every viewer of the console.
func redactSecrets(value string) string {
	return secretPattern.ReplaceAllString(value, "[redacted]")
}

// githubCLI is the MVP's only shipped TaskSource/CodeHost implementation. It
// satisfies both interfaces (see tasksource.go) via the `gh` CLI: ListTasks is
// its TaskSource half, and FindOrCreatePR/Checks/Merge/Merged are its
// CodeHost half. ref, on every method, is the project's configured
// repository_url exactly as stored -- githubCLI (and only githubCLI) parses
// it into an owner/name slug via repositoryRef; the generic delivery code in
// delivery.go and the sync code in server.go never do that parsing
// themselves any more, which is what keeps a github.com URL shape from
// leaking into code that has to stay provider-neutral.
type githubCLI struct {
	command Command
	token   func(context.Context, string) (string, error)
}

// githubIssue is gh's own issue shape, decoded straight off `gh issue list
// --json`. It never crosses the TaskSource interface -- ListTasks reduces it
// to a neutral Task (see tasksource.go) before returning, deriving
// Priority/Eligible from Labels itself, so nothing outside this file ever
// sees a GitHub label list.
type githubIssue struct {
	ExternalID string
	Title      string
	Body       string
	URL        string
	Labels     []string
	CreatedAt  time.Time
	UpdatedAt  time.Time
	Priority   int
	Eligible   bool
	// State is GitHub's issue state, lowercased ("open" or "closed"). It
	// drives app.issues.state, which every scheduling query filters on
	// directly (see UpsertIssue) — an issue closed on the tracker must be
	// reconciled here or it stays schedulable forever.
	State string
}

// asTask reduces a decoded GitHub issue to the neutral shape ListTasks
// returns. Raw is the githubIssue itself, JSON-encoded -- the only place the
// original label list survives, since it is stored purely for audit
// (app.issues.raw_snapshot) and never read back by application code.
func (issue githubIssue) asTask() Task {
	raw, err := json.Marshal(issue)
	if err != nil {
		// json.Marshal on this struct (plain strings/slices/times) cannot
		// fail in practice; falling back to null keeps a decode error here
		// from ever blocking the task itself from syncing.
		raw = []byte("null")
	}
	return Task{
		ExternalID: issue.ExternalID,
		Title:      issue.Title,
		Body:       issue.Body,
		URL:        issue.URL,
		Priority:   issue.Priority,
		Eligible:   issue.Eligible,
		State:      issue.State,
		CreatedAt:  issue.CreatedAt,
		UpdatedAt:  issue.UpdatedAt,
		Raw:        raw,
	}
}

const (
	checksPending CheckState = ChecksPending
	checksGreen   CheckState = ChecksGreen
	checksFailed  CheckState = ChecksFailed
)

// checkRun is one entry of GitHub's statusCheckRollup. The array is
// heterogeneous: CheckRun entries report progress as status/conclusion, while
// legacy StatusContext entries (commit statuses, as posted by Jenkins, Vercel
// and friends) report it as a single state. Decoding only the first pair made
// every StatusContext look like an empty — and therefore passing — entry.
type checkRun struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

// result reduces one entry to an outcome. The two shapes are disjoint, so the
// populated field identifies which one this is, and anything unrecognised falls
// to pending — including a shape GitHub has not introduced yet.
func (check checkRun) result() CheckState {
	outcome := strings.ToUpper(strings.TrimSpace(check.State))
	if outcome == "" {
		if !strings.EqualFold(strings.TrimSpace(check.Status), "COMPLETED") {
			return checksPending
		}
		outcome = strings.ToUpper(strings.TrimSpace(check.Conclusion))
	}
	switch outcome {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return checksGreen
	case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE", "ERROR":
		return checksFailed
	}
	return checksPending
}

// NewGitHubCLI builds the single adapter instance that satisfies both
// TaskSource and CodeHost for a GitHub-tracked, GitHub-hosted project.
func NewGitHubCLI(command Command, token func(context.Context, string) (string, error)) githubCLI {
	if command == nil {
		command = execCommand{}
	}
	return githubCLI{command: command, token: token}
}

// resolveGitHubToken returns the project's stored github_token, or an empty
// string when the project has none configured so the caller falls back to the
// global token file. A project that stores a token it cannot decrypt is an
// error, not a silent fallback: using the wrong tenant's token would publish
// one project's work under another's identity.
func resolveGitHubToken(ctx context.Context, queries *db.Queries, projectID string) (string, error) {
	if projectID == "" {
		return "", nil
	}
	secret, err := queries.GetProjectCredentialSecret(ctx, db.GetProjectCredentialSecretParams{ProjectID: projectID, Kind: "github_token"})
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", databaseError(err)
	}
	aead, err := configuredCipher()
	if err != nil {
		return "", err
	}
	plaintext, err := aead.Open(nil, secret.Nonce, secret.Ciphertext, nil)
	if err != nil {
		return "", status.Error(codes.FailedPrecondition, "stored GitHub token could not be opened")
	}
	return string(plaintext), nil
}

// githubIssueListLimit bounds a single ListIssues call. gh issue list has no
// cursor/page flag of its own -- --limit is the only lever, and it paginates
// internally (in pages of up to 100) until either the limit or the tracker's
// own issue count is reached, whichever comes first. This is set far above
// the previous --limit 100 (which silently truncated any project with more
// open issues than that) so that, in practice, every project's full issue
// list is fetched in one call; a project that still has more issues than
// this is logged rather than silently truncated (see below), because gh
// itself gives no way to tell "reached the limit" apart from "that was
// exactly the whole list".
const githubIssueListLimit = 5000

// ListTasks implements TaskSource. ref is the project's configured
// repository_url exactly as stored; it is parsed into an owner/name slug here
// (via repositoryRef), not by the generic sync code that calls this.
func (client githubCLI) ListTasks(ctx context.Context, projectID, ref string) ([]Task, error) {
	repository, err := repositoryRef(ref)
	if err != nil {
		return nil, err
	}
	token, err := client.token(ctx, projectID)
	if err != nil {
		return nil, err
	}
	// --state all: --state open (the previous default) can by definition
	// never return a closed issue, so an issue closed on the tracker never
	// got reconciled here and stayed schedulable in the database forever.
	output, err := client.command.Run(ctx, token, "issue", "list", "--repo", repository, "--state", "all", "--limit", strconv.Itoa(githubIssueListLimit), "--json", "number,title,body,url,labels,createdAt,updatedAt,state")
	if err != nil {
		return nil, err
	}
	var values []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"url"`
		State  string `json:"state"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	if err := json.Unmarshal(output, &values); err != nil {
		return nil, fmt.Errorf("decode GitHub issues: %w", err)
	}
	if len(values) >= githubIssueListLimit {
		slog.Warn("GitHub issue list may be truncated", "project_id", projectID, "repository", repository, "limit", githubIssueListLimit)
	}
	tasks := make([]Task, 0, len(values))
	for _, value := range values {
		if value.Number < 1 || strings.TrimSpace(value.Title) == "" || value.URL == "" {
			return nil, errors.New("GitHub returned an invalid issue")
		}
		labels := make([]string, 0, len(value.Labels))
		for _, label := range value.Labels {
			labels = append(labels, label.Name)
		}
		priority, eligible := issuePriority(labels)
		state := strings.ToLower(strings.TrimSpace(value.State))
		// A closed issue is never eligible regardless of its labels: it is
		// not "ready for work" if there is no longer any open issue to work.
		// The scheduler also filters on state = 'open' directly (belt and
		// suspenders), but IssueSyncStatusEntries' eligible_count and any
		// other eligible-only reader must not overcount a closed issue too.
		eligible = eligible && state == "open"
		issue := githubIssue{ExternalID: strconv.Itoa(value.Number), Title: value.Title, Body: value.Body, URL: value.URL, Labels: labels, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Priority: priority, Eligible: eligible, State: state}
		tasks = append(tasks, issue.asTask())
	}
	return tasks, nil
}

// FindOrCreatePR implements CodeHost. ref is the project's configured
// repository_url exactly as stored; see ListTasks's doc comment.
func (client githubCLI) FindOrCreatePR(ctx context.Context, projectID, ref, branch, base, title, body string) (PullRequest, error) {
	repository, err := repositoryRef(ref)
	if err != nil {
		return PullRequest{}, err
	}
	token, err := client.token(ctx, projectID)
	if err != nil {
		return PullRequest{}, err
	}
	output, err := client.command.Run(ctx, token, "pr", "list", "--repo", repository, "--head", branch, "--state", "open", "--json", "number,url,state,headRefOid")
	if err != nil {
		return PullRequest{}, err
	}
	pr, found, err := decodePRs(output)
	if err != nil {
		return PullRequest{}, err
	}
	if found {
		return pr, nil
	}
	if _, err := client.command.Run(ctx, token, "pr", "create", "--repo", repository, "--head", branch, "--base", base, "--title", title, "--body", body); err != nil {
		return PullRequest{}, err
	}
	output, err = client.command.Run(ctx, token, "pr", "list", "--repo", repository, "--head", branch, "--state", "open", "--json", "number,url,state,headRefOid")
	if err != nil {
		return PullRequest{}, err
	}
	pr, found, err = decodePRs(output)
	if err != nil {
		return PullRequest{}, err
	}
	if !found {
		return PullRequest{}, errors.New("created pull request was not found")
	}
	return pr, nil
}

// Checks implements CodeHost. ref is the project's configured repository_url
// exactly as stored; see ListTasks's doc comment.
func (client githubCLI) Checks(ctx context.Context, projectID, ref, number string) (CheckState, error) {
	repository, err := repositoryRef(ref)
	if err != nil {
		return checksPending, err
	}
	token, err := client.token(ctx, projectID)
	if err != nil {
		return checksPending, err
	}
	output, err := client.command.Run(ctx, token, "pr", "view", number, "--repo", repository, "--json", "statusCheckRollup")
	if err != nil {
		return checksPending, err
	}
	var value struct {
		StatusCheckRollup []checkRun `json:"statusCheckRollup"`
	}
	if err := json.Unmarshal(output, &value); err != nil {
		return checksPending, fmt.Errorf("decode GitHub checks: %w", err)
	}
	return checksResult(value.StatusCheckRollup), nil
}

// Merge implements CodeHost's squash-merge. ref is the project's configured
// repository_url exactly as stored; see ListTasks's doc comment.
func (client githubCLI) Merge(ctx context.Context, projectID, ref, number string) error {
	repository, err := repositoryRef(ref)
	if err != nil {
		return err
	}
	token, err := client.token(ctx, projectID)
	if err != nil {
		return err
	}
	_, err = client.command.Run(ctx, token, "pr", "merge", number, "--repo", repository, "--squash", "--delete-branch")
	return err
}

// Merged implements CodeHost. ref is the project's configured repository_url
// exactly as stored; see ListTasks's doc comment.
func (client githubCLI) Merged(ctx context.Context, projectID, ref, number string) (bool, error) {
	repository, err := repositoryRef(ref)
	if err != nil {
		return false, err
	}
	token, err := client.token(ctx, projectID)
	if err != nil {
		return false, err
	}
	output, err := client.command.Run(ctx, token, "pr", "view", number, "--repo", repository, "--json", "state,mergedAt")
	if err != nil {
		return false, err
	}
	var value struct {
		State    string     `json:"state"`
		MergedAt *time.Time `json:"mergedAt"`
	}
	if err := json.Unmarshal(output, &value); err != nil {
		return false, fmt.Errorf("decode GitHub pull request: %w", err)
	}
	return value.State == "MERGED" && value.MergedAt != nil, nil
}

func repositoryRef(value string) (string, error) {
	value = strings.TrimSpace(value)
	value = strings.TrimPrefix(value, "https://github.com/")
	value = strings.TrimPrefix(value, "git@github.com:")
	value = strings.TrimSuffix(value, ".git")
	parts := strings.Split(value, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(value, " \t\r\n") {
		return "", errors.New("GitHub repository URL is invalid")
	}
	return value, nil
}

// issuePriority reads the agent labels off an issue. `agent:ready` opts the
// issue in, and the lifecycle labels opt it back out: `agent:blocked` is how an
// operator stops autonomous work on an issue from the tracker side, and
// `agent:delivered` marks one that has already been through the pipeline.
// Honouring only `agent:ready` made both of those labels decorative.
func issuePriority(labels []string) (int, bool) {
	priority, eligible, excluded := 0, false, false
	for _, label := range labels {
		switch label {
		case "agent:ready":
			eligible = true
		case "agent:blocked", "agent:delivered":
			excluded = true
		}
		if value, ok := strings.CutPrefix(label, "agent-priority:"); ok {
			if parsed, err := strconv.Atoi(value); err == nil {
				priority = parsed
			}
		}
	}
	return priority, eligible && !excluded
}

func decodePRs(contents []byte) (PullRequest, bool, error) {
	var values []struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		State      string `json:"state"`
		HeadRefOID string `json:"headRefOid"`
	}
	if err := json.Unmarshal(contents, &values); err != nil {
		return PullRequest{}, false, fmt.Errorf("decode GitHub pull requests: %w", err)
	}
	if len(values) == 0 {
		return PullRequest{}, false, nil
	}
	if values[0].Number < 1 || values[0].URL == "" || values[0].HeadRefOID == "" {
		return PullRequest{}, false, errors.New("GitHub returned an invalid pull request")
	}
	return PullRequest{Number: strconv.Itoa(values[0].Number), URL: values[0].URL, State: strings.ToLower(values[0].State), HeadSHA: values[0].HeadRefOID}, true, nil
}

// checksResult reduces a pull request's check rollup to a merge decision. An
// empty rollup is pending, never green: GitHub reports no checks for the first
// seconds of a pull request's life and none at all for a repository with no CI
// configured, and reading either as success squash-merges unverified code.
func checksResult(checks []checkRun) CheckState {
	if len(checks) == 0 {
		return checksPending
	}
	result := checksGreen
	for _, check := range checks {
		switch check.result() {
		case checksFailed:
			return checksFailed
		case checksPending:
			result = checksPending
		}
	}
	return result
}
