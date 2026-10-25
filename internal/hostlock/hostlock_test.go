package hostlock

import (
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// Two holders of one lock file never overlap, even inside one process: flock
// locks belong to the open file, not the process, so two daemons and two
// goroutines see the same exclusion.
func TestFileLockExcludesASecondHolder(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claim.lock")
	var inside, worst atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			unlock, err := File(path).Lock()
			if err != nil {
				t.Errorf("Lock() = %v", err)
				return
			}
			n := inside.Add(1)
			for {
				w := worst.Load()
				if n <= w || worst.CompareAndSwap(w, n) {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
			inside.Add(-1)
			unlock()
		}()
	}
	wg.Wait()
	if worst.Load() != 1 {
		t.Errorf("at most %d holders at once, want 1", worst.Load())
	}
}

// The lock file's directory may not exist yet on a checkout that has never run
// a loop; taking the lock creates it.
func TestFileLockCreatesItsDirectory(t *testing.T) {
	path := filepath.Join(t.TempDir(), "new", "dir", "claim.lock")
	unlock, err := File(path).Lock()
	if err != nil {
		t.Fatalf("Lock() = %v", err)
	}
	unlock()
}

// With a limit of n, at most n runs hold a gate slot at once; the rest wait and
// are told so once each.
func TestSlotsBoundHowManyHoldAtOnce(t *testing.T) {
	for _, limit := range []int{1, 2} {
		s := Slots{Dir: t.TempDir(), Limit: limit, Poll: time.Millisecond}
		var inside, worst, waits atomic.Int32
		var wg sync.WaitGroup
		for i := 0; i < 5; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				release, err := s.Acquire(func() { waits.Add(1) })
				if err != nil {
					t.Errorf("Acquire() = %v", err)
					return
				}
				n := inside.Add(1)
				for {
					w := worst.Load()
					if n <= w || worst.CompareAndSwap(w, n) {
						break
					}
				}
				time.Sleep(15 * time.Millisecond)
				inside.Add(-1)
				release()
			}()
		}
		wg.Wait()
		if worst.Load() != int32(limit) {
			t.Errorf("limit %d: at most %d holders at once, want %d", limit, worst.Load(), limit)
		}
		if waits.Load() == 0 {
			t.Errorf("limit %d: five runs over %d slots, and no run was told it waited", limit, limit)
		}
	}
}

// A non-positive limit means no limit: nothing waits and nothing is locked.
func TestSlotsWithNoLimitNeverWait(t *testing.T) {
	s := Slots{Dir: filepath.Join(t.TempDir(), "never-created"), Limit: 0}
	release, err := s.Acquire(func() { t.Error("an unlimited slot must never wait") })
	if err != nil {
		t.Fatalf("Acquire() = %v", err)
	}
	release()
}

// A released slot is free for the next run.
func TestSlotsReleaseFreesTheSlot(t *testing.T) {
	s := Slots{Dir: t.TempDir(), Limit: 1, Poll: time.Millisecond}
	release, err := s.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	release()
	done := make(chan struct{})
	go func() {
		r, err := s.Acquire(func() { t.Error("the slot was released; the next run must not wait") })
		if err == nil {
			r()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Acquire after a release did not return")
	}
}
