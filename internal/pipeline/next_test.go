package pipeline

import (
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// fakeResolver fakes the Linear surface ResolveNext composes: a pure read
// (SelectNextTicket) plus the claim mutation, recording the claim it issues.
type fakeResolver struct {
	ticket  ticket.Ticket
	ok      bool
	selErr  error
	moveErr error
	moved   []string
}

func (f *fakeResolver) SelectNextTicket() (ticket.Ticket, bool, error) {
	return f.ticket, f.ok, f.selErr
}

func (f *fakeResolver) MoveToInProgress(id string) error {
	f.moved = append(f.moved, id)
	return f.moveErr
}

// StillReady reports a ticket this resolver already claimed as taken.
func (f *fakeResolver) StillReady(id string) (bool, error) {
	for _, m := range f.moved {
		if m == id {
			return false, nil
		}
	}
	return true, nil
}

func (f *fakeResolver) ReleaseToTodo(string) error  { return nil }
func (f *fakeResolver) MoveToCanceled(string) error { return nil }

// A real run selects, then claims the ticket (claim-on-select dequeue, ADR-0003)
// and signals the caller to proceed with the held lease.
func TestResolveNextSelectsAndClaims(t *testing.T) {
	r := &fakeResolver{ticket: ticket.Ticket{Identifier: "BEH-100", Priority: "Urgent"}, ok: true}
	rec := &recorder{}

	sel := ResolveNext(r, &mutexLock{}, false, "", rec)

	if !sel.Proceed {
		t.Fatal("a selected ticket on a real run should Proceed")
	}
	if sel.Identifier != "BEH-100" {
		t.Errorf("Identifier = %q, want BEH-100", sel.Identifier)
	}
	if sel.Lease == nil || !sel.Lease.Held() {
		t.Error("a claimed selection must hand back a held lease so the stage skips its own claim")
	}
	if len(r.moved) != 1 || r.moved[0] != "BEH-100" {
		t.Errorf("claim mutations = %v, want exactly [BEH-100]", r.moved)
	}
	// The selection is tagged ticket-selected for the viewer's global stream, and
	// carries the ticket so the viewer can surface "current ticket" (ADR-0005).
	if len(rec.records) != 1 {
		t.Fatalf("expected exactly one structured record, got %d", len(rec.records))
	}
	if got := rec.records[0]; got.Kind != loopstream.KindTicketSelected || got.Ticket != "BEH-100" {
		t.Errorf("structured record = %+v, want kind=ticket-selected ticket=BEH-100", got)
	}
}

// An empty queue is a normal steady state: exit 0, narrated, no claim.
func TestResolveNextEmptyQueueExitsZeroWithoutClaiming(t *testing.T) {
	r := &fakeResolver{ok: false}
	rec := &recorder{}

	sel := ResolveNext(r, &mutexLock{}, false, "", rec)

	if sel.Proceed {
		t.Error("an empty queue must not Proceed")
	}
	if sel.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0 (empty queue is not a failure)", sel.ExitCode)
	}
	if len(r.moved) != 0 {
		t.Errorf("an empty queue must claim nothing, claimed %v", r.moved)
	}
	if !rec.saw("queue empty") {
		t.Errorf("expected a 'queue empty' narration; events = %v", rec.events)
	}
}

// Dry-run resolves the real ticket but never claims — the guarantee tightens to
// "mutates no Linear". It does not Proceed (the caller prints the plan and exits).
func TestResolveNextDryRunResolvesButDoesNotClaim(t *testing.T) {
	r := &fakeResolver{ticket: ticket.Ticket{Identifier: "BEH-101"}, ok: true}
	rec := &recorder{}

	sel := ResolveNext(r, &mutexLock{}, true, "", rec)

	if sel.Proceed {
		t.Error("dry-run must not Proceed to running the pipeline")
	}
	if sel.Identifier != "BEH-101" {
		t.Errorf("Identifier = %q, want BEH-101 (dry-run still resolves the real ticket for the plan)", sel.Identifier)
	}
	if sel.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", sel.ExitCode)
	}
	if len(r.moved) != 0 {
		t.Errorf("dry-run must claim nothing, claimed %v", r.moved)
	}
	if sel.Lease != nil {
		t.Error("dry-run claimed nothing, so there is no lease to hand back")
	}
	if !rec.saw("not claiming") {
		t.Errorf("expected a 'not claiming' dry-run narration; events = %v", rec.events)
	}
}

// A selection read failure is a hard error: exit 1, no claim.
func TestResolveNextSelectionErrorExitsOne(t *testing.T) {
	r := &fakeResolver{selErr: errors.New("linear down")}
	rec := &recorder{}

	sel := ResolveNext(r, &mutexLock{}, false, "", rec)

	if sel.Proceed {
		t.Error("a selection error must not Proceed")
	}
	if sel.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", sel.ExitCode)
	}
	if len(r.moved) != 0 {
		t.Errorf("a selection error must claim nothing, claimed %v", r.moved)
	}
	if sel.Err == nil {
		t.Error("a selection error must be handed back in Err, so the daemon can tell it from an empty queue")
	}
}

// The empty-queue narration carries the hint, so an operator staring at an idle
// daemon is told how to give it work.
func TestResolveNextEmptyQueueNarratesTheHint(t *testing.T) {
	rec := &recorder{}
	ResolveNext(&fakeResolver{}, &mutexLock{}, false, "label a ticket `ready-for-agent` to queue it", rec)
	if !rec.saw("queue empty; label a ticket `ready-for-agent` to queue it") {
		t.Errorf("expected the hint in the empty-queue narration; events = %v", rec.events)
	}
}

// A claim failure after a successful select is a hard error: exit 1, do not
// Proceed (the ticket could not be dequeued).
func TestResolveNextClaimErrorExitsOne(t *testing.T) {
	r := &fakeResolver{ticket: ticket.Ticket{Identifier: "BEH-102"}, ok: true, moveErr: errors.New("linear down")}
	rec := &recorder{}

	sel := ResolveNext(r, &mutexLock{}, false, "", rec)

	if sel.Proceed {
		t.Error("a claim failure must not Proceed")
	}
	if sel.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", sel.ExitCode)
	}
}

// mutexLock is the host's claim lock, in memory.
type mutexLock struct{ sync.Mutex }

func (m *mutexLock) Lock() (func(), error) {
	m.Mutex.Lock()
	return m.Mutex.Unlock, nil
}

// sharedQueue is one tracker queue several instances select from at once. Its
// selection read lags every claim by `lag` (as a search index does), and the
// first `staleReads` selections ignore claims altogether, so an instance can
// select a ticket another has just claimed; the ticket's own state, which
// StillReady reads, never lags.
type sharedQueue struct {
	mu         sync.Mutex
	keys       []string
	claimed    map[string]time.Time
	lag        time.Duration
	staleReads int
	moves      map[string]int
}

func newSharedQueue(lag time.Duration, keys ...string) *sharedQueue {
	return &sharedQueue{keys: keys, claimed: map[string]time.Time{}, moves: map[string]int{}, lag: lag}
}

func (q *sharedQueue) SelectNextTicket() (ticket.Ticket, bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	stale := q.staleReads > 0
	q.staleReads--
	for _, k := range q.keys {
		at, ok := q.claimed[k]
		if !ok || stale || time.Since(at) < q.lag {
			return ticket.Ticket{Identifier: k}, true, nil
		}
	}
	return ticket.Ticket{}, false, nil
}

func (q *sharedQueue) StillReady(key tracker.Key) (bool, error) {
	q.mu.Lock()
	defer q.mu.Unlock()
	_, taken := q.claimed[key]
	return !taken, nil
}

func (q *sharedQueue) MoveToInProgress(key tracker.Key) error {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.claimed[key] = time.Now()
	q.moves[key]++
	return nil
}

func (q *sharedQueue) ReleaseToTodo(tracker.Key) error  { return nil }
func (q *sharedQueue) MoveToCanceled(tracker.Key) error { return nil }

// An instance whose selected ticket another instance has just claimed loses the
// claim, claims nothing, and selects again: it gets the next ticket (#89).
func TestResolveNextSelectsAgainAfterALostClaim(t *testing.T) {
	q := newSharedQueue(0, "BEH-1", "BEH-2")
	q.claimed["BEH-1"] = time.Now() // another instance's claim, which the first read misses
	q.staleReads = 1
	rec := &recorder{}

	sel := ResolveNext(q, &mutexLock{}, false, "", rec)

	if !sel.Proceed || sel.Identifier != "BEH-2" {
		t.Fatalf("selection = %+v, want to proceed on BEH-2 after losing BEH-1", sel)
	}
	if q.moves["BEH-1"] != 0 {
		t.Error("a lost claim must not move the ticket another instance holds")
	}
	if !rec.saw("BEH-1") {
		t.Errorf("expected the lost claim on BEH-1 to be narrated; events = %v", rec.events)
	}
}

// A queue that keeps offering only tickets other instances hold is nothing to do
// this poll: a clean empty result, not a failure and not a claim.
func TestResolveNextWithOnlyLostClaimsIsAnEmptyPoll(t *testing.T) {
	q := newSharedQueue(time.Hour, "BEH-1")
	q.claimed["BEH-1"] = time.Now()
	rec := &recorder{}

	sel := ResolveNext(q, &mutexLock{}, false, "", rec)

	if sel.Proceed || sel.ExitCode != 0 || sel.Err != nil {
		t.Errorf("selection = %+v, want a clean empty poll", sel)
	}
	if q.moves["BEH-1"] != 0 {
		t.Error("nothing may be claimed")
	}
}

// Several instances select at the same moment from one lagging queue. No two
// proceed on the same ticket, so no two work the same branch, and every ticket
// is claimed once (#89).
func TestConcurrentInstancesNeverProceedOnOneTicketOrBranch(t *testing.T) {
	const instances = 4
	q := newSharedQueue(50*time.Millisecond, "BEH-1", "BEH-2", "BEH-3", "BEH-4", "BEH-5")
	lock := &mutexLock{}
	got := make([]NextSelection, instances)
	var wg sync.WaitGroup
	for i := 0; i < instances; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got[i] = ResolveNext(q, lock, false, "", &recorder{})
		}(i)
	}
	wg.Wait()

	branches := map[string]int{}
	for i, sel := range got {
		if !sel.Proceed {
			continue
		}
		branch := fmt.Sprintf("feat/%s", ticket.Slug(sel.Identifier))
		if j, dup := branches[branch]; dup {
			t.Errorf("instances %d and %d both proceed on %s", j, i, branch)
		}
		branches[branch] = i
	}
	if len(branches) == 0 {
		t.Error("no instance proceeded on any ticket")
	}
	for key, n := range q.moves {
		if n != 1 {
			t.Errorf("%s was claimed %d times, want once", key, n)
		}
	}
}
