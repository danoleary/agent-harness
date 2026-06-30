// Package ticket holds the Linear ticket shape the harness needs (fetched
// host-side; see ADR-0001).
package ticket

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

// SubIssue is a child of an umbrella/batch ticket, carrying the title + body the
// implementation prompt inlines so a sandboxed session can implement it without
// reaching Linear (BEH-619).
type SubIssue struct {
	Identifier  string
	Title       string
	Description string
}
