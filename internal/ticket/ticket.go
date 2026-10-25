// Package ticket holds the Linear ticket shape the harness needs (fetched
// host-side; see ADR-0001).
package ticket

import (
	"regexp"
	"strings"
)

// Ticket is a Linear ticket, as the harness needs it.
type Ticket struct {
	// Identifier is the human identifier, e.g. "BEH-362".
	Identifier string
	Title      string
	// Description is the markdown body (includes the acceptance criteria).
	Description string
	// Priority is the priority name, e.g. "Urgent" — for console narration.
	Priority string
	// URL is the ticket's web URL.
	URL string
	// TeamID is the UUID of the ticket's team — findings are filed back into it.
	TeamID string
	// SubIssues are the ticket's child sub-issues (BEH-619). An umbrella/batch
	// ticket defers its real work to these, and the sandbox is isolated from Linear
	// (ADR-0002), so the harness fetches each child host-side and inlines its full
	// spec into the implementation prompt — the sandbox never reaches Linear.
	SubIssues []SubIssue
	// Prerequisites are the still-open, unmerged pull requests the ticket's body
	// names (#82). Selection holds such a ticket in the queue; when one is
	// dispatched anyway, the envelope names each branch so the session reads the
	// design from it instead of assuming the two PRs are disjoint, and the CI watch
	// knows a later conflict with base may be that PR's merge.
	Prerequisites []Prerequisite
}

// Prerequisite is one open pull request a ticket builds on.
type Prerequisite struct {
	// Identifier is the PR's Key, e.g. "#81".
	Identifier string
	Title      string
	// Branch is the PR's unmerged head branch, e.g. "feat/gh-80".
	Branch string
	URL    string
}

// Slug is the filesystem- and ref-safe name a Key's worktree and branch are keyed
// off: `.claude/worktrees/<slug>` and `<branch_prefix>/<slug>`. A Linear or Jira
// Key lowercases as-is (`BEH-362` → `beh-362`). A GitHub Key is `#30`, and a `#`
// in a path or ref turns the rest of any unquoted shell line into a comment, so
// it becomes `gh-30`. Anything else outside [a-z0-9-] collapses to one hyphen.
func Slug(key string) string {
	k := strings.ToLower(strings.TrimSpace(key))
	if strings.HasPrefix(k, "#") {
		k = "gh-" + k[1:]
	}
	var b strings.Builder
	hyphen := false
	for _, r := range k {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
			hyphen = false
			continue
		}
		if !hyphen && b.Len() > 0 {
			b.WriteByte('-')
			hyphen = true
		}
	}
	return strings.TrimSuffix(b.String(), "-")
}

// extractionVerbs signal that a ticket creates shared code (a new module,
// component, or helper pulled out of existing files).
var extractionVerbs = []string{"extract", "consolidate", "factor out", "pull out", "pull into", "de-duplicate", "deduplicate", "dedupe"}

// rewireVerbs signal that a ticket then adopts that shared code across several
// existing call sites.
var rewireVerbs = []string{"rewire", "call site", "call-site", "callsite", "consume", "consumes", "consuming", "caller", "callers", "adopt"}

// IsLargeRefactor reports whether the ticket describes a multi-file
// "extract-and-rewire" refactor: extract shared code into new modules AND rewire
// several existing call sites to consume it. These are inherently sequential
// (each call site rewired in turn) and routinely overrun a single tdd cap
// mid-surgery, leaving an uncompilable half-rewired checkpoint that must be
// redone from scratch (BEH-441, BEH-688 Symptom 2) — so the harness grants them
// a larger implementation cap.
//
// The signal is the CO-OCCURRENCE of an extraction verb and a rewire/consume
// verb: either alone is common in ordinary tickets ("extract this helper", "the
// dock consumes the hook"), but together they mark the extract-N-then-rewire-N
// shape. Kept deliberately high-precision — a false positive only over-grants a
// cap, but broadening it would slow the common case, so scope stays narrow.
func (t Ticket) IsLargeRefactor() bool {
	text := strings.ToLower(t.Title + "\n" + t.Description)
	return containsAny(text, extractionVerbs) && containsAny(text, rewireVerbs)
}

func containsAny(text string, needles []string) bool {
	for _, n := range needles {
		if strings.Contains(text, n) {
			return true
		}
	}
	return false
}

// openQuestionSignals mark a section heading as one a human was meant to settle
// before the ticket was ready for an agent (#90). Matched against the lowercased
// heading with "agent ready" folded to "agent-ready". Narrow on purpose: a heading
// that merely mentions readiness ("Why this is agent-ready") is not one.
var openQuestionSignals = []string{
	"open question", "unresolved question", "for a human",
	"not agent-ready", "not yet agent-ready", "before this is agent-ready",
}

// settledHeading marks an open-questions heading whose questions are answered.
// Word-bounded, so "unresolved" never reads as "resolved".
var settledHeading = regexp.MustCompile(`\b(resolved|answered|decided)\b`)

// OpenQuestions returns the headings of the ticket body's sections that still
// list questions for a human, in order: "Open questions (for a human before this
// is agent-ready)" in #55, "Not yet agent-ready", "Questions for a human". A
// Markdown heading or a line that is all bold counts as a heading; a heading in a
// fenced block, one marked resolved, and a section whose content is empty or
// "None" do not. The phrase in running prose is not a section, so it never counts.
//
// It is a heuristic for an advisory, never a skip: a ready label is a human's
// call, and the open questions may not block the work.
func (t Ticket) OpenQuestions() []string {
	var found []string
	open := ""
	var content strings.Builder
	flush := func() {
		if open != "" && !emptySection(content.String()) {
			found = append(found, open)
		}
		open = ""
		content.Reset()
	}
	fence := ""
	for _, line := range strings.Split(t.Description, "\n") {
		trimmed := strings.TrimSpace(line)
		if marker := fenceOpener(trimmed); fence == "" && marker != "" {
			fence = marker
		} else if fence != "" {
			if strings.HasPrefix(trimmed, fence) {
				fence = ""
			}
			content.WriteString("code\n")
			continue
		}
		heading, ok := headingText(line)
		if !ok {
			content.WriteString(line + "\n")
			continue
		}
		flush()
		if isOpenQuestionsHeading(heading) {
			open = heading
		}
	}
	flush()
	return found
}

// fenceOpener returns the ``` or ~~~ run that opens a fenced block, or "".
func fenceOpener(trimmed string) string {
	for _, f := range []string{"```", "~~~"} {
		if strings.HasPrefix(trimmed, f) {
			return f
		}
	}
	return ""
}

// headingText returns a line's heading text when the line is an ATX heading
// (`## Open questions`) or wholly bold (`**Open questions:**`).
func headingText(line string) (string, bool) {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 {
		return "", false
	}
	trimmed = strings.TrimSpace(trimmed)
	if hashes := len(trimmed) - len(strings.TrimLeft(trimmed, "#")); hashes >= 1 && hashes <= 6 {
		rest := trimmed[hashes:]
		if rest == "" || rest[0] != ' ' && rest[0] != '\t' {
			return "", false
		}
		return strings.TrimSpace(strings.TrimRight(strings.TrimSpace(rest), "#")), true
	}
	for _, m := range []string{"**", "__"} {
		inner := strings.TrimSuffix(trimmed, ":")
		if len(inner) > 2*len(m) && strings.HasPrefix(inner, m) && strings.HasSuffix(inner, m) {
			text := inner[len(m) : len(inner)-len(m)]
			if !strings.Contains(text, m) {
				return strings.TrimSpace(text), true
			}
		}
	}
	return "", false
}

func isOpenQuestionsHeading(heading string) bool {
	h := strings.ReplaceAll(strings.ToLower(heading), "agent ready", "agent-ready")
	return containsAny(h, openQuestionSignals) && !settledHeading.MatchString(h)
}

// emptySection reports whether a section's content says nothing is open.
func emptySection(content string) bool {
	switch strings.Trim(strings.ToLower(content), " \t\n-*_.:;!()") {
	case "", "none", "n/a", "no", "nothing":
		return true
	}
	return false
}

// SubIssue is a child of an umbrella/batch ticket, carrying the title + body the
// implementation prompt inlines so a sandboxed session can implement it without
// reaching Linear (BEH-619).
type SubIssue struct {
	Identifier  string
	Title       string
	Description string
}
