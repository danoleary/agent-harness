package prompt

import (
	"regexp"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// fetchInstruction matches a sentence that tells the session to fetch: `git
// fetch` or "fetch origin/main", with no "not" or "never" earlier in the same
// sentence. "do not run `git fetch`" is the steer; "`git fetch`, then see" and
// "Fetch `origin/main` only to read it" are the #364 instructions.
var fetchInstruction = regexp.MustCompile("(?i)(^|[.!?:]\\s)([^.!?:]*?)(`git fetch`|git fetch|fetch `?origin/main)")

// fetchInstructions returns each sentence in p that tells the session to fetch.
func fetchInstructions(p string) []string {
	var found []string
	for _, m := range fetchInstruction.FindAllStringSubmatch(p, -1) {
		if !regexp.MustCompile(`(?i)\b(not|never)\b`).MatchString(m[2]) {
			found = append(found, strings.TrimSpace(m[0]))
		}
	}
	return found
}

// #364: the sandbox has no network and no credentials (ADR-0002), so a session
// told to fetch origin/main lost a turn to `fatal: could not read Username for
// 'https://github.com'` and could not tell whether its origin/main was fresh.
// The host fetches it before the session starts; both sections that told the
// session to fetch now say so and forbid the fetch.
func TestBranchAndADRSectionsSayOriginMainIsFetchedAndForbidTheFetch(t *testing.T) {
	for name, s := range map[string]string{
		"branchHandoffContract": branchHandoffContract("feat", "gh-364"),
		"adrNumberSection":      adrNumberSection([]string{"docs/adr/0039-<slug>.md"}),
	} {
		if got := fetchInstructions(s); len(got) > 0 {
			t.Errorf("%s tells the session to fetch: %q", name, got)
		}
		for _, want := range []string{"already fetched `origin/main`", "no network and no credentials", "do not run `git fetch`"} {
			if !strings.Contains(s, want) {
				t.Errorf("%s missing %q:\n%s", name, want, s)
			}
		}
	}
}

// No prompt the harness builds, for any Stage or Resume variant, tells a
// sandboxed session to fetch (#364).
func TestNoPromptTellsTheSessionToFetch(t *testing.T) {
	adrTicket := ticket.Ticket{
		Identifier:  "#197",
		Title:       "Decide how open security alerts get fixed",
		Description: "Write the decision up as docs/adr/0039-<slug>.md.",
	}
	bodies := map[Stage]string{Implement: "implement", Review: "review", Retrospective: "retro"}
	for _, st := range []Stage{Implement, Review, Retrospective, CIFix, RebaseFix, SecurityFix, Triage} {
		for _, r := range []Resume{Fresh, ResumedBranch, AfterRefusal} {
			c := Context{Ticket: adrTicket, Slug: "gh-197", BranchPrefix: testPrefix, WorktreePath: sampleWorktree, Resume: r, CILogs: sampleCILogs}
			if b, ok := bodies[st]; ok {
				c.Body = exampleBody(t, b)
			}
			if got := fetchInstructions(For(st, c)); len(got) > 0 {
				t.Errorf("stage %d resume %d tells the session to fetch: %q", st, r, got)
			}
		}
	}
}
