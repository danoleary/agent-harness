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

func TestPollExtendsPastSoftBudgetWhileRealGateInFlight(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	// The BEH-685 scenario: a real required gate (linting_and_tests, the browser-backed
	// storybook/e2e suite) is still running well past the soft budget, alongside a
	// merge-queue-only EXPECTED context. With an adaptive maxBudget above the soft budget,
	// the poll must keep waiting for the slow-but-real gate rather than timing out mid-run
	// and dumping a healthy about-to-go-green PR to manual triage.
	running := []Check{
		{Name: "linting_and_tests", Bucket: BucketPending, State: "IN_PROGRESS"},
		{Name: "build", Bucket: BucketPending, State: StateExpected}, // merge_group only
	}
	green := []Check{
		{Name: "linting_and_tests", Bucket: BucketPass},
		{Name: "build", Bucket: BucketPass},
	}
	// Soft budget 2m (polls at 0,30,…,120s would time out); the gate only settles green at
	// t=210s (3.5m), which is inside the 6m hard ceiling. Stall disabled to isolate the
	// budget behaviour.
	snaps := [][]Check{running, running, running, running, running, running, running, green}
	fetch, _ := scriptedChecks(snaps...)
	cfg := pollConfig{interval: 30 * time.Second, budget: 2 * time.Minute, maxBudget: 6 * time.Minute}
	v, _, err := poll(fetch, cfg, sleep, clock.now)
	if err != nil {
		t.Fatalf("poll: %v (a slow-but-running real gate must not be abandoned at the soft budget)", err)
	}
	if v != Passed {
		t.Fatalf("verdict = %v, want Passed once the extended poll reaches the green terminal", v)
	}
}

func TestPollExtensionRespectsHardCeiling(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	// A real gate stuck pending forever: the adaptive extension buys extra time past the
	// soft budget, but the hard ceiling still enforces ErrPollTimeout so a genuinely-hung
	// required gate can never make the poll run indefinitely.
	fetch, _ := scriptedChecks([]Check{
		{Name: "linting_and_tests", Bucket: BucketPending, State: "IN_PROGRESS"},
	})
	cfg := pollConfig{interval: 30 * time.Second, budget: 2 * time.Minute, maxBudget: 6 * time.Minute}
	v, _, err := poll(fetch, cfg, sleep, clock.now)
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v, want ErrPollTimeout at the hard ceiling", err)
	}
	if v != Pending {
		t.Fatalf("verdict = %v, want Pending on timeout", v)
	}
	// It must have polled well past the 2m soft budget (proving the extension fired) but
	// stopped at the 6m ceiling rather than looping forever.
	if elapsed := clock.now().Sub(time.Unix(0, 0)); elapsed <= 2*time.Minute || elapsed > 6*time.Minute {
		t.Fatalf("elapsed = %v, want in (2m, 6m] — extended past the soft budget, capped at the ceiling", elapsed)
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
	// A genuinely-stuck PR: its only check is a merge-queue/main-only required context
	// GitHub reports as EXPECTED but never schedules on the PR head, and no real gate
	// has gone green (so it is not wedged-ready). The frozen pending set is entirely
	// EXPECTED, so the stall window correctly bails rather than burning the full budget
	// on a check that will never move (BEH-623: only an all-wedged freeze stalls).
	fetch, calls := scriptedChecks([]Check{
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected}, // never moves on a PR
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

func TestPollDoesNotStallWhileARealGateIsStillRunning(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	// BEH-508: a real gate ("Linting and tests") legitimately runs for ~5min. Its
	// bucket stays `pending` the whole time, so the (name,bucket) signature is frozen
	// even though CI is progressing — it just hasn't finished. The stall window must
	// NOT misclassify a still-running real gate (pending, not EXPECTED) as a merge-
	// queue wedge and abandon a PR that is about to go green; only an all-wedged frozen
	// set bails as ErrPollStalled. The running gate rides the budget until it settles.
	running := []Check{
		{Name: "Validate migrations", Bucket: BucketPass},
		{Name: "Linting and tests", Bucket: BucketPending, State: "IN_PROGRESS"},
	}
	green := []Check{
		{Name: "Validate migrations", Bucket: BucketPass},
		{Name: "Linting and tests", Bucket: BucketPass},
	}
	// Frozen on the running snapshot well past the 2m stall window, then it goes green.
	fetch, _ := scriptedChecks(running, running, running, running, running, running, green)
	cfg := pollConfig{interval: 30 * time.Second, budget: 20 * time.Minute, stall: 2 * time.Minute}
	v, _, err := poll(fetch, cfg, sleep, clock.now)
	if err != nil {
		t.Fatalf("poll: %v (a still-running real gate must not be cut short as a wedge stall)", err)
	}
	if v != Passed {
		t.Fatalf("verdict = %v, want Passed", v)
	}
}

func TestPollDoesNotStallOnColdStartAllPending(t *testing.T) {
	clock := newFakeClock()
	sleep := func(d time.Duration) { clock.sleep(d) }
	// A slow cold start: every required gate is still QUEUED (pending, no EXPECTED
	// state, nothing green) because the CI runners have not picked the jobs up yet.
	// The signature is frozen, but byte-for-byte it is indistinguishable from a real
	// wedge ONLY if we ignore that nothing has progressed. A slow start is not a wedge,
	// so the poll must keep waiting to the budget, not bail with ErrPollStalled (BEH-620).
	fetch, _ := scriptedChecks([]Check{
		{Name: "build", Bucket: BucketPending},
		{Name: "storybook", Bucket: BucketPending},
		{Name: "e2e", Bucket: BucketPending},
	})
	cfg := pollConfig{interval: 30 * time.Second, budget: 10 * time.Minute, stall: 2 * time.Minute}
	v, _, err := poll(fetch, cfg, sleep, clock.now)
	if errors.Is(err, ErrPollStalled) {
		t.Fatal("a frozen all-pending cold start must not be treated as a stall (BEH-620)")
	}
	if !errors.Is(err, ErrPollTimeout) {
		t.Fatalf("err = %v, want ErrPollTimeout (rode to budget, no stall)", err)
	}
	if v != Pending {
		t.Fatalf("verdict = %v, want Pending", v)
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

func TestPollShortCircuitsImmediatelyOnWedgedReady(t *testing.T) {
	clock := newFakeClock()
	slept := 0
	sleep := func(d time.Duration) { slept++; clock.sleep(d) }
	// Real gates green, only a merge-queue/main-only EXPECTED context left pending.
	fetch, calls := scriptedChecks([]Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "test", Bucket: BucketPass},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	})
	// A generous stall window: the point is it bails on the FIRST poll, well before stall.
	cfg := pollConfig{interval: 30 * time.Second, budget: 20 * time.Minute, stall: 5 * time.Minute}
	v, checks, err := poll(fetch, cfg, sleep, clock.now)
	if !errors.Is(err, ErrWedgedReadyForMergeQueue) {
		t.Fatalf("err = %v, want ErrWedgedReadyForMergeQueue", err)
	}
	if v != Pending {
		t.Fatalf("verdict = %v, want Pending (the wedge is still pending)", v)
	}
	if *calls != 1 {
		t.Fatalf("fetched %d times, want 1 (short-circuit on the first poll, not the stall window)", *calls)
	}
	if slept != 0 {
		t.Fatalf("slept %d times, want 0 (no waiting on a wedged-ready run)", slept)
	}
	if len(checks) == 0 {
		t.Fatal("expected the checks to be returned for the operator report")
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
