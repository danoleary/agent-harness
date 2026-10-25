package prloop

import (
	"fmt"
	"strings"
)

// planFor is one line saying what the Pipeline will do to it.
func planFor(it Item) string {
	switch {
	case it.Kind == CIRed:
		return "re-run the failed checks once, then run a CIFix session over the failing logs, re-run every gate, push the committed fix and watch CI"
	case it.Strategy().String() == "rebase":
		return "run a RebaseFix session to rebase onto origin/main, re-run every gate, push --force-with-lease and watch CI"
	default:
		return "run a RebaseFix session to merge origin/main into the branch (its history is kept: a human opted it in), re-run every gate, push without force and watch CI"
	}
}

// RenderPlan prints the Source's judgement of every open PR for `loop <name>
// --dry-run`: which PRs it would work and how, which it would hand to a human, and
// why it skips the rest.
func RenderPlan(cands []Candidate) string {
	if len(cands) == 0 {
		return "PR loop dry-run: no open PRs.\n"
	}
	var b strings.Builder
	b.WriteString("PR loop dry-run — nothing below was changed.\n")
	for _, c := range cands {
		fmt.Fprintf(&b, "\nPR #%d  %s @ %s  %s\n", c.PR.Number, c.PR.HeadBranch, short(c.PR.HeadSHA), c.PR.Title)
		switch {
		case c.Skip != "":
			fmt.Fprintf(&b, "  skip: %s\n", c.Skip)
		case c.Exhausted:
			fmt.Fprintf(&b, "  %s, but its %d-attempt budget is used up: would comment with what was tried and label it blocked\n", c.Kind.describe(), c.Attempts)
		default:
			fmt.Fprintf(&b, "  work: %s (attempt %d) — %s\n", c.Kind.describe(), c.Attempts+1, planFor(c.Item))
		}
	}
	return b.String()
}
