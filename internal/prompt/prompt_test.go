package prompt

import (
	"regexp"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/ticket"
)

var sample = ticket.Ticket{
	Identifier:  "BEH-362",
	Title:       "Agent harness Phase 1: run-tdd CLI",
	Description: "## What to build\n\nA minimal CLI.\n\n## Acceptance criteria\n\n- [ ] fetch the ticket",
	Priority:    "Urgent",
}

func TestBuildTddInvokesSkillOnTicketAndSlug(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	for _, want := range []string{"/tdd", "BEH-362", "slug `beh-362`"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildTddInjectsTitleAndDescription(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	for _, want := range []string{sample.Title, "Acceptance criteria", "fetch the ticket"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildTddSteersOffLinear(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	if !regexp.MustCompile(`(?i)already.*(claimed|In Progress)`).MatchString(p) {
		t.Error("prompt does not say the ticket is already claimed/In Progress")
	}
	if !regexp.MustCompile(`(?i)(do not|don't).*Linear`).MatchString(p) {
		t.Error("prompt does not steer off Linear")
	}
}

func TestBuildTddRedirectsFindingsToDropbox(t *testing.T) {
	p := BuildTdd(sample, "beh-362")

	if !strings.Contains(p, "/findings/out.json") {
		t.Error("prompt missing the findings dropbox path")
	}
	if !regexp.MustCompile(`title.*body.*kind`).MatchString(p) {
		t.Error("prompt missing the {title, body, kind} finding shape")
	}
}
