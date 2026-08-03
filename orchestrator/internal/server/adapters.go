package server

import "fmt"

// providerAdapters is Core's private registry of TaskSource/CodeHost
// implementations. resolveTaskSource selects one per task source, from that
// source's own provider column (app.project_task_sources, see migration 026
// and #293); resolveCodeHost selects one per project, from the project's own
// code_host_type (app.projects, read by GetProject and GetDeliveryWorkflow)
// -- a project has 1..N task sources but 0..1 code hosts (see tasksource.go),
// so only the former is plural. Every task source migration 026 auto-created
// for a project that predates this seam has provider "github" (copied from
// that project's old issue_tracker_type, itself defaulted to "github"), so an
// unrecognised/empty value always falls back to the GitHub adapter rather
// than erroring -- the one behavior-preservation requirement this whole
// refactor exists to satisfy.
type providerAdapters struct {
	defaultTaskSource   TaskSource
	defaultCodeHost     CodeHost
	localFileTaskSource TaskSource
}

// resolveTaskSource picks the TaskSource a task source's provider column
// configures. "local_file" is the seam's proof-of-concept adapter
// (localfile.go); every other value, including "" and "github", uses the
// GitHub-backed default so a source migrated from an existing project's
// issue_tracker_type behaves exactly as that project did before #293.
func (adapters providerAdapters) resolveTaskSource(provider string) (TaskSource, error) {
	switch provider {
	case "", "github":
		return adapters.defaultTaskSource, nil
	case "local_file":
		if adapters.localFileTaskSource == nil {
			return nil, fmt.Errorf("task source provider %q is not configured on this orchestrator", provider)
		}
		return adapters.localFileTaskSource, nil
	default:
		return nil, fmt.Errorf("unsupported task source provider %q", provider)
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
