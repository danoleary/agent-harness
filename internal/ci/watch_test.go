package ci

import (
	"errors"
	"strings"
	"testing"
	"time"
)

type pollResult struct {
	v      Verdict
	checks []Check
	err    error
}

// fakeDriver scripts a sequence of Poll outcomes (the last repeats once the
// script is exhausted) and records every effect, so WatchAndFix's control flow
// is exercised without touching gh or the sandbox.
type fakeDriver struct {
	polls   []pollResult
	pollIdx int

	reruns, fixes, pushes, awaits int
	rerunErr, fixErr, awaitErr    error

	mergeState MergeVerdict // verdict d.MergeState() reports (zero = MergeUnknown)
	mergeErr   error

	clock   *fakeClock
	fixCost time.Duration
}

func (d *fakeDriver) Poll() (Verdict, []Check, error) {
	r := d.polls[d.pollIdx]
	if d.pollIdx < len(d.polls)-1 {
		d.pollIdx++
	}
	return r.v, r.checks, r.err
}
func (d *fakeDriver) Rerun([]Check) error { d.reruns++; return d.rerunErr }
func (d *fakeDriver) Fix([]Check) error {
	d.fixes++
	if d.clock != nil {
		d.clock.sleep(d.fixCost)
	}
	return d.fixErr
}
func (d *fakeDriver) Push() error         { d.pushes++; return nil }
func (d *fakeDriver) AwaitHeadRun() error { d.awaits++; return d.awaitErr }
func (d *fakeDriver) MergeState() (MergeVerdict, error) {
	return d.mergeState, d.mergeErr
}

func failChecks() []Check { return []Check{{Name: "lint", Bucket: BucketFail}} }

func testWatchCfg() Config { return Config{MaxFixAttempts: 2, Budget: time.Hour} }

func TestWatchGreenOnFirstPoll(t *testing.T) {
	d := &fakeDriver{polls: []pollResult{{v: Passed}}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK, got %+v", out)
	}
	if d.reruns != 0 || d.fixes != 0 {
		t.Fatalf("no rerun/fix expected on green CI; got rerun=%d fix=%d", d.reruns, d.fixes)
	}
}

func TestWatchBlocksGreenCIOnMergeConflict(t *testing.T) {
	// Green checks but the PR conflicts with base (main moved underneath it).
	// The harness must report not-passing — distinct from a check failure — and
	// must not try to auto-fix it (a merge conflict is a rebase problem).
	d := &fakeDriver{
		polls:      []pollResult{{v: Passed}},
		mergeState: MergeConflicting,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK on a merge conflict despite green CI, got %+v", out)
	}
	if d.reruns != 0 || d.fixes != 0 {
		t.Fatalf("a merge conflict must not trigger rerun/fix; got rerun=%d fix=%d", d.reruns, d.fixes)
	}
	r := strings.ToLower(out.Reason)
	if !strings.Contains(r, "conflict") && !strings.Contains(r, "merge") {
		t.Fatalf("reason %q should name the merge conflict, distinct from a check failure", out.Reason)
	}
}

func TestWatchGreenCIWithCleanMergePasses(t *testing.T) {
	// Green checks and a clean merge → the existing pass, unchanged.
	d := &fakeDriver{
		polls:      []pollResult{{v: Passed}},
		mergeState: MergeClean,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK on green CI + clean merge, got %+v", out)
	}
}

func TestWatchGreenCIDegradesOnMergeStateError(t *testing.T) {
	// An unreadable merge state must not fail an otherwise-green PR — it degrades
	// to the green pass (mirrors the checks-unobservable degrade philosophy).
	d := &fakeDriver{
		polls:    []pollResult{{v: Passed}},
		mergeErr: errors.New("gh pr view: boom"),
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK (merge state unreadable, CI green), got %+v", out)
	}
}

func TestWatchFlakeResolvedByRerun(t *testing.T) {
	// Fail, then green after the single flake re-run — no code fix should happen.
	d := &fakeDriver{polls: []pollResult{
		{v: Failed, checks: failChecks()},
		{v: Passed},
	}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK after flake re-run, got %+v", out)
	}
	if d.reruns != 1 {
		t.Fatalf("reruns = %d, want exactly 1 flake re-run", d.reruns)
	}
	if d.fixes != 0 {
		t.Fatalf("fixes = %d, want 0 (re-run resolved it)", d.fixes)
	}
}

func TestWatchRealFailureFixedOnFirstAttempt(t *testing.T) {
	// Fail, still fail after re-run (real), then green after one fix+push.
	d := &fakeDriver{polls: []pollResult{
		{v: Failed, checks: failChecks()},
		{v: Failed, checks: failChecks()},
		{v: Passed},
	}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK after fix, got %+v", out)
	}
	if d.reruns != 1 {
		t.Fatalf("reruns = %d, want 1", d.reruns)
	}
	if d.fixes != 1 || d.pushes != 1 {
		t.Fatalf("fix=%d push=%d, want 1/1", d.fixes, d.pushes)
	}
}

func TestWatchExhaustsAttemptsAndReportsFailing(t *testing.T) {
	// Never recovers — fix loop runs MaxFixAttempts then gives up, keeping the PR.
	d := &fakeDriver{polls: []pollResult{{v: Failed, checks: failChecks()}}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK on exhaustion, got %+v", out)
	}
	if d.fixes != 2 {
		t.Fatalf("fixes = %d, want MaxFixAttempts (2)", d.fixes)
	}
	if len(out.Failing) == 0 {
		t.Fatal("expected the last failing checks reported for the operator")
	}
}

func TestWatchStopsFixingWhenBudgetSpent(t *testing.T) {
	// Budget only fits one fix attempt even though MaxFixAttempts is 5.
	clock := newFakeClock()
	d := &fakeDriver{
		polls:   []pollResult{{v: Failed, checks: failChecks()}},
		clock:   clock,
		fixCost: 40 * time.Minute, // one fix overshoots the 30-min budget
	}
	out := WatchAndFix(d, Config{MaxFixAttempts: 5, Budget: 30 * time.Minute}, clock.now)
	if out.OK {
		t.Fatalf("expected non-OK, got %+v", out)
	}
	if d.fixes != 1 {
		t.Fatalf("fixes = %d, want 1 (budget cut it short)", d.fixes)
	}
}

func TestWatchReportsPollTimeout(t *testing.T) {
	d := &fakeDriver{polls: []pollResult{{v: Pending, checks: []Check{{Name: "build", Bucket: BucketPending}}, err: ErrPollTimeout}}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK on poll timeout, got %+v", out)
	}
	if !strings.Contains(strings.ToLower(out.Reason), "terminal") && !strings.Contains(strings.ToLower(out.Reason), "settle") {
		t.Fatalf("reason %q should explain CI never settled", out.Reason)
	}
}

func TestWatchSurfacesRerunError(t *testing.T) {
	boom := errors.New("gh run rerun: not found")
	d := &fakeDriver{
		polls:    []pollResult{{v: Failed, checks: failChecks()}},
		rerunErr: boom,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK, got %+v", out)
	}
	if d.fixes != 0 {
		t.Fatalf("fixes = %d, want 0 (re-run errored before any fix)", d.fixes)
	}
	if len(out.Failing) == 0 {
		t.Fatal("expected the failing checks reported")
	}
}

func TestWatchDegradesWhenChecksUnobservable(t *testing.T) {
	// The token opened the PR but can't read check runs (fine-grained PAT). The
	// PR shipped, so this is a success that degrades — not a failure to auto-fix:
	// no rerun, no fix, OK, and a reason that points the operator at manual CI.
	d := &fakeDriver{polls: []pollResult{{v: Pending, err: errChecksUnobservable}}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK (PR shipped, CI just unobservable), got %+v", out)
	}
	if d.reruns != 0 || d.fixes != 0 {
		t.Fatalf("unobservable CI must not trigger rerun/fix; got rerun=%d fix=%d", d.reruns, d.fixes)
	}
	if !strings.Contains(strings.ToLower(out.Reason), "unobservable") {
		t.Fatalf("reason %q should explain CI is unobservable with this token", out.Reason)
	}
}

func TestWatchSurfacesFixError(t *testing.T) {
	boom := errors.New("sandbox fix session crashed")
	d := &fakeDriver{
		polls:  []pollResult{{v: Failed, checks: failChecks()}, {v: Failed, checks: failChecks()}},
		fixErr: boom,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK, got %+v", out)
	}
	if !strings.Contains(out.Reason, "sandbox fix session crashed") {
		t.Fatalf("reason %q should surface the fix error", out.Reason)
	}
}

func TestWatchDoesNotRefixWhileCIRunPredatesFix(t *testing.T) {
	// Real failure → fix → push, but CI hasn't started a run for the new commit yet
	// (AwaitHeadRun times out → the only red still showing is the prior run's stale
	// one). The loop must NOT launch a second fix against that stale failure; it stops
	// and keeps the PR for a human. This is the BEH-493 regression: a fast fix landing
	// before CI re-evaluates used to burn a whole no-op fix session.
	d := &fakeDriver{
		polls: []pollResult{
			{v: Failed, checks: failChecks()}, // initial
			{v: Failed, checks: failChecks()}, // still red after the flake re-run → real
		},
		awaitErr: ErrCIRerunTimeout,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK when CI hasn't re-run for the fix, got %+v", out)
	}
	if d.fixes != 1 {
		t.Fatalf("fixes = %d, want 1 (must not re-fix against a stale CI run)", d.fixes)
	}
	if d.awaits != 1 {
		t.Fatalf("awaits = %d, want 1 (loop must wait for CI to re-run after pushing)", d.awaits)
	}
}

func TestWatchWaitsForCIRerunThenPasses(t *testing.T) {
	// Real failure → fix → push → wait for CI to start the run for the new commit →
	// re-poll green. The wait must happen (awaits == 1) before the post-push poll.
	d := &fakeDriver{
		polls: []pollResult{
			{v: Failed, checks: failChecks()},
			{v: Failed, checks: failChecks()},
			{v: Passed},
		},
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK after waiting for the re-run, got %+v", out)
	}
	if d.fixes != 1 || d.pushes != 1 {
		t.Fatalf("fix=%d push=%d, want 1/1", d.fixes, d.pushes)
	}
	if d.awaits != 1 {
		t.Fatalf("awaits = %d, want 1 (must wait for CI before the post-push poll)", d.awaits)
	}
}

func TestWatchSurfacesAwaitHeadRunError(t *testing.T) {
	// A non-timeout AwaitHeadRun failure (a real gh/git failure while confirming the
	// re-run, not ErrCIRerunTimeout) is surfaced verbatim — the non-timeout branch of
	// ciRerunErrOutcome, distinct from the "CI never started a run" outcome.
	d := &fakeDriver{
		polls: []pollResult{
			{v: Failed, checks: failChecks()}, // initial
			{v: Failed, checks: failChecks()}, // still red after the flake re-run → real
		},
		awaitErr: errors.New("git rev-parse: boom"),
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK when confirming the re-run errors, got %+v", out)
	}
	if d.fixes != 1 {
		t.Fatalf("fixes = %d, want 1 (must not re-fix when the re-run check errors)", d.fixes)
	}
	if !strings.Contains(out.Reason, "boom") {
		t.Fatalf("reason %q should surface the await error", out.Reason)
	}
}
