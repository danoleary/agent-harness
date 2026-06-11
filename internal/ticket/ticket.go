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
}
