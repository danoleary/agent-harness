package pipeline

import (
	"errors"
	"testing"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/ticket"
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

// A real run selects, then claims the ticket (claim-on-select dequeue, ADR-0003)
// and signals the caller to proceed with PreClaimed set.
func TestResolveNextSelectsAndClaims(t *testing.T) {
	r := &fakeResolver{ticket: ticket.Ticket{Identifier: "BEH-100", Priority: "Urgent"}, ok: true}
	rec := &recorder{}

	sel := ResolveNext(r, false, rec)

	if !sel.Proceed {
		t.Fatal("a selected ticket on a real run should Proceed")
	}
	if sel.Identifier != "BEH-100" {
		t.Errorf("Identifier = %q, want BEH-100", sel.Identifier)
	}
	if !sel.PreClaimed {
		t.Error("a claimed selection must set PreClaimed so the stage skips its own claim")
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

	sel := ResolveNext(r, false, rec)

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

	sel := ResolveNext(r, true, rec)

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
	if sel.PreClaimed {
		t.Error("dry-run claimed nothing, so PreClaimed must be false")
	}
	if !rec.saw("not claiming") {
		t.Errorf("expected a 'not claiming' dry-run narration; events = %v", rec.events)
	}
}

// A selection read failure is a hard error: exit 1, no claim.
func TestResolveNextSelectionErrorExitsOne(t *testing.T) {
	r := &fakeResolver{selErr: errors.New("linear down")}
	rec := &recorder{}

	sel := ResolveNext(r, false, rec)

	if sel.Proceed {
		t.Error("a selection error must not Proceed")
	}
	if sel.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", sel.ExitCode)
	}
	if len(r.moved) != 0 {
		t.Errorf("a selection error must claim nothing, claimed %v", r.moved)
	}
}

// A claim failure after a successful select is a hard error: exit 1, do not
// Proceed (the ticket could not be dequeued).
func TestResolveNextClaimErrorExitsOne(t *testing.T) {
	r := &fakeResolver{ticket: ticket.Ticket{Identifier: "BEH-102"}, ok: true, moveErr: errors.New("linear down")}
	rec := &recorder{}

	sel := ResolveNext(r, false, rec)

	if sel.Proceed {
		t.Error("a claim failure must not Proceed")
	}
	if sel.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", sel.ExitCode)
	}
}
