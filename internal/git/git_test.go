package git

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// fakeClock is a deterministic clock for retry tests: time advances only when the
// injected sleeper is called, so the wall-clock backoff/budget logic is exercised
// with zero real waiting (BEH-403). now() and sleep satisfy the withRetry seams.
type fakeClock struct{ t time.Time }

func newFakeClock() *fakeClock              { return &fakeClock{t: time.Unix(0, 0)} }
func (c *fakeClock) now() time.Time         { return c.t }
func (c *fakeClock) sleep(d time.Duration)  { c.t = c.t.Add(d) }
func (c *fakeClock) elapsed() time.Duration { return c.t.Sub(time.Unix(0, 0)) }

// scriptedRunner returns a commandRunner that fails for the first `failures`
// calls (returning errEach) then succeeds, recording every argv it saw.
func scriptedRunner(failures int, errEach error) (commandRunner, *[][]string) {
	var calls [][]string
	run := func(name string, args ...string) error {
		calls = append(calls, append([]string{name}, args...))
		if len(calls) <= failures {
			return errEach
		}
		return nil
	}
	return run, &calls
}

// blipRunner models a transient remote disruption: it fails while the clock reads
// less than `clearAfter` past its first call, then succeeds — i.e. a blip of a
// given wall-clock duration that the retry loop must ride out.
func blipRunner(clock *fakeClock, clearAfter time.Duration, errEach error) (commandRunner, *int) {
	calls := 0
	start := clock.now()
	run := func(string, ...string) error {
		calls++
		if clock.now().Sub(start) < clearAfter {
			return errEach
		}
		return nil
	}
	return run, &calls
}

// BEH-412: the implementation tool hands the worktree back to a reviewer who may
// be on a non-Linux host. The sandbox-built `web/node_modules` carries Linux-only
// native bindings that crash the macOS gates, and `pnpm install --frozen-lockfile`
// won't repair them. Stripping the tree forces the reviewer to install fresh for
// their own platform.
func TestStripWorktreeNodeModulesRemovesIt(t *testing.T) {
	wt := t.TempDir()
	binding := filepath.Join(wt, "web", "node_modules", "@oxlint", "binding-linux-arm64-gnu")
	if err := os.MkdirAll(binding, 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := StripWorktreeNodeModules(wt); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if _, err := os.Stat(filepath.Join(wt, "web", "node_modules")); !os.IsNotExist(err) {
		t.Fatalf("web/node_modules should be gone, stat err = %v", err)
	}
}

// Idempotent: a worktree that never ran `pnpm install` (or was already stripped)
// must not be an error — the strip runs unconditionally on every handoff.
func TestStripWorktreeNodeModulesIsNoOpWhenAbsent(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "web"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := StripWorktreeNodeModules(wt); err != nil {
		t.Fatalf("expected no error when node_modules is absent, got %v", err)
	}
}

// The strip is surgical: only `web/node_modules` goes — the committed source the
// reviewer is here to read (including everything else under `web/`) stays put.
func TestStripWorktreeNodeModulesLeavesSourceIntact(t *testing.T) {
	wt := t.TempDir()
	if err := os.MkdirAll(filepath.Join(wt, "web", "node_modules", "left-pad"), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	src := filepath.Join(wt, "web", "src", "app.tsx")
	if err := os.MkdirAll(filepath.Dir(src), 0o755); err != nil {
		t.Fatalf("setup: %v", err)
	}
	if err := os.WriteFile(src, []byte("export const App = () => null"), 0o644); err != nil {
		t.Fatalf("setup: %v", err)
	}

	if err := StripWorktreeNodeModules(wt); err != nil {
		t.Fatalf("expected success, got %v", err)
	}

	if _, err := os.Stat(src); err != nil {
		t.Fatalf("web/src/app.tsx should survive the strip, stat err = %v", err)
	}
}

func TestPushUsesNoVerifyAndCorrectArgs(t *testing.T) {
	clock := newFakeClock()
	run, calls := scriptedRunner(0, nil)
	if err := push("/herd", "beh-403", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one attempt, got %d", len(*calls))
	}
	got := (*calls)[0]
	want := []string{"git", "-C", "/herd", "push", "--no-verify", "origin", "feat/beh-403"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q (full: %v)", i, got[i], want[i], got)
		}
	}
}

func TestPushRetriesThenSucceeds(t *testing.T) {
	// Two transient failures, then success — must not be stranded.
	clock := newFakeClock()
	run, calls := scriptedRunner(2, errors.New("exit status 128"))
	if err := push("/herd", "beh-403", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("expected 3 attempts (2 fail + 1 ok), got %d", len(*calls))
	}
}

// The core of BEH-403: a disruption lasting a couple of minutes (far beyond the
// old ~4s window) must not strand a green branch — the loop keeps retrying until
// the blip clears.
func TestPushRidesOutMultiMinuteBlip(t *testing.T) {
	clock := newFakeClock()
	run, calls := blipRunner(clock, 2*time.Minute, errors.New("exit status 128"))
	if err := push("/herd", "beh-403", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected eventual success once the blip cleared, got %v", err)
	}
	if *calls < 2 {
		t.Fatalf("expected multiple attempts across the blip, got %d", *calls)
	}
	if clock.elapsed() < 2*time.Minute {
		t.Fatalf("retry span %v did not ride out the 2-minute blip", clock.elapsed())
	}
}

// An indefinite outage must terminate — but only after spending close to the full
// budget, not after the old 4s window — and never exceed the budget.
func TestPushGivesUpNearBudgetAfterPersistentFailure(t *testing.T) {
	clock := newFakeClock()
	boom := errors.New("exit status 128")
	run, calls := scriptedRunner(1<<30, boom) // always fails
	err := push("/herd", "beh-403", run, clock.sleep, clock.now)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the last error %v, got %v", boom, err)
	}
	if clock.elapsed() > remoteRetryBudget {
		t.Fatalf("retry span %v exceeded the budget %v", clock.elapsed(), remoteRetryBudget)
	}
	if clock.elapsed() < remoteRetryBudget-remoteMaxDelay {
		t.Fatalf("retry span %v gave up well before the budget %v", clock.elapsed(), remoteRetryBudget)
	}
	if len(*calls) < 2 {
		t.Fatalf("expected many attempts within the budget, got %d", len(*calls))
	}
}

func TestFetchMainRetriesThenSucceeds(t *testing.T) {
	clock := newFakeClock()
	run, calls := scriptedRunner(1, errors.New("exit status 128"))
	if err := fetchMain("/herd", run, clock.sleep, clock.now); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if len(*calls) != 2 {
		t.Fatalf("expected 2 attempts (1 fail + 1 ok), got %d", len(*calls))
	}
	got := (*calls)[0]
	want := []string{"git", "-C", "/herd", "fetch", "-q", "origin", "main"}
	if len(got) != len(want) {
		t.Fatalf("argv = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("argv[%d] = %q, want %q", i, got[i], want[i])
		}
	}
}

func TestWithRetryBackoffIsExponentialAndCapped(t *testing.T) {
	clock := newFakeClock()
	var delays []time.Duration
	sleep := func(d time.Duration) { delays = append(delays, d); clock.sleep(d) }
	if err := withRetry(func() error { return errors.New("always") }, sleep, clock.now); err == nil {
		t.Fatal("expected an error when op always fails")
	}
	if len(delays) < 2 {
		t.Fatalf("expected several backoff sleeps, got %d", len(delays))
	}
	if delays[0] != remoteBaseDelay {
		t.Fatalf("first delay = %v, want base %v", delays[0], remoteBaseDelay)
	}
	if delays[1] != 2*remoteBaseDelay {
		t.Fatalf("second delay = %v, want %v (doubled)", delays[1], 2*remoteBaseDelay)
	}
	for i, d := range delays {
		if d > remoteMaxDelay {
			t.Fatalf("delay[%d] = %v exceeds cap %v", i, d, remoteMaxDelay)
		}
	}
	if last := delays[len(delays)-1]; last != remoteMaxDelay {
		t.Fatalf("last delay = %v, want it to have grown to the cap %v", last, remoteMaxDelay)
	}
}

func TestWithRetryNoSleepOnFirstSuccess(t *testing.T) {
	clock := newFakeClock()
	sleeps := 0
	sleep := func(d time.Duration) { sleeps++; clock.sleep(d) }
	if err := withRetry(func() error { return nil }, sleep, clock.now); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if sleeps != 0 {
		t.Fatalf("expected no sleeps on immediate success, got %d", sleeps)
	}
}
