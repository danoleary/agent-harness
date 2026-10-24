package loop

import (
	"time"

	"github.com/danoleary/agent-harness/internal/lease"
	"github.com/danoleary/agent-harness/internal/stages"
)

// fakeHost, fakeSource and fakePipeline are the scripted [Host], [Source] and
// [Pipeline] the daemon tests drive: one field per method, each falling back to
// the quiet, healthy default when a test leaves it unset — an empty queue, a
// source that accepts every mutation, a no-PR run with nothing to recover, a disk
// with nothing to reclaim — so a test wires only the signal it is actually about.
//
// They are what the sixteen injected thunks became: the tests still script
// per-call behaviour, but the daemon depends on three seams with two adapters
// apiece (these and loophost's) instead of sixteen with one.
type fakeHost struct {
	clearStop      func() error
	checkAccess    func() error
	stopRequested  func() bool
	fetchMain      func() error
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

func (h *fakeHost) CheckAccess() error {
	if h.checkAccess == nil {
		return nil
	}
	return h.checkAccess()
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

// fakeSource is a scripted queue of keyed items. next yields the next key (ok=false
// for an empty queue); the item's Branch is derived from it the way the ticket
// Source slugs one, so a test can see the item reach the Pipeline whole.
type fakeSource struct {
	nextErr      error
	next         func() (string, bool)
	release      func(string) error
	close        func(string) error
	comment      func(string, string) error
	claims       func() ([]StaleClaim, error)
	remoteBranch func(string) bool

	// leased is the lease Next last handed the loop, so a test can act as a
	// Pipeline that settles it mid-run.
	leased *lease.Lease
}

// scriptedQueue is the store behind the fake's leases: a claim always succeeds
// (selection already claimed), and a release or close lands in the same release /
// close scripts the reaper's release uses, so a test asserts every mutation on the
// source in one place.
type scriptedQueue struct{ s *fakeSource }

func (scriptedQueue) MoveToInProgress(string) error { return nil }
func (q scriptedQueue) ReleaseToTodo(key string) error {
	return q.s.Release(key)
}
func (q scriptedQueue) MoveToCanceled(key string) error {
	if q.s.close == nil {
		return nil
	}
	return q.s.close(key)
}

var _ Source = (*fakeSource)(nil)

func (s *fakeSource) Next() (WorkItem, *lease.Lease, bool, error) {
	if s.nextErr != nil {
		return WorkItem{}, nil, false, s.nextErr
	}
	if s.next == nil {
		return WorkItem{}, nil, false, nil // an empty queue: the daemon idles and re-polls
	}
	key, ok := s.next()
	if !ok {
		return WorkItem{}, nil, false, nil
	}
	s.leased = lease.Held(scriptedQueue{s}, key)
	return WorkItem{Key: key, Branch: "feat/" + key}, s.leased, true, nil
}

func (s *fakeSource) Release(key string) error {
	if s.release == nil {
		return nil
	}
	return s.release(key)
}

func (s *fakeSource) Comment(key, body string) error {
	if s.comment == nil {
		return nil
	}
	return s.comment(key, body)
}

func (s *fakeSource) Claims() ([]StaleClaim, error) {
	if s.claims == nil {
		return nil, nil
	}
	return s.claims()
}

func (s *fakeSource) HasRemoteBranch(key string) bool {
	if s.remoteBranch == nil {
		return false
	}
	return s.remoteBranch(key)
}

// fakePipeline scripts one run per item, keyed by the item's Key.
type fakePipeline struct {
	run     func(string) stages.Result
	recover func(string) (stages.Result, bool)
}

var _ Pipeline = (*fakePipeline)(nil)

func (p *fakePipeline) Run(item WorkItem, _ *lease.Lease) stages.Result {
	if p.run == nil {
		return stages.Result{}
	}
	return p.run(item.Key)
}

func (p *fakePipeline) Recover(item WorkItem) (stages.Result, bool) {
	if p.recover == nil {
		return stages.Result{}, false // nothing to recover: release as usual
	}
	return p.recover(item.Key)
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
