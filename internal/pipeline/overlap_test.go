package pipeline

import (
	"errors"
	"testing"

	"github.com/danoleary/agent-harness/internal/overlap"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// readyQueue is a tracker's ready queue in order. Selection honours the skip list,
// as every adapter does; a claim takes the ticket off the queue.
type readyQueue struct {
	ready []ticket.Ticket
	// ignoreSkip models an adapter that does not honour the skip list.
	ignoreSkip bool
	selects    int
}

func (q *readyQueue) SelectNextTicket(skip ...tracker.Key) (ticket.Ticket, bool, error) {
	q.selects++
	for _, t := range q.ready {
		if !q.ignoreSkip && contains(skip, t.Identifier) {
			continue
		}
		return t, true, nil
	}
	return ticket.Ticket{}, false, nil
}

func (q *readyQueue) StillReady(key tracker.Key) (bool, error) {
	for _, t := range q.ready {
		if t.Identifier == key {
			return true, nil
		}
	}
	return false, nil
}

func (q *readyQueue) MoveToInProgress(key tracker.Key) error {
	for i, t := range q.ready {
		if t.Identifier == key {
			q.ready = append(q.ready[:i:i], q.ready[i+1:]...)
		}
	}
	return nil
}

func (q *readyQueue) ReleaseToTodo(tracker.Key) error  { return nil }
func (q *readyQueue) MoveToCanceled(tracker.Key) error { return nil }

func contains(keys []string, k string) bool {
	for _, x := range keys {
		if x == k {
			return true
		}
	}
	return false
}

// inFlight is the work in flight, as the test sets it between polls.
type inFlight struct {
	work []overlap.Work
	err  error
}

func (f *inFlight) read() ([]overlap.Work, error) { return f.work, f.err }

func branchOf(key string) string { return "feat/" + ticket.Slug(key) }

func screened(q *readyQueue, f *inFlight, p overlap.Policy, rec *recorder) NextResolver {
	return DeferOverlaps(q, f.read, branchOf, p, rec)
}

var openPR = overlap.Work{Label: "PR #10", Branch: "feat/gh-9", Files: []string{"internal/loop/loop.go"}}

// A ticket that names a file an open harness PR changes waits; the next ready
// ticket dispatches instead, and the log names the PR and the files (#97).
func TestDeferOverlapsSkipsToTheNextReadyTicket(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{
		{Identifier: "#1", Description: "Tidy internal/loop/loop.go."},
		{Identifier: "#2", Description: "Fix internal/stages/review.go."},
	}}
	rec := &recorder{}

	sel := ResolveNext(screened(q, &inFlight{work: []overlap.Work{openPR}}, overlap.Policy{Threshold: 1}, rec), &mutexLock{}, false, "", rec)

	if !sel.Proceed || sel.Identifier != "#2" {
		t.Fatalf("selection = %+v, want to proceed on #2", sel)
	}
	if !rec.saw("#1 deferred: overlaps PR #10 on internal/loop/loop.go") {
		t.Errorf("expected the deferral to name the PR and the file; events = %v", rec.events)
	}
}

// When every ready ticket overlaps, the poll is empty: nothing is claimed.
func TestDeferOverlapsWithEveryTicketDeferredIsAnEmptyPoll(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{{Identifier: "#1", Description: "Tidy internal/loop/loop.go."}}}
	rec := &recorder{}

	sel := ResolveNext(screened(q, &inFlight{work: []overlap.Work{openPR}}, overlap.Policy{Threshold: 1}, rec), &mutexLock{}, false, "", rec)

	if sel.Proceed || sel.ExitCode != 0 || sel.Err != nil {
		t.Errorf("selection = %+v, want a clean empty poll", sel)
	}
	if len(q.ready) != 1 {
		t.Error("a deferred ticket must stay in the ready queue")
	}
}

// Two siblings under one parent name the same module. The first dispatches; while
// its PR is open the second waits; on the first poll after that PR merges, the
// second dispatches. They run one after the other, not in parallel.
func TestSiblingsNamingOneModuleRunOneAfterAnother(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{
		{Identifier: "#1", Description: "Backdrop colour in `host-document-session`."},
		{Identifier: "#2", Description: "Page-turn speed in `host-document-session`."},
	}}
	f := &inFlight{}
	rec := &recorder{}
	poll := func() NextSelection {
		return ResolveNext(screened(q, f, overlap.Policy{Threshold: 1}, rec), &mutexLock{}, false, "", rec)
	}

	if sel := poll(); sel.Identifier != "#1" {
		t.Fatalf("first poll selected %+v, want #1", sel)
	}
	f.work = []overlap.Work{{Label: "PR #3", Branch: "feat/gh-1", Files: []string{"web/src/modules/space/host-document-session.ts"}}}
	if sel := poll(); sel.Proceed {
		t.Fatalf("second poll proceeded on %s while #1's PR is open", sel.Identifier)
	}
	f.work = nil // PR #3 merged
	if sel := poll(); !sel.Proceed || sel.Identifier != "#2" {
		t.Fatalf("poll after the merge = %+v, want to proceed on #2", sel)
	}
}

// The escape label dispatches a ticket the check would otherwise defer.
func TestDeferOverlapsHonoursTheEscapeLabel(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{{Identifier: "#1", Description: "Tidy internal/loop/loop.go.", Labels: []string{"parallel-ok"}}}}
	rec := &recorder{}

	sel := ResolveNext(screened(q, &inFlight{work: []overlap.Work{openPR}}, overlap.Policy{Threshold: 1, EscapeLabel: "parallel-ok"}, rec), &mutexLock{}, false, "", rec)

	if !sel.Proceed || sel.Identifier != "#1" {
		t.Errorf("selection = %+v, want to proceed on #1", sel)
	}
}

// Not knowing what is in flight is the world before #97, not a reason to stall
// the queue: the ticket dispatches and the log says the check was skipped.
func TestDeferOverlapsFailsOpenWhenTheWorkInFlightCannotBeRead(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{{Identifier: "#1", Description: "Tidy internal/loop/loop.go."}}}
	rec := &recorder{}

	sel := ResolveNext(screened(q, &inFlight{err: errors.New("gh down")}, overlap.Policy{Threshold: 1}, rec), &mutexLock{}, false, "", rec)

	if !sel.Proceed || sel.Identifier != "#1" {
		t.Errorf("selection = %+v, want to proceed on #1", sel)
	}
	if !rec.saw("gh down") {
		t.Errorf("expected the skipped check to be narrated; events = %v", rec.events)
	}
}

// A check that is off reads nothing in flight.
func TestDeferOverlapsOffReadsNothing(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{{Identifier: "#1", Description: "Tidy internal/loop/loop.go."}}}
	read := 0
	r := DeferOverlaps(q, func() ([]overlap.Work, error) { read++; return []overlap.Work{openPR}, nil }, branchOf, overlap.Policy{}, &recorder{})

	if tk, ok, _ := r.SelectNextTicket(); !ok || tk.Identifier != "#1" {
		t.Errorf("selected %v %v, want #1", tk.Identifier, ok)
	}
	if read != 0 {
		t.Errorf("read the work in flight %d times with the check off", read)
	}
}

// An adapter that offers a skipped ticket again has nothing else to offer: the
// screen stops rather than spinning on it.
func TestDeferOverlapsStopsWhenTheAdapterOffersASkippedTicket(t *testing.T) {
	q := &readyQueue{ignoreSkip: true, ready: []ticket.Ticket{{Identifier: "#1", Description: "Tidy internal/loop/loop.go."}}}
	r := DeferOverlaps(q, (&inFlight{work: []overlap.Work{openPR}}).read, branchOf, overlap.Policy{Threshold: 1}, &recorder{})

	if _, ok, err := r.SelectNextTicket(); ok || err != nil {
		t.Errorf("ok=%v err=%v, want an empty selection", ok, err)
	}
	if q.selects > 2 {
		t.Errorf("selected %d times, want at most 2", q.selects)
	}
}
