package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
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
	output, err := command.CombinedOutput()
	if err != nil {
		return nil, fmt.Errorf("gh %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(output)))
	}
	return output, nil
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
		StatusCheckRollup []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"statusCheckRollup"`
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

func issuePriority(labels []string) (int, bool) {
	priority, eligible := 0, false
	for _, label := range labels {
		if label == "agent:ready" {
			eligible = true
		}
		if value, ok := strings.CutPrefix(label, "agent-priority:"); ok {
			if parsed, err := strconv.Atoi(value); err == nil {
				priority = parsed
			}
		}
	}
	return priority, eligible
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

func checksResult(checks []struct {
	Status     string `json:"status"`
	Conclusion string `json:"conclusion"`
}) checkState {
	for _, check := range checks {
		conclusion := strings.ToUpper(check.Conclusion)
		status := strings.ToUpper(check.Status)
		if conclusion == "FAILURE" || conclusion == "CANCELLED" || conclusion == "TIMED_OUT" || conclusion == "ACTION_REQUIRED" || conclusion == "STARTUP_FAILURE" || conclusion == "STALE" {
			return checksFailed
		}
		if conclusion != "" && conclusion != "SUCCESS" && conclusion != "NEUTRAL" && conclusion != "SKIPPED" {
			return checksPending
		}
		if status != "" && status != "COMPLETED" {
			return checksPending
		}
	}
	return checksGreen
}
