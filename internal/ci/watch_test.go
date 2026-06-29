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
	polls     []pollResult
	pollIdx   int
	pollCalls int

	// diffEmpty is what DiffEmpty() reports — the zero-net-diff short-circuit signal
	// WatchAndFix consults before its first poll (BEH-602). Defaults false so every
	// existing test exercises the normal watch.
	diffEmpty bool

	reruns, fixes, pushes, awaits int
	rerunErr, fixErr, awaitErr    error

	mergeState MergeVerdict // verdict d.MergeState() reports (zero = MergeUnknown)
	mergeErr   error
	merges     []MergeVerdict // scripted MergeState sequence (last repeats); overrides mergeState
	mergeIdx   int

	// BEH-570 reactive auto-rebase: RebaseOntoBase's scripted verdict/error + a count.
	rebaseVerdict RebaseVerdict
	rebaseErr     error
	rebases       int

	clock   *fakeClock
	fixCost time.Duration
}

func (d *fakeDriver) Poll() (Verdict, []Check, error) {
	d.pollCalls++
	r := d.polls[d.pollIdx]
	if d.pollIdx < len(d.polls)-1 {
		d.pollIdx++
	}
	return r.v, r.checks, r.err
}
func (d *fakeDriver) DiffEmpty() bool     { return d.diffEmpty }
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
	if d.mergeErr != nil {
		return MergeUnknown, d.mergeErr
	}
	if len(d.merges) > 0 {
		v := d.merges[d.mergeIdx]
		if d.mergeIdx < len(d.merges)-1 {
			d.mergeIdx++
		}
		return v, nil
	}
	return d.mergeState, nil
}
func (d *fakeDriver) RebaseOntoBase() (RebaseVerdict, error) {
	d.rebases++
	return d.rebaseVerdict, d.rebaseErr
}

func failChecks() []Check { return []Check{{Name: "lint", Bucket: BucketFail}} }

func testWatchCfg() Config { return Config{MaxFixAttempts: 2, Budget: time.Hour} }

// BEH-602/BEH-604: a pushed branch byte-identical to origin/main has nothing for CI
// to validate that main hasn't already validated — polling it burns the whole poll
// budget on a PR that can never meaningfully go green (the PR #642 waste). WatchAndFix
// must short-circuit to the recommend-close disposition BEFORE the first poll. Scripted
// with NO polls so any poll attempt panics — proving the watch never started.
func TestWatchShortCircuitsZeroDiffBranch(t *testing.T) {
	d := &fakeDriver{diffEmpty: true}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("a zero-diff branch is not a green ship; want !OK, got %+v", out)
	}
	if !out.RecommendClose {
		t.Fatalf("a zero-diff branch must recommend close; got %+v", out)
	}
	if d.pollCalls != 0 {
		t.Fatalf("expected no poll on a zero-diff branch (short-circuit before the watch); got %d", d.pollCalls)
	}
}

// The short-circuit must not fire on a real diff: a branch that changes something
// proceeds into the normal watch (and polls) exactly as before.
func TestWatchDoesNotShortCircuitOnRealDiff(t *testing.T) {
	d := &fakeDriver{diffEmpty: false, polls: []pollResult{{v: Passed}}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK || out.RecommendClose {
		t.Fatalf("a real-diff green branch should pass normally; got %+v", out)
	}
	if d.pollCalls == 0 {
		t.Fatalf("expected the normal watch to poll on a real-diff branch")
	}
}

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

func TestWatchBlocksGreenCIOnGenuineContentConflict(t *testing.T) {
	// Green checks but the PR has a GENUINE content conflict with base — the
	// auto-rebase can't apply cleanly. The harness must report not-passing (distinct
	// from a check failure), must not try to code-fix it (a merge conflict is a rebase
	// problem), and must have attempted exactly one rebase before deferring to a human.
	d := &fakeDriver{
		polls:         []pollResult{{v: Passed}},
		mergeState:    MergeConflicting,
		rebaseVerdict: RebaseConflict,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK on a genuine content conflict despite green CI, got %+v", out)
	}
	if d.rebases != 1 {
		t.Fatalf("rebases = %d, want exactly 1 (try the auto-rebase before deferring)", d.rebases)
	}
	if d.reruns != 0 || d.fixes != 0 {
		t.Fatalf("a merge conflict must not trigger rerun/fix; got rerun=%d fix=%d", d.reruns, d.fixes)
	}
	r := strings.ToLower(out.Reason)
	if !strings.Contains(r, "conflict") && !strings.Contains(r, "merge") {
		t.Fatalf("reason %q should name the merge conflict, distinct from a check failure", out.Reason)
	}
}

func TestWatchAutoRebasesStaleBaseConflict(t *testing.T) {
	// Green checks, but main moved underneath the branch so the PR reads CONFLICTING.
	// It is NOT a genuine content conflict — the rebase applies cleanly and is
	// re-pushed — so after CI re-runs green on the rebased commit and merge reads
	// clean, the watch passes WITHOUT human intervention (the BEH-570 dead-end).
	d := &fakeDriver{
		polls:         []pollResult{{v: Passed}},
		merges:        []MergeVerdict{MergeConflicting, MergeClean},
		rebaseVerdict: RebaseClean,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("expected OK after auto-rebasing a stale-base conflict, got %+v", out)
	}
	if d.rebases != 1 {
		t.Fatalf("rebases = %d, want exactly 1", d.rebases)
	}
	if d.awaits != 1 {
		t.Fatalf("awaits = %d, want 1 (must confirm CI re-ran on the rebased HEAD)", d.awaits)
	}
	if d.fixes != 0 {
		t.Fatalf("fixes = %d, want 0 (a rebase is not a code fix)", d.fixes)
	}
}

func TestWatchGivesUpAfterRepeatedRebaseConflicts(t *testing.T) {
	// main keeps moving: every rebase applies cleanly but the merge re-reads
	// CONFLICTING each time. The watch must not chase main forever — it caps the
	// auto-rebase attempts and then defers to a human.
	d := &fakeDriver{
		polls:         []pollResult{{v: Passed}},
		mergeState:    MergeConflicting, // never clears
		rebaseVerdict: RebaseClean,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK once the rebase cap is hit, got %+v", out)
	}
	if d.rebases != maxRebaseAttempts {
		t.Fatalf("rebases = %d, want the cap %d", d.rebases, maxRebaseAttempts)
	}
	if !strings.Contains(strings.ToLower(out.Reason), "conflict") {
		t.Fatalf("reason %q should name the merge conflict", out.Reason)
	}
}

func TestWatchSurfacesRebaseError(t *testing.T) {
	// The auto-rebase/re-push errored (a gh/git failure, not a content conflict):
	// surface it and keep the PR for a human rather than silently passing.
	d := &fakeDriver{
		polls:      []pollResult{{v: Passed}},
		mergeState: MergeConflicting,
		rebaseErr:  errors.New("force-with-lease push rejected"),
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK when the auto-rebase errors, got %+v", out)
	}
	if !strings.Contains(out.Reason, "force-with-lease push rejected") {
		t.Fatalf("reason %q should surface the rebase error", out.Reason)
	}
}

func TestWatchReportsCIRedAfterRebase(t *testing.T) {
	// A stale-base conflict rebases cleanly, but CI comes back RED on the rebased
	// commit (the new base genuinely broke the build). That is now a real failure to
	// report, distinct from the merge-conflict outcome.
	d := &fakeDriver{
		polls: []pollResult{
			{v: Passed},                       // initial green that triggers greenOutcome
			{v: Failed, checks: failChecks()}, // post-rebase re-poll is red
		},
		mergeState:    MergeConflicting,
		rebaseVerdict: RebaseClean,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK when CI is red on the rebased commit, got %+v", out)
	}
	if d.rebases != 1 {
		t.Fatalf("rebases = %d, want 1", d.rebases)
	}
	if len(out.Failing) == 0 {
		t.Fatal("expected the failing checks reported for the operator")
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

func TestWatchReportsPollStalled(t *testing.T) {
	d := &fakeDriver{polls: []pollResult{{v: Pending, checks: []Check{{Name: "deploy", Bucket: BucketPending}}, err: ErrPollStalled}}}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK on a stalled poll, got %+v", out)
	}
	if !strings.Contains(strings.ToLower(out.Reason), "stall") {
		t.Fatalf("reason %q should explain CI stalled while pending", out.Reason)
	}
	if len(out.Failing) == 0 {
		t.Fatal("expected the wedged checks carried for the operator report")
	}
}

// A wedged-ready poll (real gates green, only a merge-queue/main-only EXPECTED context
// pending) is a SUCCESS, not a stall: the PR has validated everything that will run on
// it and is ready for the merge queue. WatchAndFix must report OK with a distinct
// ready-for-merge-queue reason, never code-fix it, and still confirm mergeability
// (the merge-state gate runs) (BEH-614).
func TestWatchPassesWedgedReadyAsReadyForMergeQueue(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	d := &fakeDriver{
		polls:      []pollResult{{v: Pending, checks: checks, err: ErrWedgedReadyForMergeQueue}},
		mergeState: MergeClean,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("a wedged-ready PR is a green ship; want OK, got %+v", out)
	}
	if d.fixes != 0 || d.reruns != 0 {
		t.Fatalf("must not fix/rerun a wedged-ready PR; got fix=%d rerun=%d", d.fixes, d.reruns)
	}
	low := strings.ToLower(out.Reason)
	if !strings.Contains(low, "merge queue") {
		t.Fatalf("reason %q should name the ready-for-merge-queue disposition", out.Reason)
	}
}

// The wedged-ready pass is still gated on mergeability: if main moved underneath the
// branch into a genuine content conflict, it must NOT be reported as a clean ship.
func TestWatchWedgedReadyStillGatesOnMergeConflict(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	d := &fakeDriver{
		polls:         []pollResult{{v: Pending, checks: checks, err: ErrWedgedReadyForMergeQueue}},
		mergeState:    MergeConflicting,
		rebaseVerdict: RebaseConflict,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("a wedged-ready PR with a genuine merge conflict is not shippable; got %+v", out)
	}
}

// The wedged-ready sentinel is honoured INSIDE the auto-rebase loop too: a stale-base
// wedged-ready PR rebases cleanly, comes back wedged-ready on the rebased HEAD, and
// once the merge re-reads clean it still ships — the sentinel must not be mistaken for
// "CI red after rebase" (BEH-614 × BEH-570).
func TestWatchWedgedReadyHonouredAfterAutoRebase(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	wedged := pollResult{v: Pending, checks: checks, err: ErrWedgedReadyForMergeQueue}
	d := &fakeDriver{
		polls:         []pollResult{wedged, wedged},
		merges:        []MergeVerdict{MergeConflicting, MergeClean},
		rebaseVerdict: RebaseClean,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if !out.OK {
		t.Fatalf("wedged-ready after a clean auto-rebase is a ship; got %+v", out)
	}
	if d.rebases != 1 {
		t.Fatalf("rebases = %d, want exactly 1", d.rebases)
	}
	if d.awaits != 1 {
		t.Fatalf("awaits = %d, want 1 (must confirm CI re-ran on the rebased HEAD)", d.awaits)
	}
	if d.fixes != 0 {
		t.Fatalf("fixes = %d, want 0 (a wedged-ready PR is never code-fixed)", d.fixes)
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

func TestWatchDefersOnSpendingCapAbort(t *testing.T) {
	// The auto-fix session died on an active spending cap (BEH-571): it could not
	// run, so the red is not a code defect or unfixable CI — the cap resets and a
	// re-dispatch would proceed. WatchAndFix must surface this as its own
	// retry-after-reset class (SpendingCapAbort + a distinct reason), NOT as a
	// generic "auto-fix session failed", and must not push a non-existent fix.
	d := &fakeDriver{
		polls:  []pollResult{{v: Failed, checks: failChecks()}, {v: Failed, checks: failChecks()}},
		fixErr: ErrSpendingCapActive,
	}
	out := WatchAndFix(d, testWatchCfg(), newFakeClock().now)
	if out.OK {
		t.Fatalf("expected non-OK on a spending-cap abort, got %+v", out)
	}
	if !out.SpendingCapAbort {
		t.Fatalf("expected SpendingCapAbort=true, got %+v", out)
	}
	if !strings.Contains(out.Reason, "spending cap") || !strings.Contains(out.Reason, "retry after reset") {
		t.Fatalf("reason %q should name the retry-after-reset class", out.Reason)
	}
	if strings.Contains(out.Reason, "auto-fix session failed") {
		t.Fatalf("reason %q must not be the generic fix-failure wording", out.Reason)
	}
	if d.pushes != 0 {
		t.Fatalf("pushes = %d, want 0 (nothing was fixed to push)", d.pushes)
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
