package lease

import (
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"

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

// board is a tracker's In Progress column shared by every instance: a claim
// moves a ready ticket there, and StillReady reads it. The read sleeps so two
// claims that did not exclude each other would both see the ticket ready.
type board struct {
	mu      sync.Mutex
	claimed map[string]int
	readErr error
}

func newBoard() *board { return &board{claimed: map[string]int{}} }

func (b *board) StillReady(key string) (bool, error) {
	time.Sleep(5 * time.Millisecond)
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.claimed[key] == 0, b.readErr
}

func (b *board) MoveToInProgress(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.claimed[key]++
	return nil
}

func (b *board) ReleaseToTodo(key string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.claimed[key] = 0
	return nil
}

func (b *board) MoveToCanceled(string) error { return nil }

// mutexLock is the host's claim lock, in memory.
type mutexLock struct{ sync.Mutex }

func (m *mutexLock) Lock() (func(), error) {
	m.Mutex.Lock()
	return m.Mutex.Unlock, nil
}

// Two instances that selected the same ticket at the same moment both try to
// claim it (#89). Exactly one wins and holds it; the other holds nothing and
// issued no claim of its own.
func TestClaimGivesASelectedTicketToExactlyOneInstance(t *testing.T) {
	b := newBoard()
	lock := &mutexLock{}
	var wg sync.WaitGroup
	var winners atomic.Int32
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			l, won, err := Claim(b, lock, "PROJ-1")
			if err != nil {
				t.Errorf("Claim() = %v", err)
				return
			}
			if won != (l != nil) {
				t.Errorf("won = %v but lease = %v: a lease is handed back exactly when the claim is won", won, l)
			}
			if won {
				winners.Add(1)
				if !l.Held() {
					t.Error("the winner's lease must be held")
				}
			}
		}()
	}
	wg.Wait()
	if winners.Load() != 1 {
		t.Errorf("%d instances won PROJ-1, want exactly 1", winners.Load())
	}
	if b.claimed["PROJ-1"] != 1 {
		t.Errorf("PROJ-1 was moved In Progress %d times, want once: the loser must not claim", b.claimed["PROJ-1"])
	}
}

// A ticket another run already holds is lost, not an error: nothing is claimed,
// so nothing will be released either.
func TestClaimOfATicketAnotherRunHoldsIsLost(t *testing.T) {
	b := newBoard()
	b.claimed["PROJ-1"] = 1

	l, won, err := Claim(b, &mutexLock{}, "PROJ-1")

	if err != nil || won || l != nil {
		t.Errorf("Claim() = %v, %v, %v; want nil, false, nil", l, won, err)
	}
	if b.claimed["PROJ-1"] != 1 {
		t.Errorf("a lost claim moved the ticket again (%d moves)", b.claimed["PROJ-1"])
	}
}

// A failed confirmation read is an error, and claims nothing: guessing either
// way would risk a ticket worked twice or one never worked.
func TestClaimFailsClosedOnAFailedRead(t *testing.T) {
	b := newBoard()
	b.readErr = errors.New("tracker down")

	l, won, err := Claim(b, &mutexLock{}, "PROJ-1")

	if err == nil || won || l != nil {
		t.Errorf("Claim() = %v, %v, %v; want an error and no lease", l, won, err)
	}
	if b.claimed["PROJ-1"] != 0 {
		t.Error("a failed read must claim nothing")
	}
}
