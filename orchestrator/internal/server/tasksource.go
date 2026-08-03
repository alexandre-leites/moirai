package server

import (
	"context"
	"encoding/json"
	"time"
)

// Task is the neutral, provider-agnostic shape a TaskSource hands back to the
// scheduler. It carries only the facts the orchestrator's core actually
// decides on: nothing here is GitHub-shaped. In particular there is no raw
// label list -- Priority and Eligible are the two answers every adapter must
// derive for itself (a GitHub adapter reads them off `agent:*` labels, a
// future Jira adapter would read a JQL filter and a priority field, a Linear
// adapter a workflow state and an estimate), and only those derived answers
// cross the interface. Raw carries the adapter's own untouched snapshot
// purely for audit/debugging storage (app.issues.raw_snapshot); nothing in
// the core ever parses it back apart.
type Task struct {
	ExternalID string
	Title      string
	Body       string
	URL        string
	Priority   int
	Eligible   bool
	// State is the task's lifecycle state, normalized to "open" or "closed".
	// A closed task is never eligible, regardless of what the adapter itself
	// concluded from provider-specific fields.
	State     string
	CreatedAt time.Time
	UpdatedAt time.Time
	// Raw is the adapter's untouched snapshot of the task, stored verbatim in
	// app.issues.raw_snapshot for audit purposes. Opaque to every reader
	// outside the adapter that produced it.
	Raw json.RawMessage
}

// TaskSource is where work comes from -- an issue tracker, in the MVP's only
// shipped adapter, but the interface itself commits to nothing GitHub-shaped.
// A project can configure any number of sources (app.project_task_sources,
// see #293); each row's own provider selects which TaskSource constructs its
// tasks (see resolveTaskSource), and every implementation is responsible for
// deciding eligibility and priority itself before a Task ever reaches the
// scheduler.
type TaskSource interface {
	// ListTasks returns every task currently known for one configured source.
	// taskSourceID identifies which app.project_task_sources row this call is
	// for -- distinct from projectID because a project may have several
	// sources, and is what lets an adapter scope anything source-specific
	// (a per-source credential, see resolveGitHubToken) rather than only
	// ever project-specific. ref locates that source's tasks in whatever
	// shape this source needs: the GitHub adapter reads it as an
	// "owner/name" repository slug (see repositoryRef), the local-file
	// adapter (localfile.go) reads it as a directory path. Both come from
	// the source's own configuration->>'ref' (see syncSource); a
	// source-specific interpretation of the same configured string is
	// exactly what lets one source be pointed at a different kind of
	// tracker without a schema change.
	ListTasks(ctx context.Context, projectID, taskSourceID, ref string) ([]Task, error)
}

// PullRequest is the neutral result of a CodeHost creating or finding a
// pull/merge request for a workflow's branch.
type PullRequest struct {
	Number  string
	URL     string
	State   string
	HeadSHA string
}

// CheckState is the neutral outcome of a CodeHost's CI/status checks for a
// pull request.
type CheckState int

const (
	ChecksPending CheckState = iota
	ChecksGreen
	ChecksFailed
)

// CodeHost is where code is delivered -- opening a pull request, watching its
// checks, and merging it once they pass. Addressed by ref, a provider-neutral
// pointer to the repository (see TaskSource.ListTasks's doc comment): the
// GitHub implementation is the only thing that ever parses it into an
// owner/name slug, never the generic delivery code in delivery.go.
type CodeHost interface {
	FindOrCreatePR(ctx context.Context, projectID, ref, branch, base, title, body string) (PullRequest, error)
	Checks(ctx context.Context, projectID, ref, number string) (CheckState, error)
	Merge(ctx context.Context, projectID, ref, number string) error
	Merged(ctx context.Context, projectID, ref, number string) (bool, error)
}
