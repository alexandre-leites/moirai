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
)

type Command interface {
	Run(context.Context, ...string) ([]byte, error)
}

type execCommand struct{}

func (execCommand) Run(ctx context.Context, args ...string) ([]byte, error) {
	command := exec.CommandContext(ctx, "gh", args...)
	if file := os.Getenv("LOOP_GITHUB_TOKEN_FILE"); file != "" {
		token, err := os.ReadFile(file)
		if err != nil {
			return nil, fmt.Errorf("read GitHub token: %w", err)
		}
		command.Env = append(os.Environ(), "GH_TOKEN="+strings.TrimSpace(string(token)))
	}
	// Stdout is captured on its own rather than combined with stderr: callers
	// parse this as JSON, and gh writes upgrade notices and deprecation
	// warnings to stderr, which would land inside the document being decoded.
	var stderr strings.Builder
	command.Stderr = &stderr
	output, err := command.Output()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, redactSecrets(strings.TrimSpace(stderr.String())))
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
	ListIssues(context.Context, string) ([]githubIssue, error)
	FindOrCreatePR(context.Context, string, string, string, string, string) (githubPR, error)
	Checks(context.Context, string, string) (checkState, error)
	MergeSquash(context.Context, string, string) error
	Merged(context.Context, string, string) (bool, error)
}

type githubCLI struct{ command Command }

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
	TypeName   string `json:"__typename"`
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
	State      string `json:"state"`
}

func NewGitHubCLI(command Command) GitHub {
	if command == nil {
		command = execCommand{}
	}
	return githubCLI{command: command}
}

func (client githubCLI) ListIssues(ctx context.Context, repository string) ([]githubIssue, error) {
	output, err := client.command.Run(ctx, "issue", "list", "--repo", repository, "--state", "open", "--limit", "100", "--json", "number,title,body,url,labels,createdAt,updatedAt")
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

func (client githubCLI) FindOrCreatePR(ctx context.Context, repository, branch, base, title, body string) (githubPR, error) {
	output, err := client.command.Run(ctx, "pr", "list", "--repo", repository, "--head", branch, "--state", "open", "--json", "number,url,state,headRefOid")
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
	if _, err := client.command.Run(ctx, "pr", "create", "--repo", repository, "--head", branch, "--base", base, "--title", title, "--body", body); err != nil {
		return githubPR{}, err
	}
	output, err = client.command.Run(ctx, "pr", "list", "--repo", repository, "--head", branch, "--state", "open", "--json", "number,url,state,headRefOid")
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

func (client githubCLI) Checks(ctx context.Context, repository, number string) (checkState, error) {
	output, err := client.command.Run(ctx, "pr", "view", number, "--repo", repository, "--json", "statusCheckRollup")
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

func (client githubCLI) MergeSquash(ctx context.Context, repository, number string) error {
	_, err := client.command.Run(ctx, "pr", "merge", number, "--repo", repository, "--squash", "--delete-branch")
	return err
}

func (client githubCLI) Merged(ctx context.Context, repository, number string) (bool, error) {
	output, err := client.command.Run(ctx, "pr", "view", number, "--repo", repository, "--json", "state,mergedAt")
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

// checksResult reduces a pull request's check rollup to a merge decision.
//
// An empty rollup is pending, never green. GitHub reports no checks for the
// first seconds of a pull request's life, before it has queued the workflows a
// push triggers, and it reports none at all for a repository whose CI has not
// been configured yet. Treating that as success let the observer squash-merge
// agent-written code before any CI had run — the one outcome the green-checks
// gate exists to prevent. A caller that genuinely has no checks to wait for
// stays pending, which is a stall an operator can see and resolve, rather than
// an unverified merge nobody finds out about.
func checksResult(checks []checkRun) checkState {
	if len(checks) == 0 {
		return checksPending
	}
	for _, check := range checks {
		conclusion := strings.ToUpper(check.Conclusion)
		status := strings.ToUpper(check.Status)
		state := strings.ToUpper(check.State)
		// A StatusContext carries state alone; a CheckRun carries the other two.
		// An entry that reports neither is a shape this code does not
		// understand, and guessing "passing" for it is the dangerous guess.
		if conclusion == "" && status == "" && state == "" {
			return checksPending
		}
		if failedCheck(conclusion) || failedCheck(state) {
			return checksFailed
		}
		if conclusion != "" && !passedCheck(conclusion) {
			return checksPending
		}
		if state != "" && !passedCheck(state) {
			return checksPending
		}
		if status != "" && status != "COMPLETED" {
			return checksPending
		}
	}
	return checksGreen
}

func failedCheck(value string) bool {
	switch value {
	case "FAILURE", "CANCELLED", "TIMED_OUT", "ACTION_REQUIRED", "STARTUP_FAILURE", "STALE", "ERROR":
		return true
	}
	return false
}

func passedCheck(value string) bool {
	switch value {
	case "SUCCESS", "NEUTRAL", "SKIPPED":
		return true
	}
	return false
}
