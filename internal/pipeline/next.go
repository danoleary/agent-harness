package pipeline

import (
	"fmt"

	"github.com/beherd/agent-harness/internal/ticket"
)

// NextResolver is the Linear surface `pipeline --next` needs: a pure read to
// select the top-of-queue eligible ticket, plus the claim mutation. Claim-on-select
// (ADR-0003) is ResolveNext composing the two — selection stays side-effect-free so
// dry-run can resolve a ticket without mutating Linear.
type NextResolver interface {
	SelectNextTicket() (ticket.Ticket, bool, error)
	MoveToInProgress(identifier string) error
}

// NextSelection is the outcome of resolving `pipeline --next`.
type NextSelection struct {
	// Identifier is the selected ticket, or "" when the queue is empty or selection
	// errored. It is set even when Proceed is false on a dry-run preview, so the
	// caller can print the plan for it.
	Identifier string
	// PreClaimed records that ResolveNext claimed the ticket (a real run, not a
	// dry-run), so the implementation stage skips its own claim and releases on a
	// preflight failure (ADR-0003).
	PreClaimed bool
	// Proceed is true when the caller should run the pipeline over Identifier;
	// false means exit now with ExitCode (printing the plan first on a dry-run).
	Proceed bool
	// ExitCode is the process exit code when Proceed is false: 0 for an empty queue
	// or a dry-run preview (both clean), 1 for a selection or claim failure.
	ExitCode int
}

// ResolveNext implements `pipeline --next` ticket resolution (DESIGN.md
// "Single-shot auto-select"): select the top-of-queue eligible ticket and — unless
// dryRun — claim it (claim-on-select dequeue, ADR-0003). An empty queue is a clean
// exit 0 (a normal steady state); a dry-run resolves the real ticket and previews
// the plan without claiming (the guarantee tightens from "touches no Linear" to
// "mutates no Linear"). A selection or claim failure exits 1.
func ResolveNext(r NextResolver, dryRun bool, log Narrator) NextSelection {
	t, ok, err := r.SelectNextTicket()
	if err != nil {
		log.Event("pipeline — ticket selection failed: " + err.Error())
		return NextSelection{ExitCode: 1}
	}
	if !ok {
		// A normal steady state, not a failure — the loop will later wrap --next and
		// reads this exit-0 as "nothing to do" without remapping codes.
		log.Event("no eligible ticket — queue empty")
		return NextSelection{ExitCode: 0}
	}
	if dryRun {
		// Selection is side-effect-free, so previewing WHICH ticket --next would grab
		// is honest and useful — but it claims nothing.
		log.Event(fmt.Sprintf("dry-run — selected %s (not claiming)", t.Identifier))
		return NextSelection{Identifier: t.Identifier, ExitCode: 0}
	}
	if err := r.MoveToInProgress(t.Identifier); err != nil {
		log.Event(fmt.Sprintf("pipeline — claiming %s failed: %v", t.Identifier, err))
		return NextSelection{ExitCode: 1}
	}
	log.Event(fmt.Sprintf("selected %s (%s) — claimed → In Progress", t.Identifier, priorityOrNone(t.Priority)))
	return NextSelection{Identifier: t.Identifier, PreClaimed: true, Proceed: true}
}

// priorityOrNone renders a ticket's priority for narration, defaulting an unset
// priority to "No priority" (matching the implementation stage's narration).
func priorityOrNone(priority string) string {
	if priority == "" {
		return "No priority"
	}
	return priority
}
