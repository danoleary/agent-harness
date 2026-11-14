package pipeline

import (
	"errors"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/nowork"
	"github.com/danoleary/agent-harness/internal/overlap"
	"github.com/danoleary/agent-harness/internal/ticket"
)

// main is origin/main as the test sets it between polls.
type mainRef struct {
	sha string
	err error
}

func (m *mainRef) read() (string, error) { return m.sha, m.err }

const resolvedOn = "1c26ab0f3e5d9a7b2c4e6f8091a2b3c4d5e6f708"

// issue330 is #330 as selection read it when its first run was dispatched.
var issue330 = ticket.Ticket{
	Identifier:  "#330",
	Title:       "Stop re-dispatching a ticket right after a run found it already resolved on main",
	Description: "What happened: the first implementation run on this ticket ended with no handoff commit…",
	Labels:      []string{"agent-ready", "stage-flow"},
}

func noWorkRecords(t *testing.T) nowork.Records {
	return nowork.In(t.TempDir(), time.Now)
}

// #330, replayed: the first run on the ticket found the fix already on
// origin/main and made no commit, and about 20 seconds later the loop dispatched
// it again into the same nothing. Now the re-pick, with origin/main and the ticket
// unchanged, passes over it, says why, and dispatches the next ready ticket.
func TestSkipNoWorkPassesOverATicketWhoseLastRunFoundNothingOnTheSameMain(t *testing.T) {
	records := noWorkRecords(t)
	if err := records.Write("#330", resolvedOn, nowork.Of(issue330)); err != nil {
		t.Fatal(err)
	}
	q := &readyQueue{ready: []ticket.Ticket{issue330, {Identifier: "#331", Description: "Other work."}}}
	rec := &recorder{}

	sel := ResolveNext(SkipNoWork(q, records, (&mainRef{sha: resolvedOn}).read, rec), &mutexLock{}, false, "", rec)

	if !sel.Proceed || sel.Identifier != "#331" {
		t.Fatalf("selection = %+v, want #330 passed over and #331 dispatched", sel)
	}
	if !rec.saw("skipping #330: last run found no work against origin/main 1c26ab0f3e5d; unchanged (#330)") {
		t.Errorf("the skip must name the ticket and the base; events = %v", rec.events)
	}
	if len(q.ready) != 1 || q.ready[0].Identifier != "#330" {
		t.Errorf("a passed-over ticket stays in the ready queue unclaimed; ready = %v", q.ready)
	}
}

// When the passed-over ticket is the only ready one, the poll is held, not empty,
// and the stream says so once (#281).
func TestSkipNoWorkWithOnlyThatTicketReadyHoldsTheQueue(t *testing.T) {
	records := noWorkRecords(t)
	if err := records.Write("#330", resolvedOn, nowork.Of(issue330)); err != nil {
		t.Fatal(err)
	}
	q := &readyQueue{ready: []ticket.Ticket{issue330}}
	rec := &recorder{}
	r := SkipNoWork(q, records, (&mainRef{sha: resolvedOn}).read, rec)

	for poll := 0; poll < 2; poll++ {
		sel := ResolveNext(r, &mutexLock{}, false, "", rec)
		if want := "1 ticket(s) whose last run found no work on an unchanged main (#330)"; sel.Proceed || sel.Held != want {
			t.Fatalf("poll %d: selection = %+v, want nothing claimed and Held %q", poll, sel, want)
		}
	}
	streamed := 0
	for _, r := range rec.records {
		if r.Kind == loopstream.KindItemSkipped && r.Ticket == "#330" {
			streamed++
		}
	}
	if streamed != 1 {
		t.Errorf("the skip streamed %d times over two polls, want once", streamed)
	}
}

// The record only stands while nothing changed: a main that moved, or a ticket
// whose body or labels a human edited, dispatches the ticket again.
func TestSkipNoWorkDispatchesOnceMainOrTheTicketMoved(t *testing.T) {
	edited := issue330
	edited.Description += "\n\nIt is not on main: see #312."
	relabelled := issue330
	relabelled.Labels = []string{"agent-ready", "stage-flow", "redo"}
	for name, c := range map[string]struct {
		main   string
		ticket ticket.Ticket
		says   string
	}{
		"main moved":     {main: "dadf13a000000000000000000000000000000000", ticket: issue330, says: "#330's last run found no work against origin/main 1c26ab0f3e5d; main is now dadf13a00000 — dispatching it again (#330)"},
		"body edited":    {main: resolvedOn, ticket: edited, says: "#330's last run found no work against origin/main 1c26ab0f3e5d; the ticket changed since — dispatching it again (#330)"},
		"labels changed": {main: resolvedOn, ticket: relabelled, says: "#330's last run found no work against origin/main 1c26ab0f3e5d; the ticket changed since — dispatching it again (#330)"},
	} {
		records := noWorkRecords(t)
		if err := records.Write("#330", resolvedOn, nowork.Of(issue330)); err != nil {
			t.Fatal(err)
		}
		q := &readyQueue{ready: []ticket.Ticket{c.ticket}}
		rec := &recorder{}

		sel := ResolveNext(SkipNoWork(q, records, (&mainRef{sha: c.main}).read, rec), &mutexLock{}, false, "", rec)

		if !sel.Proceed || sel.Identifier != "#330" {
			t.Errorf("%s: selection = %+v, want #330 dispatched", name, sel)
		}
		if !rec.saw(c.says) {
			t.Errorf("%s: want %q; events = %v", name, c.says, rec.events)
		}
	}
}

// A main that cannot be read cannot confirm the record, so the ticket
// dispatches, as it did before the record existed.
func TestSkipNoWorkFailsOpenWhenMainCannotBeRead(t *testing.T) {
	records := noWorkRecords(t)
	if err := records.Write("#330", resolvedOn, nowork.Of(issue330)); err != nil {
		t.Fatal(err)
	}
	q := &readyQueue{ready: []ticket.Ticket{issue330}}
	rec := &recorder{}

	sel := ResolveNext(SkipNoWork(q, records, (&mainRef{err: errors.New("no origin/main")}).read, rec), &mutexLock{}, false, "", rec)

	if !sel.Proceed || sel.Identifier != "#330" {
		t.Fatalf("selection = %+v, want #330 dispatched when main cannot be read", sel)
	}
}

// The selection carries the fingerprint of the ticket it dispatched, which the
// implementation stage records if the run then finds nothing.
func TestResolveNextCarriesTheSelectedTicketsFingerprint(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{issue330}}

	sel := ResolveNext(q, &mutexLock{}, false, "", &recorder{})

	if sel.Fingerprint != nowork.Of(issue330) {
		t.Errorf("Fingerprint = %q, want the selected ticket's", sel.Fingerprint)
	}
}

// Under the overlap screen, a queue held by both screens names both reasons.
func TestAQueueHeldByOverlapsAndNoWorkNamesBoth(t *testing.T) {
	records := noWorkRecords(t)
	if err := records.Write("#330", resolvedOn, nowork.Of(issue330)); err != nil {
		t.Fatal(err)
	}
	q := &readyQueue{ready: []ticket.Ticket{issue330, {Identifier: "#1", Description: "Tidy internal/loop/loop.go."}}}
	rec := &recorder{}
	r := DeferOverlaps(SkipNoWork(q, records, (&mainRef{sha: resolvedOn}).read, rec), (&inFlight{work: []overlap.Work{openPR}}).read, branchOf, overlap.Policy{Threshold: 1}, rec)

	sel := ResolveNext(r, &mutexLock{}, false, "", rec)

	if want := "1 ticket(s) deferred by overlaps (#1); 1 ticket(s) whose last run found no work on an unchanged main (#330)"; sel.Proceed || sel.Held != want {
		t.Errorf("selection = %+v, want Held %q", sel, want)
	}
}
