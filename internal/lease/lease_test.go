package lease

import (
	"errors"
	"testing"

	"github.com/danoleary/agent-harness/internal/hostio"
)

// The bug that motivated the lease (#25): a stage released the ticket to Todo,
// the pipeline retried, and the retry skipped the claim because the run had been
// "pre-claimed" at selection. A released lease must claim again on the next Hold.
func TestHoldAfterReleaseClaimsTheTicketAgain(t *testing.T) {
	q := hostio.NewFakeTracker()
	l := Held(q, "PROJ-1")

	if err := l.Release(); err != nil {
		t.Fatalf("Release() = %v", err)
	}
	if err := l.Hold(); err != nil {
		t.Fatalf("Hold() = %v", err)
	}

	if got := q.Released; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("released = %v, want [PROJ-1]", got)
	}
	if got := q.Claimed; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("claimed = %v, want the retry to re-claim [PROJ-1]", got)
	}
	if !l.Held() {
		t.Error("Held() = false after a successful Hold")
	}
}

// Implementation releases a ticket it cannot work, and the loop then settles the
// same no-PR run by releasing again. Only the first may reach the tracker; a
// hand-passed ticket that was never claimed is never released either (BEH-316).
func TestReleaseOfAnUnheldTicketTouchesNothing(t *testing.T) {
	q := hostio.NewFakeTracker()
	l := Held(q, "PROJ-1")

	_ = l.Release()
	if err := l.Release(); err != nil {
		t.Fatalf("second Release() = %v", err)
	}
	_ = Unheld(q, "PROJ-2").Release()

	if got := q.Released; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("released = %v, want exactly one release of PROJ-1", got)
	}
}

// A recommend-close run moves the ticket to the terminal canceled state (BEH-682).
// A closed ticket must never be released back to Todo afterwards, or the dispatch
// guard re-grabs it and re-runs it to the same "nothing to ship" forever.
func TestCloseCancelsAndEndsTheHold(t *testing.T) {
	q := hostio.NewFakeTracker()
	l := Held(q, "PROJ-1")

	if err := l.Close(); err != nil {
		t.Fatalf("Close() = %v", err)
	}
	_ = l.Release()

	if got := q.Canceled; len(got) != 1 || got[0] != "PROJ-1" {
		t.Errorf("canceled = %v, want [PROJ-1]", got)
	}
	if len(q.Released) != 0 {
		t.Errorf("released = %v, want a closed ticket never returned to Todo", q.Released)
	}
}

// A tracker hiccup on release leaves the ticket In Progress, so the lease must
// still hold it: the loop's own settle then gets a second chance to release it.
func TestAFailedReleaseKeepsTheHold(t *testing.T) {
	q := hostio.NewFakeTracker()
	q.ReleErr = errors.New("tracker unreachable")
	l := Held(q, "PROJ-1")

	if err := l.Release(); err == nil {
		t.Fatal("Release() = nil, want the tracker error surfaced")
	}
	q.ReleErr = nil
	_ = l.Release()

	if got := q.Released; len(got) != 2 {
		t.Errorf("release attempts = %v, want the retry to reach the tracker", got)
	}
	if l.Held() {
		t.Error("Held() = true after the retried release succeeded")
	}
}
