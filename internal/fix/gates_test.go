package fix

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/hostlock"
	"github.com/danoleary/agent-harness/internal/session"
)

// meetDeadline bounds how long a gate in gatingRuns waits, from its own entry, for
// the others: long enough that a loaded machine still meets, short enough that a
// limiter which serialises gates fails the test instead of hanging it. The clock
// is the gate's own, not the test's, so neither building the git repo nor another
// run's git work before its gate can spend it (#225).
const meetDeadline = 10 * time.Second

// gatingRuns runs `runs` gate sets at once, one per loop instance, each with its
// own Host and run log over one shared lock dir, and reports the most gates that
// were running at any moment. With meet > 0 each gate holds until meet gates are
// inside at once (or its meetDeadline passes), so overlap is a barrier rather than a
// race between sleeps (#131); with meet 0 each gate just holds briefly.
func gatingRuns(t *testing.T, cfg config.Config, runs, meet int) int32 {
	t.Helper()
	var inside, worst atomic.Int32
	var wg sync.WaitGroup
	met := make(chan struct{})
	var meetOnce sync.Once
	// One handoff carrying a code change, so the docs-only skip never fires; every
	// run gates the same worktree, each through its own Host.
	repo := gittest.New(t)
	repo.Handoff("proj-1")
	for i := 0; i < runs; i++ {
		h := hostio.NewFake(repo.Checkout(), "proj-1")
		h.ShellFn = func(int, hostio.ShellRun) session.Outcome {
			n := inside.Add(1)
			for {
				w := worst.Load()
				if n <= w || worst.CompareAndSwap(w, n) {
					break
				}
			}
			if meet <= 0 {
				time.Sleep(20 * time.Millisecond)
			} else {
				if n >= int32(meet) {
					meetOnce.Do(func() { close(met) })
				}
				select {
				case <-met:
				case <-time.After(meetDeadline):
				}
			}
			inside.Add(-1)
			return session.Outcome{}
		}
		log := testLog(t)
		e := Env{Host: h, Cfg: cfg, Log: log, Who: "review"}
		wg.Add(1)
		go func() {
			defer wg.Done()
			if res := e.Gates("gate"); !res.Green() {
				t.Errorf("gates = %+v, want green", res)
			}
		}()
	}
	wg.Wait()
	return worst.Load()
}

// With the gate limit at 1, two instances never run gates at the same time
// (#89): the second waits for the first's gate step, and only for that.
func TestGateLimitOfOneNeverRunsTwoGateSetsAtOnce(t *testing.T) {
	cfg := config.Config{}
	cfg.Gates = []config.Gate{{Name: "build", Command: "make"}, {Name: "test", Command: "make test"}}
	cfg.GateConcurrency = 1
	cfg.GateLockDir = t.TempDir()

	if worst := gatingRuns(t, cfg, 3, 0); worst != 1 {
		t.Errorf("%d gates ran at once under a limit of 1", worst)
	}
}

// The limit is a count, not a mutex: at 2, two runs gate side by side. Each gate
// holds until the other has entered, so the test does not depend on the two
// windows happening to overlap on a loaded machine (#131).
func TestGateLimitOfTwoLetsTwoRunsGateTogether(t *testing.T) {
	cfg := config.Config{}
	cfg.Gates = []config.Gate{{Name: "build", Command: "make"}}
	cfg.GateConcurrency = 2
	cfg.GateLockDir = t.TempDir()

	if worst := gatingRuns(t, cfg, 2, 2); worst != 2 {
		t.Errorf("%d gates ran at once under a limit of 2, want 2", worst)
	}
}

// A run that waits for a gate slot says so, so a quiet run reads as queued, not
// hung. The test holds the only slot itself until the run has narrated its wait,
// so the wait is certain rather than a race between two runs' gate windows,
// which a loaded machine could run back to back.
func TestAGateRunThatWaitsForASlotNarratesIt(t *testing.T) {
	cfg := config.Config{}
	cfg.Gates = []config.Gate{{Name: "build", Command: "make"}}
	cfg.GateConcurrency = 1
	cfg.GateLockDir = t.TempDir()

	held, err := hostlock.Slots{Dir: cfg.GateSlotsDir(), Limit: 1}.Acquire(nil)
	if err != nil {
		t.Fatal(err)
	}
	released := false
	release := func() {
		if !released {
			released = true
			held()
		}
	}
	defer release()

	repo := gittest.New(t)
	repo.Handoff("proj-1")
	h := hostio.NewFake(repo.Checkout(), "proj-1")
	h.ShellFn = func(int, hostio.ShellRun) session.Outcome { return session.Outcome{} }
	log := testLog(t)
	e := Env{Host: h, Cfg: cfg, Log: log, Who: "review"}
	done := make(chan GateResult, 1)
	go func() { done <- e.Gates("gate") }()

	runLog := filepath.Join(log.Dir, "run.jsonl")
	giveUp := time.Now().Add(meetDeadline)
	for {
		raw, _ := os.ReadFile(runLog)
		if strings.Contains(string(raw), "waiting for a gate slot") {
			break
		}
		if time.Now().After(giveUp) {
			t.Fatalf("a run queued behind a held gate slot never narrated its wait:\n%s", raw)
		}
		time.Sleep(10 * time.Millisecond)
	}
	release()
	if res := <-done; !res.Green() {
		t.Errorf("gates = %+v, want green once the slot is free", res)
	}
}
