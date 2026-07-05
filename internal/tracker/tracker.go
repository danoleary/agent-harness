// Package tracker is the host-side issue-tracker port (ADR-0010, supersedes
// ADR-0001). The harness owns all tracker I/O on the host; no tracker credential
// or MCP ever enters the sandbox. The port is the lowest common denominator of the
// three layers the harness uses — ticket source, queue+claim/release, findings
// sink — so Linear, Jira, and GitHub Issues each sit behind it as one adapter.
//
// The interface stays LCD by design: capabilities unique to one tracker (Linear
// sub-issues vs Jira sub-tasks vs GitHub task-lists) are normalized to "children"
// or dropped, never leaked into the port.
package tracker

import (
	"time"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/ticket"
)

// Key is the tracker-agnostic ticket identifier (`BEH-123`, `PROJ-123`, `#123`)
// the harness slugs branches from and greps `main` history with (CONTEXT.md). The
// harness treats it as an opaque string the adapter produces — nothing downstream
// may assume the `BEH-` shape. It is a string alias, not a distinct type, so the
// identifier threaded through the existing git/prompt/stage plumbing keeps flowing
// unchanged; the type exists to name the concept at the port boundary.
type Key = string

// FileFindingOptions parameterises filing one finding into the sink.
type FileFindingOptions struct {
	// TeamID is the tracker-native container the finding is created in (a Linear
	// team UUID; a Jira project; a GitHub repo). Opaque to the harness.
	TeamID string
	// RelatedKey is the Key of the ticket whose session surfaced this finding.
	RelatedKey Key
}

// CreatedIssue is the result of filing a finding: the new issue's Key and URL.
type CreatedIssue struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

// ExistingFinding is an already-filed finding, returned by the sink so filing can
// skip duplicates. Key is the dedup fingerprint recovered from the issue body's
// marker (empty when the issue carries none). Closed is true when the issue is in
// a terminal state — a closed match must NOT suppress a re-file, so a wontfix
// can't permanently mask a real regression.
type ExistingFinding struct {
	Identifier string
	Title      string
	Key        string
	Closed     bool
}

// TicketSource resolves a Key to the ticket context the harness injects into a
// session (ADR-0010 layer 1). Children (Linear sub-issues / Jira sub-tasks) arrive
// normalized on the ticket, per the LCD constraint.
type TicketSource interface {
	FetchTicket(key Key) (ticket.Ticket, error)
}

// InProgressClaim is one agent-claimed In Progress ticket, carrying the raw signals
// the stale-claim reaper needs to decide abandonment (BEH-677): when it was claimed
// (StartedAt) and whether it has anything to show for the claim (HasLinkedPR). A
// claim past the grace TTL with no linked PR (and no branch, checked separately) is
// a dead claim to release back to Todo.
type InProgressClaim struct {
	Identifier  string
	StartedAt   time.Time
	HasLinkedPR bool
}

// Queue is the select + claim/release layer (ADR-0010 layer 2): pick the next
// ready ticket and move it to/from an in-progress state. SelectNextTicket is a
// pure read (ok=false means the queue is empty) so claim-on-select (ADR-0003) is
// the caller composing it with MoveToInProgress; ReleaseToTodo undoes a claim that
// yielded nothing. ListInProgressClaims is the reaper's read: the agent-claimed
// In Progress set, so a claim stranded by a dead agent (no branch/PR past the TTL)
// can be released back to Todo (BEH-677).
type Queue interface {
	SelectNextTicket() (ticket.Ticket, bool, error)
	MoveToInProgress(key Key) error
	ReleaseToTodo(key Key) error
	// MoveToCanceled moves a ticket into the tracker's terminal canceled state,
	// consuming a recommend-close verdict (BEH-682): a run that found the branch
	// makes zero net change is a superseded/duplicate ticket, closed rather than
	// left claimed. Terminal, so the ticket leaves both the select and reap pools —
	// unlike ReleaseToTodo, which returns it to the queue for a later run.
	MoveToCanceled(key Key) error
	ListInProgressClaims() ([]InProgressClaim, error)
}

// FindingsSink files and dedups findings (ADR-0010 layer 3). SearchFindings lists
// the open filed findings so File can skip duplicates; RecordOccurrence bumps an
// already-tracked finding instead of filing a duplicate. Best-effort by contract
// (ADR-0001): callers degrade every failure to narration, never a crash.
type FindingsSink interface {
	FileFinding(f findings.Finding, opts FileFindingOptions) (CreatedIssue, error)
	SearchFindings(teamID string) ([]ExistingFinding, error)
	RecordOccurrence(key, relatedKey Key) (count int, err error)
}

// Tracker is the full host-side port: the three ADR-0010 layers plus AddComment, a
// cross-cutting breadcrumb the harness leaves on a worked ticket when a reviewed
// branch can't be auto-rebased (BEH-581). One adapter — Linear, Jira, or GitHub
// Issues — implements it, selected by `.agent-harness/config` (ADR-0008).
type Tracker interface {
	TicketSource
	Queue
	FindingsSink
	AddComment(key Key, body string) error
}
