package pipeline

import (
	"errors"
	"testing"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// #366: after a host sleep the first run's ticket was back in the ready queue
// while its run was still live, and the next poll started a second run on its
// worktree. Selection now passes over a ticket whose run lock is held, says why,
// and claims the next ready ticket instead; once the run ends it dispatches.
func TestSkipLivePassesOverATicketWhoseRunIsLive(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{{Identifier: "BEH-1470"}, {Identifier: "BEH-1483"}}}
	live := map[string]bool{"BEH-1470": true}
	rec := &recorder{}
	r := SkipLive(q, func(key string) (bool, error) { return live[key], nil }, rec)

	sel := ResolveNext(r, &mutexLock{}, false, "", rec)
	if !sel.Proceed || sel.Identifier != "BEH-1483" {
		t.Fatalf("selection = %+v, want BEH-1470 passed over and BEH-1483 claimed", sel)
	}
	if !rec.saw("skipping BEH-1470: another live run holds its run lock (#366)") {
		t.Errorf("the skip must name the ticket and why; events = %v", rec.events)
	}
	if len(q.ready) != 1 || q.ready[0].Identifier != "BEH-1470" {
		t.Errorf("a passed-over ticket stays ready and unclaimed; ready = %v", q.ready)
	}

	if sel := ResolveNext(r, &mutexLock{}, false, "", rec); sel.Proceed || sel.Held != "1 ticket(s) whose run is live (BEH-1470)" {
		t.Errorf("selection = %+v, want the queue held by the live run", sel)
	}

	live["BEH-1470"] = false
	if sel := ResolveNext(r, &mutexLock{}, false, "", rec); !sel.Proceed || sel.Identifier != "BEH-1470" {
		t.Errorf("selection = %+v, want BEH-1470 dispatched once its run ended", sel)
	}
}

// A run lock that cannot be read is no proof the run is dead: selection passes
// over the ticket and says so, rather than risk a second run on its worktree.
func TestSkipLivePassesOverATicketWhoseRunLockCannotBeRead(t *testing.T) {
	q := &readyQueue{ready: []ticket.Ticket{{Identifier: "#366"}}}
	rec := &recorder{}
	r := SkipLive(q, func(string) (bool, error) { return false, errors.New("permission denied") }, rec)

	if sel := ResolveNext(r, &mutexLock{}, false, "", rec); sel.Proceed {
		t.Fatalf("selection = %+v, want nothing claimed", sel)
	}
	if !rec.saw("skipping #366: could not read whether its run is live (permission denied) (#366)") {
		t.Errorf("events = %v", rec.events)
	}
}
