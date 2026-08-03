package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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

type GitHub interface {
	ListIssues(ctx context.Context, projectID, repository string) ([]githubIssue, error)
	FindOrCreatePR(ctx context.Context, projectID, repository, branch, base, title, body string) (githubPR, error)
	Checks(ctx context.Context, projectID, repository, number string) (checkState, error)
	MergeSquash(ctx context.Context, projectID, repository, number string) error
	Merged(ctx context.Context, projectID, repository, number string) (bool, error)
}

type githubCLI struct {
	command Command
	token   func(context.Context, string) (string, error)
}

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
}

type githubPR struct {
	Number  string
	URL     string
	State   string
	HeadSHA string
}

type checkState int

const (
	checksPending checkState = iota
	checksGreen
	checksFailed
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
func (check checkRun) result() checkState {
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

func NewGitHubCLI(command Command, token func(context.Context, string) (string, error)) GitHub {
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

func (client githubCLI) ListIssues(ctx context.Context, projectID, repository string) ([]githubIssue, error) {
	token, err := client.token(ctx, projectID)
	if err != nil {
		return nil, err
	}
	output, err := client.command.Run(ctx, token, "issue", "list", "--repo", repository, "--state", "open", "--limit", "100", "--json", "number,title,body,url,labels,createdAt,updatedAt")
	if err != nil {
		return nil, err
	}
	var values []struct {
		Number int    `json:"number"`
		Title  string `json:"title"`
		Body   string `json:"body"`
		URL    string `json:"url"`
		Labels []struct {
			Name string `json:"name"`
		} `json:"labels"`
		CreatedAt time.Time `json:"createdAt"`
		UpdatedAt time.Time `json:"updatedAt"`
	}
	if err := json.Unmarshal(output, &values); err != nil {
		return nil, fmt.Errorf("decode GitHub issues: %w", err)
	}
	issues := make([]githubIssue, 0, len(values))
	for _, value := range values {
		if value.Number < 1 || strings.TrimSpace(value.Title) == "" || value.URL == "" {
			return nil, errors.New("GitHub returned an invalid issue")
		}
		labels := make([]string, 0, len(value.Labels))
		for _, label := range value.Labels {
			labels = append(labels, label.Name)
		}
		priority, eligible := issuePriority(labels)
		issues = append(issues, githubIssue{ExternalID: strconv.Itoa(value.Number), Title: value.Title, Body: value.Body, URL: value.URL, Labels: labels, CreatedAt: value.CreatedAt, UpdatedAt: value.UpdatedAt, Priority: priority, Eligible: eligible})
	}
	return issues, nil
}

func (client githubCLI) FindOrCreatePR(ctx context.Context, projectID, repository, branch, base, title, body string) (githubPR, error) {
	token, err := client.token(ctx, projectID)
	if err != nil {
		return githubPR{}, err
	}
	output, err := client.command.Run(ctx, token, "pr", "list", "--repo", repository, "--head", branch, "--state", "open", "--json", "number,url,state,headRefOid")
	if err != nil {
		return githubPR{}, err
	}
	pr, found, err := decodePRs(output)
	if err != nil {
		return githubPR{}, err
	}
	if found {
		return pr, nil
	}
	if _, err := client.command.Run(ctx, token, "pr", "create", "--repo", repository, "--head", branch, "--base", base, "--title", title, "--body", body); err != nil {
		return githubPR{}, err
	}
	output, err = client.command.Run(ctx, token, "pr", "list", "--repo", repository, "--head", branch, "--state", "open", "--json", "number,url,state,headRefOid")
	if err != nil {
		return githubPR{}, err
	}
	pr, found, err = decodePRs(output)
	if err != nil {
		return githubPR{}, err
	}
	if !found {
		return githubPR{}, errors.New("created pull request was not found")
	}
	return pr, nil
}

func (client githubCLI) Checks(ctx context.Context, projectID, repository, number string) (checkState, error) {
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

func (client githubCLI) MergeSquash(ctx context.Context, projectID, repository, number string) error {
	token, err := client.token(ctx, projectID)
	if err != nil {
		return err
	}
	_, err = client.command.Run(ctx, token, "pr", "merge", number, "--repo", repository, "--squash", "--delete-branch")
	return err
}

func (client githubCLI) Merged(ctx context.Context, projectID, repository, number string) (bool, error) {
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

func decodePRs(contents []byte) (githubPR, bool, error) {
	var values []struct {
		Number     int    `json:"number"`
		URL        string `json:"url"`
		State      string `json:"state"`
		HeadRefOID string `json:"headRefOid"`
	}
	if err := json.Unmarshal(contents, &values); err != nil {
		return githubPR{}, false, fmt.Errorf("decode GitHub pull requests: %w", err)
	}
	if len(values) == 0 {
		return githubPR{}, false, nil
	}
	if values[0].Number < 1 || values[0].URL == "" || values[0].HeadRefOID == "" {
		return githubPR{}, false, errors.New("GitHub returned an invalid pull request")
	}
	return githubPR{Number: strconv.Itoa(values[0].Number), URL: values[0].URL, State: strings.ToLower(values[0].State), HeadSHA: values[0].HeadRefOID}, true, nil
}

// checksResult reduces a pull request's check rollup to a merge decision. An
// empty rollup is pending, never green: GitHub reports no checks for the first
// seconds of a pull request's life and none at all for a repository with no CI
// configured, and reading either as success squash-merges unverified code.
func checksResult(checks []checkRun) checkState {
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
