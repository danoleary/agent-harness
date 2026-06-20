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

func TestPollPropagatesFetchError(t *testing.T) {
	clock := newFakeClock()
	boom := errors.New("gh: auth required")
	fetch := func() ([]Check, error) { return nil, boom }
	_, _, err := poll(fetch, testPollCfg(), clock.sleep, clock.now)
	if !errors.Is(err, boom) {
		t.Fatalf("err = %v, want the fetch error", err)
	}
}
