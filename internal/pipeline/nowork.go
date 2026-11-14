package pipeline

import (
	"fmt"
	"slices"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/nowork"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// MainSHA reads the commit origin/main points at, as of the last fetch.
type MainSHA func() (string, error)

// noWorkScreen is a NextResolver that passes over a ticket whose last run found
// nothing to hand off against the origin/main that is still current, for the
// ticket as it still reads, and offers the next ready ticket instead.
type noWorkScreen struct {
	NextResolver
	records nowork.Records
	main    MainSHA
	log     Narrator
	// skipped is the tickets the last selection passed over, in queue order.
	skipped []string
	// streamed is the skip line each ticket last sent to the loop stream, so a
	// skip that stands poll after poll streams once (#281).
	streamed map[string]string
}

// SkipNoWork screens r's selection against the record of runs that found nothing
// to hand off (#330, ADR-0053). A ticket whose last run ended with no commit
// ahead of origin/main is passed over while origin/main is still on the SHA that
// run worked against and the ticket's title, body and labels still read as they
// did when it was dispatched: a second run would find the same nothing. The
// screen logs `skipping <key>: last run found no work against origin/main <sha>;
// unchanged` and selects again past it. Once main moves or a human edits the
// ticket, it dispatches.
//
// It reads and never writes: the implementation stage keeps the record, and a
// dry-run selects through this screen too. A main that cannot be read fails OPEN,
// so the ticket dispatches as it did before the record existed.
func SkipNoWork(r NextResolver, records nowork.Records, main MainSHA, log Narrator) NextResolver {
	return &noWorkScreen{NextResolver: r, records: records, main: main, log: log, streamed: map[string]string{}}
}

func (s *noWorkScreen) held() []string {
	if len(s.skipped) == 0 {
		return heldBy(s.NextResolver)
	}
	return append([]string{heldReason(s.skipped, "whose last run found no work on an unchanged main")}, heldBy(s.NextResolver)...)
}

// SelectNextTicket selects past every ticket whose last run found nothing on the
// main and the ticket that still stand.
func (s *noWorkScreen) SelectNextTicket(skip ...string) (ticket.Ticket, bool, error) {
	s.skipped = nil
	skip = slices.Clone(skip)
	main, mainErr := "", error(nil)
	read := false
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
		rec, recorded := s.records.Read(t.Identifier)
		if !recorded {
			delete(s.streamed, t.Identifier)
			return t, true, nil
		}
		if !read {
			main, mainErr = s.main()
			read = true
		}
		short := shortSHA(rec.Base)
		switch {
		case mainErr != nil:
			s.log.Event(fmt.Sprintf("%s's last run found no work against origin/main %s, but origin/main could not be read (%v) — dispatching it (#330)", t.Identifier, short, mainErr))
		case main != rec.Base:
			s.log.Event(fmt.Sprintf("%s's last run found no work against origin/main %s; main is now %s — dispatching it again (#330)", t.Identifier, short, shortSHA(main)))
		case nowork.Of(t) != rec.Ticket:
			s.log.Event(fmt.Sprintf("%s's last run found no work against origin/main %s; the ticket changed since — dispatching it again (#330)", t.Identifier, short))
		default:
			s.narrate(t.Identifier, fmt.Sprintf("skipping %s: last run found no work against origin/main %s; unchanged (#330)", t.Identifier, short))
			s.skipped = append(s.skipped, t.Identifier)
			skip = append(skip, t.Identifier)
			continue
		}
		delete(s.streamed, t.Identifier)
		return t, true, nil
	}
}

// narrate logs a skip each poll, and sends it to the loop stream the first time
// it stands.
func (s *noWorkScreen) narrate(key, line string) {
	if s.streamed[key] == line {
		s.log.Event(line)
		return
	}
	s.streamed[key] = line
	s.log.Structured(loopstream.Record{Kind: loopstream.KindItemSkipped, Ticket: key, Message: line})
}

// forgetUnskipped drops the streamed line of every ticket this selection did not
// pass over: one that is skipped again later streams again.
func (s *noWorkScreen) forgetUnskipped() {
	for key := range s.streamed {
		if !slices.Contains(s.skipped, key) {
			delete(s.streamed, key)
		}
	}
}

// shortSHA abbreviates a commit SHA for narration.
func shortSHA(sha string) string {
	if len(sha) > 12 {
		return sha[:12]
	}
	return sha
}
