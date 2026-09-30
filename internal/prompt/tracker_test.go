package prompt

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// everyStage renders every Stage prompt — each Implement variant included — for
// one tracker kind, with sub-issues and already-filed findings present so every
// envelope section that could name the tracker is in the output.
func everyStage(t *testing.T, kind string) map[string]string {
	t.Helper()
	tk := ticket.Ticket{
		Identifier:  "#30",
		Title:       "Umbrella",
		Description: "Do the children.",
		SubIssues:   []ticket.SubIssue{{Identifier: "#31", Title: "Child", Description: "child spec"}},
	}
	base := Context{Ticket: tk, Slug: "gh-30", BranchPrefix: testPrefix, WorktreePath: "/wt/gh-30", Tracker: kind}
	with := func(body string, edit func(*Context)) Context {
		c := base
		c.Body = exampleBody(t, body)
		if edit != nil {
			edit(&c)
		}
		return c
	}
	return map[string]string{
		"implement":         For(Implement, with("implement", nil)),
		"implement/resumed": For(Implement, with("implement", func(c *Context) { c.Resume = ResumedBranch })),
		"implement/retry":   For(Implement, with("implement", func(c *Context) { c.Resume = AfterRefusal })),
		"review":            For(Review, with("review", nil)),
		"retrospective": For(Retrospective, with("retro", func(c *Context) {
			c.Filed = []FiledFinding{{Key: "sandbox-x", Title: "X"}}
		})),
		"cifix":     For(CIFix, Context{Ticket: tk, Slug: "gh-30", BranchPrefix: testPrefix, WorktreePath: "/wt/gh-30", Tracker: kind, CILogs: "boom", CILogAvailable: true}),
		"rebasefix": For(RebaseFix, Context{Ticket: tk, Slug: "gh-30", BranchPrefix: testPrefix, WorktreePath: "/wt/gh-30", Tracker: kind}),
	}
}

// The envelope names the tracker the Consumer configured (ADR-0010), never
// Linear by default: a GitHub- or Jira-tracked run must not be told about a
// tracker it does not use, and must be steered off the one it does (#35).
func TestEnvelopeNamesOnlyTheConfiguredTracker(t *testing.T) {
	cases := []struct {
		kind, name string
		linear     bool
	}{
		{"github", "GitHub Issues", false},
		{"jira", "Jira", false},
		{"linear", "Linear", true},
	}
	for _, tc := range cases {
		for stage, p := range everyStage(t, tc.kind) {
			if got := strings.Contains(p, "Linear"); got != tc.linear {
				t.Errorf("%s/%s: mentions Linear = %v, want %v", tc.kind, stage, got, tc.linear)
			}
			if got := strings.Contains(p, "mcp__linear-server__"); got != tc.linear {
				t.Errorf("%s/%s: names the Linear MCP = %v, want %v", tc.kind, stage, got, tc.linear)
			}
			if want := "Do NOT touch the issue tracker (" + tc.name + ")"; !strings.Contains(p, want) {
				t.Errorf("%s/%s: missing tracker-off steer %q", tc.kind, stage, want)
			}
		}
	}
}

// A GitHub-tracked sandbox holds GH_TOKEN, so the steer names the one tool that
// would reach its tracker from inside it.
func TestGitHubEnvelopeForbidsGhIssue(t *testing.T) {
	for stage, p := range everyStage(t, "github") {
		if !strings.Contains(p, "`gh issue`") {
			t.Errorf("%s: github envelope does not forbid `gh issue`", stage)
		}
	}
}
