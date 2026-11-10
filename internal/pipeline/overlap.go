package pipeline

import (
	"fmt"
	"slices"
	"strings"

	"github.com/danoleary/agent-harness/internal/loopstream"
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
	// deferred is the tickets the last selection passed over, in queue order: a
	// selection that ends with nothing to offer holds them, not an empty queue.
	deferred []string
	// streamed is the deferral line each ticket last sent to the loop stream, so
	// a deferral that stands poll after poll streams once (#281).
	streamed map[string]string
}

// heldQueue is a NextResolver that can say which tickets its last selection
// deferred: an empty selection over them is a held queue, not an empty one.
type heldQueue interface {
	held() []string
}

// DeferOverlaps screens r's selection against the work in flight (#97,
// ADR-0032). A ticket that names files an open harness PR or a claimed branch
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
	return &overlapScreen{NextResolver: r, inFlight: inFlight, branch: branch, policy: p, log: log, streamed: map[string]string{}}
}

func (s *overlapScreen) held() []string { return s.deferred }

// SelectNextTicket selects past every ticket the work in flight defers.
func (s *overlapScreen) SelectNextTicket(skip ...string) (ticket.Ticket, bool, error) {
	s.deferred = nil
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
		if err != nil {
			return t, false, err
		}
		if !ok {
			s.forgetUndeferred()
			return t, false, nil
		}
		if slices.Contains(skip, t.Identifier) {
			// The adapter offered a ticket it was told to pass over: it has nothing
			// else to offer.
			s.forgetUndeferred()
			return ticket.Ticket{}, false, nil
		}
		d, deferred := overlap.Check(t, s.branch(t.Identifier), work, s.policy)
		if !deferred {
			delete(s.streamed, t.Identifier)
			return t, true, nil
		}
		s.narrate(t.Identifier, d)
		s.deferred = append(s.deferred, t.Identifier)
		skip = append(skip, t.Identifier)
	}
}

// narrate logs a deferral each poll, and sends it to the loop stream the first
// time it stands, so a queue the screen holds says why in loop.jsonl (#281)
// without repeating itself every poll interval.
func (s *overlapScreen) narrate(key string, d overlap.Deferral) {
	line := fmt.Sprintf("%s %s", key, d.Reason())
	if s.streamed[key] == line {
		s.log.Event(line)
		return
	}
	s.streamed[key] = line
	s.log.Structured(loopstream.Record{Kind: loopstream.KindItemSkipped, Ticket: key, Message: line})
}

// forgetUndeferred drops the streamed line of every ticket this selection, which
// saw the whole queue, did not defer: one that defers again later streams again.
func (s *overlapScreen) forgetUndeferred() {
	for key := range s.streamed {
		if !slices.Contains(s.deferred, key) {
			delete(s.streamed, key)
		}
	}
}

// maxHeldKeys bounds the tickets a held-queue line names.
const maxHeldKeys = 5

// heldReason words a held queue: `2 ticket(s) deferred by overlaps (#1, #2)`.
func heldReason(keys []string) string {
	named, more := keys, ""
	if len(named) > maxHeldKeys {
		named, more = named[:maxHeldKeys], fmt.Sprintf(" and %d more", len(keys)-maxHeldKeys)
	}
	return fmt.Sprintf("%d ticket(s) deferred by overlaps (%s%s)", len(keys), strings.Join(named, ", "), more)
}
