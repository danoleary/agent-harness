package ci

import (
	"errors"
	"testing"
	"time"
)

// fakeClock advances only when the injected sleeper is called, so poll's
// interval/budget logic is exercised with zero real waiting (mirrors git_test).
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock             { return &fakeClock{t: time.Unix(0, 0)} }
func (c *fakeClock) now() time.Time        { return c.t }
func (c *fakeClock) sleep(d time.Duration) { c.t = c.t.Add(d) }

// scriptedFetch returns a fetch seam that yields the given verdict-bearing check
// sets in order, repeating the last one once exhausted, and counts its calls.
func scriptedFetch(buckets ...string) (func() ([]Check, error), *int) {
	calls := 0
	return func() ([]Check, error) {
		i := calls
		if i >= len(buckets) {
			i = len(buckets) - 1
		}
		calls++
		return []Check{{Name: "ci", Bucket: buckets[i]}}, nil
	}, &calls
}

func testPollCfg() pollConfig {
	return pollConfig{interval: 30 * time.Second, budget: 10 * time.Minute}
}

// scriptedChecks returns a fetch seam yielding the given check snapshots in
// order (repeating the last once exhausted) and counts its calls — for stall
// tests, where the snapshot's *shape* (not just one bucket) is what matters.
func scriptedChecks(snaps ...[]Check) (func() ([]Check, error), *int) {
	calls := 0
	return func() ([]Check, error) {
		i := calls
		if i >= len(snaps) {
			i = len(snaps) - 1
		}
		calls++
		return snaps[i], nil
	}, &calls
}

func TestPollReturnsImmediatelyWhenTerminalNoSleep(t *testing.T) {
	clock := newFakeClock()
	slept := 0
	sleep := func(d time.Duration) { slept++; clock.sleep(d) }
	fetch, calls := scriptedFetch(BucketPass)
	v, _, err := poll(fetch, testPollCfg(), sleep, clock.now)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v != Passed {
		t.Fatalf("verdict = %v, want Passed", v)
	}
	if slept != 0 {
		t.Fatalf("slept %d times, want 0 on immediate terminal", slept)
	}
	if *calls != 1 {
		t.Fatalf("fetched %d times, want 1", *calls)
	}
}

func TestPollWaitsThroughPendingThenReportsFailure(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	fetch, calls := scriptedFetch(BucketPending, BucketPending, BucketFail)
	v, checks, err := poll(fetch, testPollCfg(), sleep, clock.now)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v != Failed {
		t.Fatalf("verdict = %v, want Failed", v)
	}
	if *calls != 3 {
		t.Fatalf("fetched %d times, want 3", *calls)
	}
	if len(checks) == 0 {
		t.Fatal("expected the terminal checks to be returned")
	}
}

func TestPollTimesOutWhileStillPending(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	fetch, _ := scriptedFetch(BucketPending) // never settles
	v, _, err := poll(fetch, testPollCfg(), sleep, clock.now)
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v, want ErrPollTimeout", err)
	}
	if v != Pending {
		t.Fatalf("verdict = %v, want Pending on timeout", v)
	}
}

func TestPollWaitsThroughNoChecksYet(t *testing.T) {
	// Checks not registered yet (errNoChecksYet) twice, then they appear and fail.
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	calls := 0
	fetch := func() ([]Check, error) {
		calls++
		if calls <= 2 {
			return nil, errNoChecksYet
		}
		return []Check{{Name: "ci", Bucket: BucketFail}}, nil
	}
	v, _, err := poll(fetch, testPollCfg(), sleep, clock.now)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if v != Failed {
		t.Fatalf("verdict = %v, want Failed once checks registered", v)
	}
	if calls != 3 {
		t.Fatalf("fetched %d times, want 3 (waited through 2 no-checks-yet)", calls)
	}
}

func TestPollTimesOutIfChecksNeverRegister(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	fetch := func() ([]Check, error) { return nil, errNoChecksYet }
	_, _, err := poll(fetch, testPollCfg(), sleep, clock.now)
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v, want ErrPollTimeout", err)
	}
}

func TestPollStallsOnFrozenPendingBeforeBudget(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	// A merge-queue/main-only context wedged pending: the snapshot never changes.
	fetch, calls := scriptedChecks([]Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "deploy", Bucket: BucketPending}, // never moves on a PR
	})
	cfg := pollConfig{interval: 30 * time.Second, budget: 20 * time.Minute, stall: 2 * time.Minute}
	v, checks, err := poll(fetch, cfg, sleep, clock.now)
	if !errors.Is(err, ErrPollStalled) {
		t.Fatalf("err = %v, want ErrPollStalled", err)
	}
	if v != Pending {
		t.Fatalf("verdict = %v, want Pending on stall", v)
	}
	if len(checks) == 0 {
		t.Fatal("expected the wedged checks to be returned for the operator report")
	}
	// Fired at the 2m stall window (polls at t=0,30,60,90,120s), not the 20m budget.
	if *calls != 5 {
		t.Fatalf("fetched %d times, want 5 (bailed at the stall window, not the budget)", *calls)
	}
}

func TestPollDoesNotStallWhileProgressing(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	// The check set keeps changing each poll (jobs flipping as the run advances),
	// so even though each individual snapshot is pending, the stall window must keep
	// resetting and the run is allowed to reach its terminal pass.
	fetch, calls := scriptedChecks(
		[]Check{{Name: "lint", Bucket: BucketPending}},
		[]Check{{Name: "lint", Bucket: BucketPass}, {Name: "test", Bucket: BucketPending}},
		[]Check{{Name: "lint", Bucket: BucketPass}, {Name: "test", Bucket: BucketPass}},
	)
	cfg := pollConfig{interval: 30 * time.Second, budget: 20 * time.Minute, stall: 10 * time.Second}
	v, _, err := poll(fetch, cfg, sleep, clock.now)
	if err != nil {
		t.Fatalf("poll: %v (a progressing run must not be cut short by the stall window)", err)
	}
	if v != Passed {
		t.Fatalf("verdict = %v, want Passed", v)
	}
	if *calls != 3 {
		t.Fatalf("fetched %d times, want 3", *calls)
	}
}

func TestPollZeroStallDisablesStallDetection(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	fetch, _ := scriptedChecks([]Check{{Name: "deploy", Bucket: BucketPending}}) // frozen
	cfg := pollConfig{interval: 30 * time.Second, budget: 10 * time.Minute, stall: 0}
	_, _, err := poll(fetch, cfg, sleep, clock.now)
	// With stall disabled a frozen-pending run rides the full budget to ErrPollTimeout.
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v, want ErrPollTimeout (stall disabled)", err)
	}
}

func TestPollPropagatesFetchError(t *testing.T) {
	clock := newFakeClock()
	boom := errors.New("gh: auth required")
	fetch := func() ([]Check, error) { return nil, boom }
	_, _, err := poll(fetch, testPollCfg(), clock.sleep, clock.now)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
}
