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
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
)

// gatingRuns runs `runs` gate sets at once, one per loop instance, each with its
// own Host and run log over one shared lock dir, and reports the most gates that
// were running at any moment.
func gatingRuns(t *testing.T, cfg config.Config, runs int) int32 {
	t.Helper()
	worst, _ := gatingRunsLogged(t, cfg, runs)
	return worst
}

// gatingRunsLogged is gatingRuns that also returns each run's logs root.
func gatingRunsLogged(t *testing.T, cfg config.Config, runs int) (int32, []string) {
	t.Helper()
	var roots []string
	var inside, worst atomic.Int32
	var wg sync.WaitGroup
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
			time.Sleep(20 * time.Millisecond)
			inside.Add(-1)
			return session.Outcome{}
		}
		root := t.TempDir()
		roots = append(roots, root)
		log, err := runlog.New(root, "PROJ-1")
		if err != nil {
			t.Fatal(err)
		}
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
	return worst.Load(), roots
}

// With the gate limit at 1, two instances never run gates at the same time
// (#89): the second waits for the first's gate step, and only for that.
func TestGateLimitOfOneNeverRunsTwoGateSetsAtOnce(t *testing.T) {
	cfg := config.Config{}
	cfg.Gates = []config.Gate{{Name: "build", Command: "make"}, {Name: "test", Command: "make test"}}
	cfg.GateConcurrency = 1
	cfg.GateLockDir = t.TempDir()

	if worst := gatingRuns(t, cfg, 3); worst != 1 {
		t.Errorf("%d gates ran at once under a limit of 1", worst)
	}
}

// The limit is a count, not a mutex: at 2, two runs gate side by side.
func TestGateLimitOfTwoLetsTwoRunsGateTogether(t *testing.T) {
	cfg := config.Config{}
	cfg.Gates = []config.Gate{{Name: "build", Command: "make"}}
	cfg.GateConcurrency = 2
	cfg.GateLockDir = t.TempDir()

	if worst := gatingRuns(t, cfg, 2); worst != 2 {
		t.Errorf("%d gates ran at once under a limit of 2, want 2", worst)
	}
}

// A run that waits for a gate slot says so, so a quiet run reads as queued, not
// hung.
func TestAGateRunThatWaitsForASlotNarratesIt(t *testing.T) {
	cfg := config.Config{}
	cfg.Gates = []config.Gate{{Name: "build", Command: "make"}}
	cfg.GateConcurrency = 1
	cfg.GateLockDir = t.TempDir()

	_, roots := gatingRunsLogged(t, cfg, 2)

	var all strings.Builder
	for _, root := range roots {
		raw, _ := os.ReadFile(filepath.Join(root, "PROJ-1", "run.jsonl"))
		all.Write(raw)
	}
	if !strings.Contains(all.String(), "waiting for a gate slot") {
		t.Errorf("two runs over one gate slot, and neither narrated its wait:\n%s", all.String())
	}
}
