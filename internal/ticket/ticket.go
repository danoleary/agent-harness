// Package ticket holds the Linear ticket shape the harness needs (fetched
// host-side; see ADR-0001).
package ticket

import "strings"

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

// SubIssue is a child of an umbrella/batch ticket, carrying the title + body the
// implementation prompt inlines so a sandboxed session can implement it without
// reaching Linear (BEH-619).
type SubIssue struct {
	Identifier  string
	Title       string
	Description string
}
