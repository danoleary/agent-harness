package pipeline

import (
	"fmt"
	"sort"
	"strings"

	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// NextResolver is the tracker surface `pipeline --next` needs: a pure read to
// select the top-of-queue eligible ticket, plus the queue the selected ticket's
// lease claims through, with the read that confirms the claim is this run's
// (#89). Claim-on-select (ADR-0003) is ResolveNext composing the two — selection
// stays side-effect-free so dry-run can resolve a ticket without mutating the
// tracker.
type NextResolver interface {
	SelectNextTicket(skip ...string) (ticket.Ticket, bool, error)
	lease.Exclusive
}

// maxLostClaims bounds how many selected tickets one poll may lose to other
// instances before it gives up and idles. Each loss names a ticket another
// instance holds, so a queue this contended is one the next poll serves better.
const maxLostClaims = 5

// NextSelection is the outcome of resolving `pipeline --next`.
type NextSelection struct {
	// Identifier is the selected ticket, or "" when the queue is empty or selection
	// errored. It is set even when Proceed is false on a dry-run preview, so the
	// caller can print the plan for it.
	Identifier string
	// Lease is the run's claim on the selected ticket, held because ResolveNext
	// claimed it (ADR-0003). Nil on a dry-run, which claims nothing.
	Lease *lease.Lease
	// Proceed is true when the caller should run the pipeline over Identifier;
	// false means exit now with ExitCode (printing the plan first on a dry-run).
	Proceed bool
	// ExitCode is the process exit code when Proceed is false: 0 for an empty queue
	// or a dry-run preview (both clean), 1 for a selection or claim failure.
	ExitCode int
	// Err is the selection or claim failure behind ExitCode 1, so the daemon can
	// tell "the tracker is unreachable" from "nothing to do" — the two looked the
	// same in the loop log, and a dead token idled as an empty queue for minutes.
	Err error
}

// ResolveNext implements `pipeline --next` ticket resolution (DESIGN.md
// "Single-shot auto-select"): select the top-of-queue eligible ticket and — unless
// dryRun — claim it (claim-on-select dequeue, ADR-0003). An empty queue is a clean
// exit 0 (a normal steady state); a dry-run resolves the real ticket and previews
// the plan without claiming (the guarantee tightens from "touches no Linear" to
// "mutates no Linear"). A selection or claim failure exits 1. queueHint, when
// set, is appended to the empty-queue narration so it says how a ticket gets
// queued (config.TrackerConfig.QueueHint).
//
// The claim is exclusive (#89): under lock, which every loop instance on the
// Consumer shares, the ticket is re-read and claimed only if it is still
// unclaimed. A ticket another instance won is lost, not an error — this run
// claimed nothing, so it releases nothing — and selection runs again. A queue
// that keeps offering only tickets other instances hold is an empty poll.
func ResolveNext(r NextResolver, lock lease.Locker, dryRun bool, queueHint string, log Narrator) NextSelection {
	lost := map[string]bool{}
	for {
		t, ok, err := r.SelectNextTicket()
		if err != nil {
			log.Event("pipeline — ticket selection failed: " + err.Error())
			return NextSelection{ExitCode: 1, Err: err}
		}
		if !ok {
			// A normal steady state, not a failure — the loop will later wrap --next and
			// reads this exit-0 as "nothing to do" without remapping codes.
			msg := "no eligible ticket — queue empty"
			if queueHint != "" {
				msg += "; " + queueHint
			}
			log.Event(msg)
			return NextSelection{ExitCode: 0}
		}
		if dryRun {
			// Selection is side-effect-free, so previewing WHICH ticket --next would grab
			// is honest and useful — but it claims nothing.
			log.Event(fmt.Sprintf("dry-run — selected %s (not claiming)", t.Identifier))
			return NextSelection{Identifier: t.Identifier, ExitCode: 0}
		}
		if lost[t.Identifier] || len(lost) >= maxLostClaims {
			// The queue has not caught up with the claims other instances made, so
			// selecting again would only offer them again.
			log.Event(fmt.Sprintf("no unclaimed ticket — every selected ticket is held by another run (%s); re-polling later", strings.Join(sortedKeys(lost), ", ")))
			return NextSelection{ExitCode: 0}
		}
		claim, won, err := lease.Claim(r, lock, t.Identifier)
		if err != nil {
			log.Event(fmt.Sprintf("pipeline — claiming %s failed: %v", t.Identifier, err))
			return NextSelection{ExitCode: 1, Err: err}
		}
		if !won {
			log.Event(fmt.Sprintf("%s was claimed by another run first — selecting again", t.Identifier))
			lost[t.Identifier] = true
			continue
		}
		log.Structured(loopstream.Record{
			Kind:    loopstream.KindTicketSelected,
			Ticket:  t.Identifier,
			Message: fmt.Sprintf("selected %s (%s) — claimed → In Progress", t.Identifier, priorityOrNone(t.Priority)),
		})
		return NextSelection{Identifier: t.Identifier, Lease: claim, Proceed: true}
	}
}

// sortedKeys is a set's members in order, for narration.
func sortedKeys(set map[string]bool) []string {
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// priorityOrNone renders a ticket's priority for narration, defaulting an unset
// priority to "No priority" (matching the implementation stage's narration).
func priorityOrNone(priority string) string {
	if priority == "" {
		return "No priority"
	}
	return priority
}
