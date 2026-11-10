package pipeline

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/loopstream"
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

// A queue the screen deferred whole is held, not empty (#281): the loop sat on
// "queue empty" for hours over 20 ready tickets. The selection and the narration
// say how many tickets wait and which, a dry run included, and the deferral lines
// reach the loop stream.
func TestDeferOverlapsWithEveryTicketDeferredSaysTheQueueIsHeld(t *testing.T) {
	for _, dryRun := range []bool{false, true} {
		q := &readyQueue{ready: []ticket.Ticket{
			{Identifier: "#1", Description: "Tidy internal/loop/loop.go."},
			{Identifier: "#2", Description: "Split `loop/loop.go`."},
		}}
		rec := &recorder{}

		sel := ResolveNext(screened(q, &inFlight{work: []overlap.Work{openPR}}, overlap.Policy{Threshold: 1}, rec), &mutexLock{}, dryRun, "label a ticket", rec)

		if want := "2 ticket(s) deferred by overlaps (#1, #2)"; sel.Held != want {
			t.Errorf("dry-run %v: Held = %q, want %q", dryRun, sel.Held, want)
		}
		if !rec.saw("queue held: 2 ticket(s) deferred by overlaps (#1, #2)") || rec.saw("queue empty") {
			t.Errorf("dry-run %v: want a held queue, not an empty one; events = %v", dryRun, rec.events)
		}
		var streamed []string
		for _, r := range rec.records {
			if r.Kind == loopstream.KindItemSkipped {
				streamed = append(streamed, r.Ticket)
			}
		}
		if !reflect.DeepEqual(streamed, []string{"#1", "#2"}) {
			t.Errorf("dry-run %v: deferrals streamed for %v, want #1 and #2", dryRun, streamed)
		}
	}
}

// A deferral that stands poll after poll reaches the stream once, while the
// console still shows it each poll; a new reason streams again.
func TestDeferOverlapsStreamsADeferralOnce(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{{Identifier: "#1", Description: "Tidy internal/loop/loop.go."}}}
	f := &inFlight{work: []overlap.Work{openPR}}
	rec := &recorder{}
	r := screened(q, f, overlap.Policy{Threshold: 1}, rec)
	streamed := func() int {
		n := 0
		for _, r := range rec.records {
			if r.Kind == loopstream.KindItemSkipped {
				n++
			}
		}
		return n
	}

	ResolveNext(r, &mutexLock{}, false, "", rec)
	ResolveNext(r, &mutexLock{}, false, "", rec)
	if n := streamed(); n != 1 {
		t.Errorf("streamed %d deferrals over two polls, want 1", n)
	}
	if n := countSaw(rec, "#1 deferred: overlaps PR #10"); n != 2 {
		t.Errorf("narrated the deferral %d times, want once per poll", n)
	}
	f.work = []overlap.Work{{Label: "PR #11", Branch: "feat/gh-8", Files: []string{"internal/loop/loop.go"}}}
	ResolveNext(r, &mutexLock{}, false, "", rec)
	if n := streamed(); n != 2 {
		t.Errorf("streamed %d deferrals after the reason changed, want 2", n)
	}
}

func countSaw(r *recorder, event string) int {
	n := 0
	for _, e := range r.events {
		if strings.Contains(e, event) {
			n++
		}
	}
	return n
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
