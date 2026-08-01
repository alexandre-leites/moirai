package taskpacket

import "testing"

// A GitHub issue body is markdown. Requiring it to survive TrimSpace and to
// contain no unicode.IsControl rune rejected every body with a second line --
// nearly every real issue -- as "task packet issue or objective is invalid".
// No execution could start, and the runner logged only that it had received an
// offer: 765 consecutive rejections on a live deployment before anyone could
// see why. See tasks/lessons.md.
func TestAMultiLineIssueBodyIsAccepted(t *testing.T) {
	packet := validPacket(RolePlanner)
	packet.Issue.Body = "## Problem\n\nThe README opens with architecture.\n\n- [ ] rewrite it\n"
	if err := packet.Validate(); err != nil {
		t.Fatalf("a markdown issue body was rejected: %v", err)
	}
}

func TestProseFieldsAcceptTheLineStructureAHumanWrote(t *testing.T) {
	for name, mutate := range map[string]func(*Packet){
		"issue body":   func(p *Packet) { p.Issue.Body = "line one\nline two\n" },
		"diff summary": func(p *Packet) { p.DiffSummary = "a.go | 2 +-\nb.go | 9 ++++\n" },
		"plan step":    func(p *Packet) { p.Plan = []string{"step one\n  with detail"} },
		"finding":      func(p *Packet) { p.ReviewFindings = []string{"foo.go:12\n  unchecked error"} },
		"failure":      func(p *Packet) { p.PreviousFailures = []string{"exit 1\nstderr: boom"} },
		"tab indent":   func(p *Packet) { p.Issue.Body = "code:\n\tindented" },
	} {
		t.Run(name, func(t *testing.T) {
			packet := validPacket(RolePlanner)
			mutate(&packet)
			if err := packet.Validate(); err != nil {
				t.Fatalf("rejected: %v", err)
			}
		})
	}
}

// Newlines and tabs are content. The rest of the control range is not: NUL and
// escape are what terminal-escape injection uses, and nothing a human types
// into an issue produces them.
func TestProseStillRejectsControlCharactersThatAreNotLineStructure(t *testing.T) {
	for name, body := range map[string]string{
		"NUL":          "before\x00after",
		"escape":       "before\x1b[31mred",
		"bell":         "before\x07after",
		"vertical tab": "before\x0bafter",
		"C1 NEL":       "before\u0085after",
	} {
		t.Run(name, func(t *testing.T) {
			packet := validPacket(RolePlanner)
			packet.Issue.Body = body
			if err := packet.Validate(); err == nil {
				t.Fatal("accepted a control character that is not line structure")
			}
		})
	}
}

// A title, a branch and a URL are single-line by nature. Loosening those is how
// a newline reaches somewhere it acts as a separator.
func TestSingleLineFieldsStillRejectNewlines(t *testing.T) {
	for name, mutate := range map[string]func(*Packet){
		"title":  func(p *Packet) { p.Issue.Title = "one\ntwo" },
		"branch": func(p *Packet) { p.Repository.Branch = "agent/1\nrm -rf" },
		"url":    func(p *Packet) { p.Repository.URL = "https://example.test/x.git\nmore" },
	} {
		t.Run(name, func(t *testing.T) {
			packet := validPacket(RolePlanner)
			mutate(&packet)
			if err := packet.Validate(); err == nil {
				t.Fatalf("a newline was accepted in %s", name)
			}
		})
	}
}
