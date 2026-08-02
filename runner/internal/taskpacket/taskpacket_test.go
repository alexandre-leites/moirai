package taskpacket

import (
	"encoding/json"
	"testing"
)

func TestParseAcceptsValidRolePackets(t *testing.T) {
	for _, role := range []Role{RolePlanner, RoleDeveloper, RolePipeline, RoleReviewer, RoleRepairer} {
		t.Run(string(role), func(t *testing.T) {
			packet := validPacket(role)
			if role == RoleDeveloper || role == RoleRepairer {
				packet.Constraints.MayModifyFiles = true
			}
			contents, err := json.Marshal(packet)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			parsed, err := Parse(contents)
			if err != nil {
				t.Fatalf("Parse() error = %v", err)
			}
			if parsed.Role != role || parsed.Repository.Mode != "managed_clone" {
				t.Fatalf("Parse() = %#v", parsed)
			}
		})
	}
}

func TestPacketRejectsInvalidExecutionImage(t *testing.T) {
	packet := validPacket(RoleDeveloper)
	packet.Constraints.MayModifyFiles = true
	packet.ExecutionImage = "image with spaces"
	if err := packet.Validate(); err == nil {
		t.Fatal("Validate() accepted invalid execution image")
	}
}

func TestParsePreservesWorkflowContext(t *testing.T) {
	packet := validPacket(RoleReviewer)
	packet.AcceptanceCriteria = []string{"Returns an actionable review"}
	packet.Plan = []string{"Inspect the change"}
	packet.PreviousFailures = []string{"lint failed previously"}
	packet.CurrentCommit = "abc123"
	packet.DiffSummary = "one file changed"
	packet.FailedChecks = []string{"lint"}
	packet.ReviewFindings = []string{"none"}
	contents, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	parsed, err := Parse(contents)
	if err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
	if parsed.CurrentCommit != "abc123" || len(parsed.AcceptanceCriteria) != 1 || len(parsed.Plan) != 1 {
		t.Fatalf("Parse() = %#v", parsed)
	}
}

func TestParseSupportsExistingPathRepository(t *testing.T) {
	packet := validPacket(RoleDeveloper)
	packet.Constraints.MayModifyFiles = true
	packet.Repository = Repository{
		ProjectID:     "project-1",
		Mode:          "existing_path",
		LocalPath:     "/repositories/service",
		DefaultBranch: "main",
		Branch:        "agent/issue-7/run-1",
	}
	contents, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, err := Parse(contents); err != nil {
		t.Fatalf("Parse() error = %v", err)
	}
}

func TestParseRejectsUnsafeOrUnauthorizedPackets(t *testing.T) {
	tests := []struct {
		name   string
		modify func(*Packet)
	}{
		{name: "unknown protocol", modify: func(packet *Packet) { packet.ProtocolVersion = "2.0" }},
		{name: "path traversal", modify: func(packet *Packet) { packet.PromptPath = ".loop/../outside.md" }},
		{name: "absolute prompt", modify: func(packet *Packet) { packet.PromptPath = "/tmp/prompt.md" }},
		{name: "merge permission", modify: func(packet *Packet) { packet.Constraints.MayMerge = true }},
		{name: "planner write permission", modify: func(packet *Packet) { packet.Constraints.MayModifyFiles = true }},
		{name: "relative local repository", modify: func(packet *Packet) {
			packet.Repository = Repository{ProjectID: "project-1", Mode: "existing_path", LocalPath: "repositories/service", DefaultBranch: "main", Branch: "agent/issue-7/run-1"}
		}},
		{name: "option repository URL", modify: func(packet *Packet) { packet.Repository.URL = "--upload-pack=evil" }},
		{name: "invalid pipeline command", modify: func(packet *Packet) { packet.Pipeline = []PipelineCommand{{Command: "", TimeoutSeconds: 1}} }},
		{name: "duplicate environment", modify: func(packet *Packet) {
			packet.EnvironmentRefs = append(packet.EnvironmentRefs, packet.EnvironmentRefs[0])
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			packet := validPacket(RolePlanner)
			test.modify(&packet)
			contents, err := json.Marshal(packet)
			if err != nil {
				t.Fatalf("Marshal() error = %v", err)
			}
			if _, err := Parse(contents); err == nil {
				t.Fatal("Parse() accepted invalid packet")
			}
		})
	}
}

func TestParseRejectsUnknownAndMultipleJSONValues(t *testing.T) {
	packet := validPacket(RolePlanner)
	contents, err := json.Marshal(packet)
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	contents = append(contents[:len(contents)-1], []byte(`,"unexpected":true}`)...)
	if _, err := Parse(contents); err == nil {
		t.Fatal("Parse() accepted an unknown field")
	}
	contents, err = json.Marshal(validPacket(RolePlanner))
	if err != nil {
		t.Fatalf("Marshal() error = %v", err)
	}
	if _, err := Parse(append(contents, contents...)); err == nil {
		t.Fatal("Parse() accepted multiple JSON values")
	}
}

func validPacket(role Role) Packet {
	return Packet{
		ProtocolVersion: ProtocolVersion,
		JobID:           "job-1",
		ExecutionID:     "execution-1",
		Role:            role,
		Objective:       "Implement issue seven",
		Issue:           Issue{ExternalID: "7", Title: "Improve task packet validation", Body: "Validate runner task packets."},
		Repository: Repository{
			ProjectID:     "project-1",
			Mode:          "managed_clone",
			URL:           "git@github.com:example/service.git",
			DefaultBranch: "main",
			Branch:        "agent/issue-7/run-1",
		},
		PromptPath:      ".loop/prompt.md",
		ExpectedOutput:  ".loop/result.json",
		TimeoutSeconds:  600,
		EnvironmentRefs: []EnvironmentRef{{Name: "GITHUB_TOKEN", SecretRef: "project/github-token"}},
		Constraints:     Constraints{},
	}
}
