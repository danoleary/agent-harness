// Package pr builds the templated title and body for the pull request the
// harness opens after a green review (ADR-0002: the harness owns push + PR,
// host-side). The template is deterministic — ticket id + commit subjects +
// a generated-by trailer — so no model session is needed to author the PR (a
// Haiku PR-author session is explicitly deferred; see DESIGN.md).
package pr

import (
	"strings"

	"github.com/beherd/agent-harness/internal/ticket"
)

// trailer marks the PR as harness-generated so a human reading it knows it was
// not hand-authored.
const trailer = "🤖 Opened by the BeHerd agent harness after a green host-side gate re-run."

// BuildTitle builds the PR title: the ticket id and its title, so the PR is
// identifiable at a glance and links back to the issue.
func BuildTitle(t ticket.Ticket) string {
	return t.Identifier + ": " + t.Title
}

// BuildBody builds the PR body from the ticket and the feature branch's commit
// subjects (ahead of main). It references the ticket so Linear associates the PR,
// lists the commits as a summary of the work, and ends with the generated-by
// trailer. An empty subject list yields a body with no commit bullets rather than
// a stray empty one.
func BuildBody(t ticket.Ticket, commitSubjects []string) string {
	var b strings.Builder

	b.WriteString("Resolves " + t.Identifier + ".")
	if t.URL != "" {
		b.WriteString("\n\n" + t.URL)
	}

	if len(commitSubjects) > 0 {
		b.WriteString("\n\n## Commits\n\n")
		for _, s := range commitSubjects {
			if strings.TrimSpace(s) == "" {
				continue
			}
			b.WriteString("- " + s + "\n")
		}
	}

	b.WriteString("\n---\n\n" + trailer + "\n")
	return b.String()
}
