package loop

import (
	"time"

	"github.com/danoleary/agent-harness/internal/stages"
)

// fakeHost is the scripted [Host] the daemon tests drive: one field per method,
// each falling back to the quiet, healthy default when a test leaves it unset —
// an empty queue, a tracker that accepts every mutation, a disk with nothing to
// reclaim — so a test wires only the signal it is actually about.
//
// It is what the sixteen injected thunks became: the tests still script per-call
// behaviour, but the daemon now depends on one seam with two adapters (this and
// loophost.Real) instead of sixteen with one apiece.
type fakeHost struct {
	clearStop      func() error
	stopRequested  func() bool
	fetchMain      func() error
	resolveNext    func() (string, bool)
	release        func(string) error
	closeTicket    func(string) error
	comment        func(string, string) error
	claims         func() ([]StaleClaim, error)
	remoteBranch   func(string) bool
	runPipeline    func(string) stages.Result
	recoverFix     func(string) (stages.Result, bool)
	freeDisk       func() (uint64, error)
	pruneWorktrees func() (int, error)
	cachePrune     func() error
	dockerPrune    func() error
}

var _ Host = (*fakeHost)(nil)

func (h *fakeHost) ClearStopFile() error {
	if h.clearStop == nil {
		return nil
	}
	return h.clearStop()
}

func (h *fakeHost) StopRequested() bool {
	if h.stopRequested == nil {
		return false
	}
	return h.stopRequested()
}

func (h *fakeHost) FetchMain() error {
	if h.fetchMain == nil {
		return nil
	}
	return h.fetchMain()
}

func (h *fakeHost) ResolveNext() (string, bool) {
	if h.resolveNext == nil {
		return "", false // an empty queue: the daemon idles and re-polls
	}
	return h.resolveNext()
}

func (h *fakeHost) ReleaseTicket(id string) error {
	if h.release == nil {
		return nil
	}
	return h.release(id)
}

func (h *fakeHost) CloseTicket(id string) error {
	if h.closeTicket == nil {
		return nil
	}
	return h.closeTicket(id)
}

func (h *fakeHost) CommentTicket(id, body string) error {
	if h.comment == nil {
		return nil
	}
	return h.comment(id, body)
}

func (h *fakeHost) ListInProgressClaims() ([]StaleClaim, error) {
	if h.claims == nil {
		return nil, nil
	}
	return h.claims()
}

func (h *fakeHost) TicketHasRemoteBranch(id string) bool {
	if h.remoteBranch == nil {
		return false
	}
	return h.remoteBranch(id)
}

func (h *fakeHost) RunPipeline(id string) stages.Result {
	if h.runPipeline == nil {
		return stages.Result{}
	}
	return h.runPipeline(id)
}

func (h *fakeHost) RecoverCommittedFix(id string) (stages.Result, bool) {
	if h.recoverFix == nil {
		return stages.Result{}, false // nothing to recover: release as usual
	}
	return h.recoverFix(id)
}

func (h *fakeHost) FreeDisk() (uint64, error) {
	if h.freeDisk == nil {
		return 0, nil
	}
	return h.freeDisk()
}

func (h *fakeHost) PruneMergedWorktrees() (int, error) {
	if h.pruneWorktrees == nil {
		return 0, nil
	}
	return h.pruneWorktrees()
}

func (h *fakeHost) CachePrune() error {
	if h.cachePrune == nil {
		return nil
	}
	return h.cachePrune()
}

func (h *fakeHost) DockerPrune() error {
	if h.dockerPrune == nil {
		return nil
	}
	return h.dockerPrune()
}

// testClock is the tests' [Clock]: a now that a test can step and a sleep that
// records instead of waiting, so the daemon's ceilings, idle ticks and cap
// backoff are exercised in microseconds. An unset now is the zero instant (the
// MaxRuntime ceiling then never fires), an unset sleep returns immediately.
type testClock struct {
	now   func() time.Time
	sleep func(time.Duration)
}

var _ Clock = testClock{}

func (c testClock) Now() time.Time {
	if c.now == nil {
		return time.Time{}
	}
	return c.now()
}

func (c testClock) Sleep(d time.Duration) {
	if c.sleep != nil {
		c.sleep(d)
	}
}
