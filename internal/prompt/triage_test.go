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

// #409: a read-only session loads the Consumer's Stop hooks, and a handoff hook
// that reads no HARNESS_SESSION_READ_ONLY tells it to run the formatter and
// re-commit, even over a check that crashed rather than found drift. The
// envelope says such an instruction does not reach a session that must not
// write, and names what it is told instead: a hook's report, not a task.
func TestReadOnlyPromptsOverruleAHookThatAsksForAWrite(t *testing.T) {
	for name, p := range map[string]string{
		"retrospective": For(Retrospective, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
		"triage":        For(Triage, Context{Ticket: ticket.Ticket{Identifier: "#12", Title: "Add a flag"}, Tracker: "github"}),
	} {
		for _, want := range []string{"Stop hook", "does not apply to this session", "HARNESS_SESSION_READ_ONLY"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt is missing %q:\n%s", name, want, p)
			}
		}
	}
	if p := For(Implement, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}); strings.Contains(p, "does not apply to this session") {
		t.Error("the implementation session writes the handoff; its hooks' instructions are its own")
	}
}
