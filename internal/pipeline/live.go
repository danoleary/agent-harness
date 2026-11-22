package pipeline

import (
	"fmt"
	"slices"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// Live reads whether a run working key is live right now: its run lock is held.
type Live func(key string) (bool, error)

// liveScreen is a NextResolver that passes over a ticket a live run is working.
type liveScreen struct {
	NextResolver
	live Live
	log  Narrator
	// skipped is the tickets the last selection passed over, in queue order.
	skipped []string
	// streamed is the skip line each ticket last sent to the loop stream, so a
	// skip that stands poll after poll streams once (#281).
	streamed map[string]string
}

// SkipLive screens r's selection against the runs that are live (#366,
// ADR-0061). A ready ticket whose run lock another run holds is passed over: a
// second run would share its worktree, each seeing the other's edits. The screen
// logs `skipping <key>: another live run holds its run lock` and selects again
// past it; once that run ends, the ticket dispatches. A run lock that cannot be
// read fails CLOSED, unlike the other screens: the cost of a wrong skip is one
// poll, and of a wrong dispatch two runs on one worktree.
func SkipLive(r NextResolver, live Live, log Narrator) NextResolver {
	return &liveScreen{NextResolver: r, live: live, log: log, streamed: map[string]string{}}
}

func (s *liveScreen) held() []string {
	if len(s.skipped) == 0 {
		return heldBy(s.NextResolver)
	}
	return append([]string{heldReason(s.skipped, "whose run is live")}, heldBy(s.NextResolver)...)
}

// SelectNextTicket selects past every ticket a live run is working.
func (s *liveScreen) SelectNextTicket(skip ...string) (ticket.Ticket, bool, error) {
	s.skipped = nil
	skip = slices.Clone(skip)
	for {
		t, ok, err := s.NextResolver.SelectNextTicket(skip...)
		if err != nil || !ok {
			s.forgetUnskipped()
			return t, ok, err
		}
		if slices.Contains(skip, t.Identifier) {
			// The adapter offered a ticket it was told to pass over: it has nothing
			// else to offer.
			s.forgetUnskipped()
			return ticket.Ticket{}, false, nil
		}
		live, err := s.live(t.Identifier)
		switch {
		case err != nil:
			s.narrate(t.Identifier, fmt.Sprintf("skipping %s: could not read whether its run is live (%v) (#366)", t.Identifier, err))
		case live:
			s.narrate(t.Identifier, fmt.Sprintf("skipping %s: another live run holds its run lock (#366)", t.Identifier))
		default:
			delete(s.streamed, t.Identifier)
			return t, true, nil
		}
		s.skipped = append(s.skipped, t.Identifier)
		skip = append(skip, t.Identifier)
	}
}

// narrate logs a skip each poll, and sends it to the loop stream the first time
// it stands.
func (s *liveScreen) narrate(key, line string) {
	if s.streamed[key] == line {
		s.log.Event(line)
		return
	}
	s.streamed[key] = line
	s.log.Structured(loopstream.Record{Kind: loopstream.KindItemSkipped, Ticket: key, Message: line})
}

// forgetUnskipped drops the streamed line of every ticket this selection did not
// pass over: one that is skipped again later streams again.
func (s *liveScreen) forgetUnskipped() {
	for key := range s.streamed {
		if !slices.Contains(s.skipped, key) {
			delete(s.streamed, key)
		}
	}
}
