package server

import "testing"

func TestResolveTaskSourceDefaultsToGitHub(t *testing.T) {
	github := NewGitHubCLI(nil, noToken)
	adapters := providerAdapters{defaultTaskSource: github, defaultCodeHost: github, localFileTaskSource: LocalFileTaskSource{}}

	for _, issueTrackerType := range []string{"", "github"} {
		source, err := adapters.resolveTaskSource(issueTrackerType)
		if err != nil {
			t.Fatalf("issue_tracker_type=%q: %v", issueTrackerType, err)
		}
		if _, ok := source.(githubCLI); !ok {
			t.Fatalf("issue_tracker_type=%q resolved to %T, want githubCLI", issueTrackerType, source)
		}
	}
}

func TestResolveTaskSourceLocalFile(t *testing.T) {
	github := NewGitHubCLI(nil, noToken)
	local := LocalFileTaskSource{}
	adapters := providerAdapters{defaultTaskSource: github, defaultCodeHost: github, localFileTaskSource: local}

	source, err := adapters.resolveTaskSource("local_file")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := source.(LocalFileTaskSource); !ok {
		t.Fatalf("resolveTaskSource(\"local_file\") = %T, want LocalFileTaskSource", source)
	}
}

func TestResolveTaskSourceLocalFileUnconfiguredErrors(t *testing.T) {
	github := NewGitHubCLI(nil, noToken)
	adapters := providerAdapters{defaultTaskSource: github, defaultCodeHost: github}
	if _, err := adapters.resolveTaskSource("local_file"); err == nil {
		t.Fatal("expected an error when no local-file task source is configured")
	}
}

func TestResolveTaskSourceUnsupportedErrors(t *testing.T) {
	github := NewGitHubCLI(nil, noToken)
	adapters := providerAdapters{defaultTaskSource: github, defaultCodeHost: github}
	if _, err := adapters.resolveTaskSource("jira"); err == nil {
		t.Fatal("expected an error for an unsupported issue_tracker_type")
	}
}

func TestResolveCodeHostDefaultsToGitHub(t *testing.T) {
	github := NewGitHubCLI(nil, noToken)
	adapters := providerAdapters{defaultTaskSource: github, defaultCodeHost: github}
	for _, codeHostType := range []string{"", "github"} {
		host, err := adapters.resolveCodeHost(codeHostType)
		if err != nil {
			t.Fatalf("code_host_type=%q: %v", codeHostType, err)
		}
		if _, ok := host.(githubCLI); !ok {
			t.Fatalf("code_host_type=%q resolved to %T, want githubCLI", codeHostType, host)
		}
	}
}

func TestResolveCodeHostUnsupportedErrors(t *testing.T) {
	github := NewGitHubCLI(nil, noToken)
	adapters := providerAdapters{defaultTaskSource: github, defaultCodeHost: github}
	if _, err := adapters.resolveCodeHost("gitlab"); err == nil {
		t.Fatal("expected an error for an unsupported code_host_type")
	}
}
