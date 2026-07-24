package repository

import (
	"strings"
	"testing"
)

func TestBranchNameIsDeterministicSafeAndIssueScoped(t *testing.T) {
	branch, err := BranchName("Issue #123: Improve API/UX", "workflow-0001")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(branch, "agent/issue-123-improve-api-ux/") || !safeRef(branch) {
		t.Fatalf("unexpected branch %q", branch)
	}
	again, err := BranchName("Issue #123: Improve API/UX", "workflow-0001")
	if err != nil || again != branch {
		t.Fatalf("branch was not deterministic: %q, %v", again, err)
	}
	other, err := BranchName("Issue #123: Improve API/UX", "workflow-0002")
	if err != nil || other == branch {
		t.Fatalf("workflow identity did not change branch: %q, %v", other, err)
	}
}

func TestBranchNameRejectsEmptyOrUnusableIssueIdentifiers(t *testing.T) {
	for _, issue := range []string{"", " \t ", "---"} {
		if _, err := BranchName(issue, "workflow"); err == nil {
			t.Fatalf("expected error for %q", issue)
		}
	}
	if _, err := BranchName("123", ""); err == nil {
		t.Fatal("expected empty workflow error")
	}
}
