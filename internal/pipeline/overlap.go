package pipeline

import (
	"fmt"
	"slices"

	"github.com/danoleary/agent-harness/internal/overlap"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// InFlight reads the work in flight — the open harness PRs and the claimed
// branches, with the files each changes — once per selection (#97).
type InFlight func() ([]overlap.Work, error)

// overlapScreen is a NextResolver that defers a selected ticket whose predicted
// files overlap the work in flight, and offers the next ready ticket instead.
type overlapScreen struct {
	NextResolver
	inFlight InFlight
	branch   func(key string) string
	policy   overlap.Policy
	log      Narrator
}

// DeferOverlaps screens r's selection against the work in flight (#97,
// ADR-0031). A ticket that names files an open harness PR or a claimed branch
// already changes waits for this poll: the screen logs `<key> deferred: overlaps
// PR #N on <files>` and selects again past it. Nothing about the ticket changes,
// so the next poll re-checks it, and once the overlapping PR merges or closes it
// dispatches. branch names a Key's own branch, which is never work it waits on.
//
// A failed read of the work in flight fails OPEN: the ticket dispatches, as it
// did before the check existed, and the log says the check was skipped. The
// cost of a wrong dispatch is a conflict a human already fixes today; the cost
// of a stalled queue is every ticket.
func DeferOverlaps(r NextResolver, inFlight InFlight, branch func(key string) string, p overlap.Policy, log Narrator) NextResolver {
	return &overlapScreen{NextResolver: r, inFlight: inFlight, branch: branch, policy: p, log: log}
}

// SelectNextTicket selects past every ticket the work in flight defers.
func (s *overlapScreen) SelectNextTicket(skip ...string) (ticket.Ticket, bool, error) {
	if s.policy.Threshold <= 0 {
		return s.NextResolver.SelectNextTicket(skip...)
	}
	work, err := s.inFlight()
	if err != nil {
		s.log.Event("overlap check skipped — could not read the work in flight: " + err.Error())
		return s.NextResolver.SelectNextTicket(skip...)
	}
	skip = slices.Clone(skip)
	for {
		t, ok, err := s.NextResolver.SelectNextTicket(skip...)
		if err != nil || !ok {
			return t, ok, err
		}
		if slices.Contains(skip, t.Identifier) {
			// The adapter offered a ticket it was told to pass over: it has nothing
			// else to offer.
			return ticket.Ticket{}, false, nil
		}
		d, deferred := overlap.Check(t, s.branch(t.Identifier), work, s.policy)
		if !deferred {
			return t, true, nil
		}
		s.log.Event(fmt.Sprintf("%s %s", t.Identifier, d.Reason()))
		skip = append(skip, t.Identifier)
	}
}
