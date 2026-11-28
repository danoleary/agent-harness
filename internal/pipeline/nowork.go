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

// OnMain reports whether origin/main, as of the last fetch, contains commit. An
// error is a check that could not run, not a commit main lacks.
type OnMain func(commit string) (bool, error)

// noWorkScreen is a NextResolver that passes over a ticket whose last run found
// nothing to hand off against the origin/main that is still current, for the
// ticket as it still reads, and offers the next ready ticket instead.
type noWorkScreen struct {
	NextResolver
	records nowork.Records
	main    MainSHA
	onMain  OnMain
	log     Narrator
	// skipped is the tickets the last selection passed over, in queue order.
	skipped []string
	// awaitingClose is those of skipped passed over as already resolved on main.
	awaitingClose []string
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
// A record naming the commit that already resolves the ticket does not go stale
// when main moves: the fix is still there. While onMain still finds that commit on
// main and the ticket reads as it did, the screen logs `skipping <key>: already
// resolved on main by <sha>; awaiting close` and passes over it, until a human
// closes the ticket (#453). A resolved record the loop's blocked label holds is
// passed through: the adapter offers the ticket only once a human removed the
// label (#480).
//
// It reads and never writes: the implementation stage keeps the record, and a
// dry-run selects through this screen too. A main that cannot be read, or an
// ancestry check that cannot run, fails OPEN, so the ticket dispatches as it did
// before the record existed.
func SkipNoWork(r NextResolver, records nowork.Records, main MainSHA, onMain OnMain, log Narrator) NextResolver {
	return &noWorkScreen{NextResolver: r, records: records, main: main, onMain: onMain, log: log, streamed: map[string]string{}}
}

func (s *noWorkScreen) held() []string {
	var held []string
	if unchanged := slices.DeleteFunc(slices.Clone(s.skipped), func(key string) bool { return slices.Contains(s.awaitingClose, key) }); len(unchanged) > 0 {
		held = append(held, heldReason(unchanged, "whose last run found no work on an unchanged main"))
	}
	if len(s.awaitingClose) > 0 {
		held = append(held, heldReason(s.awaitingClose, "already resolved on main, awaiting close"))
	}
	return append(held, heldBy(s.NextResolver)...)
}

// SelectNextTicket selects past every ticket whose last run found nothing on the
// main and the ticket that still stand.
func (s *noWorkScreen) SelectNextTicket(skip ...string) (ticket.Ticket, bool, error) {
	s.skipped, s.awaitingClose = nil, nil
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
		case rec.Held:
			// The loop's blocked label held the ticket, and only a human removing it
			// lets selection offer it again: the human's word that the commit does
			// not fix it (#480).
			s.log.Event(fmt.Sprintf("%s's last run found it already resolved on main by %s, and the blocked label the loop applied is gone — dispatching it again (#480)", t.Identifier, shortSHA(rec.ResolvedBy)))
		case mainErr != nil:
			s.log.Event(fmt.Sprintf("%s's last run found no work against origin/main %s, but origin/main could not be read (%v) — dispatching it (#330)", t.Identifier, short, mainErr))
		case main != rec.Base && rec.ResolvedBy == "":
			s.log.Event(fmt.Sprintf("%s's last run found no work against origin/main %s; main is now %s — dispatching it again (#330)", t.Identifier, short, shortSHA(main)))
		case nowork.Of(t) != rec.Ticket:
			s.log.Event(fmt.Sprintf("%s's last run found no work against origin/main %s; the ticket changed since — dispatching it again (#330)", t.Identifier, short))
		case rec.ResolvedBy == "":
			s.pass(t.Identifier, fmt.Sprintf("skipping %s: last run found no work against origin/main %s; unchanged (#330)", t.Identifier, short))
			skip = append(skip, t.Identifier)
			continue
		default:
			by := shortSHA(rec.ResolvedBy)
			held, err := s.onMain(rec.ResolvedBy)
			if err != nil {
				s.log.Event(fmt.Sprintf("%s's last run found it already resolved on main by %s, but whether main %s still has it could not be read (%v) — dispatching it (#330)", t.Identifier, by, shortSHA(main), err))
				break
			}
			if !held {
				s.log.Event(fmt.Sprintf("%s's last run found it already resolved on main by %s; main is now %s and no longer has it — dispatching it again (#330)", t.Identifier, by, shortSHA(main)))
				break
			}
			s.pass(t.Identifier, fmt.Sprintf("skipping %s: already resolved on main by %s; awaiting close (#330)", t.Identifier, by))
			s.awaitingClose = append(s.awaitingClose, t.Identifier)
			skip = append(skip, t.Identifier)
			continue
		}
		delete(s.streamed, t.Identifier)
		return t, true, nil
	}
}

// pass records that this selection passed over key, saying why in line.
func (s *noWorkScreen) pass(key, line string) {
	s.narrate(key, line)
	s.skipped = append(s.skipped, key)
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
