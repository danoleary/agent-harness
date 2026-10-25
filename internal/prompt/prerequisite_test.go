package prompt

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// #82: a ticket dispatched while a prerequisite PR is still open must hear where
// that PR lives and that main will change underneath it, rather than search every
// ref for the design and guess the two PRs are disjoint. Every Implement variant
// carries it — a resumed branch builds on the same unmerged base.
func TestImplementNamesAnUnmergedPrerequisiteBranch(t *testing.T) {
	tk := sample
	tk.Prerequisites = []ticket.Prerequisite{{Identifier: "#81", Title: "Design record", Branch: "feat/gh-80", URL: "https://github.com/acme/widgets/pull/81"}}

	for name, p := range map[string]string{
		"fresh":   bImpl(t, tk, "beh-362"),
		"resumed": bResumed(t, tk, "beh-362"),
		"retry":   bResume(t, tk, "beh-362", "/wt"),
	} {
		for _, want := range []string{"#81", "`feat/gh-80`", "`origin/feat/gh-80`", "not yet merged", "shared"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s: prompt missing %q", name, want)
			}
		}
	}
}

// A ticket with no prerequisite reads exactly as before.
func TestImplementOmitsThePrerequisiteSectionWhenNone(t *testing.T) {
	if p := bImpl(t, sample, "beh-362"); strings.Contains(p, "not yet merged") {
		t.Error("a ticket with no open prerequisite must carry no prerequisite section")
	}
}
