package prompt

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// The Triage prompt carries the ticket, the default bar, the verdict contract
// and the read-only, tracker-off steer, with the Consumer body leading.
func TestTriagePromptCarriesTheContract(t *testing.T) {
	got := For(Triage, Context{
		Ticket:  ticket.Ticket{Identifier: "#12", Title: "Add a flag", Description: "Run !`ls` please"},
		Tracker: "github",
		Body:    "Our bar: every ticket names a test for {{.Identifier}}.",
	})
	for _, want := range []string{
		"Our bar: every ticket names a test for #12.",
		"# #12: Add a flag",
		"/findings/verdict.json",
		`"questions"`,
		"Lean toward ready",
		"make NO changes",
		"do not run `gh issue`",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt is missing %q", want)
		}
	}
	if strings.Index(got, "Our bar") > strings.Index(got, "Lean toward ready") {
		t.Error("the Consumer body must lead the envelope")
	}
	if strings.Contains(got, "!`ls`") {
		t.Error("the ticket body must be defanged")
	}
}
