package server

import "fmt"

// providerAdapters is Core's private registry of TaskSource/CodeHost
// implementations, selected per project by resolveTaskSource/resolveCodeHost
// from the project's own issue_tracker_type/code_host_type configuration
// (app.projects, read by GetProject, ListSyncableProjects,
// ListSyncableProjectByID, ListProjectsDueForSync and GetDeliveryWorkflow).
// Every project created before this seam existed has both columns at their
// schema default, "github", so an unrecognised/empty value always falls back
// to the GitHub adapter rather than erroring -- the one behavior-preservation
// requirement this whole refactor exists to satisfy.
type providerAdapters struct {
	defaultTaskSource   TaskSource
	defaultCodeHost     CodeHost
	localFileTaskSource TaskSource
}

// resolveTaskSource picks the TaskSource a project's issue_tracker_type
// configures. "local_file" is the seam's proof-of-concept adapter
// (localfile.go); every other value, including "" and "github", uses the
// GitHub-backed default so existing projects behave exactly as before this
// column was ever read.
func (adapters providerAdapters) resolveTaskSource(issueTrackerType string) (TaskSource, error) {
	switch issueTrackerType {
	case "", "github":
		return adapters.defaultTaskSource, nil
	case "local_file":
		if adapters.localFileTaskSource == nil {
			return nil, fmt.Errorf("issue_tracker_type %q is not configured on this orchestrator", issueTrackerType)
		}
		return adapters.localFileTaskSource, nil
	default:
		return nil, fmt.Errorf("unsupported issue_tracker_type %q", issueTrackerType)
	}
}

// resolveCodeHost picks the CodeHost a project's code_host_type configures.
// GitHub is the only shipped CodeHost; an unrecognised value other than the
// default still errors clearly rather than silently delivering through the
// wrong host.
func (adapters providerAdapters) resolveCodeHost(codeHostType string) (CodeHost, error) {
	switch codeHostType {
	case "", "github":
		return adapters.defaultCodeHost, nil
	default:
		return nil, fmt.Errorf("unsupported code_host_type %q", codeHostType)
	}
}
