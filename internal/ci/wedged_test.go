package ci

import "testing"

// The structural-wedge signal: every real gate has gone green and the only thing
// still pending is a required context GitHub lists as EXPECTED but never schedules a
// run for on the PR head (a merge-queue/main-only check). That PR is ready for the
// merge queue, so wedgedReady must report true (BEH-614).
func TestWedgedReadyWhenGatesGreenAndOnlyExpectedPending(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "test", Bucket: BucketPass},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	if !wedgedReady(checks) {
		t.Fatalf("gates green + only an EXPECTED context pending should be wedged-ready; got false")
	}
}

// Safety against a false positive at PR-open: before any workflow runs, required
// contexts can appear as EXPECTED with nothing yet green. With no real check passed,
// the watch must keep polling — the run hasn't actually validated anything.
func TestNotWedgedReadyWhenNothingHasGoneGreenYet(t *testing.T) {
	checks := []Check{
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
		{Name: "main-only-deploy", Bucket: BucketPending, State: StateExpected},
	}
	if wedgedReady(checks) {
		t.Fatalf("all-EXPECTED with no real green check must not be ready (nothing validated yet)")
	}
}

// A genuinely-pending real check (a QUEUED/IN_PROGRESS run, not EXPECTED) means the
// run is still in flight — not wedged-ready even though gates so far are green.
func TestNotWedgedReadyWhileARealCheckIsStillRunning(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "test", Bucket: BucketPending, State: "IN_PROGRESS"},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	if wedgedReady(checks) {
		t.Fatalf("a real check still running means not ready; got true")
	}
}

// A failing real check is a real failure to act on, never a ready-for-merge-queue pass.
func TestNotWedgedReadyWhenARealCheckFailed(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketFail},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	if wedgedReady(checks) {
		t.Fatalf("a failed real check must not be reported as ready; got true")
	}
}

// No wedge at all (a plain all-green set): not a wedged-ready case — the normal
// Passed verdict already covers it, so wedgedReady stays false.
func TestNotWedgedReadyWithoutAnExpectedContext(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "test", Bucket: BucketPass},
	}
	if wedgedReady(checks) {
		t.Fatalf("an all-green set with no EXPECTED context is not wedged-ready; got true")
	}
}

// A frozen pending set is only safe to bail on as a wedge when every pending check is
// an EXPECTED context. With a single EXPECTED context (other checks terminal),
// pendingAllWedged is true — the stall detector may treat the freeze as terminal.
func TestPendingAllWedgedWithOnlyExpectedPending(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	if !pendingAllWedged(checks) {
		t.Fatalf("only-EXPECTED pending should be all-wedged; got false")
	}
}

// A still-running real gate (pending, not EXPECTED) means the freeze is a slow-but-
// progressing run, not a wedge — pendingAllWedged must be false so the poller keeps
// waiting instead of abandoning an about-to-go-green PR (BEH-623).
func TestNotPendingAllWedgedWhileARealGateIsRunning(t *testing.T) {
	checks := []Check{
		{Name: "Validate migrations", Bucket: BucketPass},
		{Name: "Linting and tests", Bucket: BucketPending, State: "IN_PROGRESS"},
		{Name: "merge-queue-gate", Bucket: BucketPending, State: StateExpected},
	}
	if pendingAllWedged(checks) {
		t.Fatalf("a real gate still running means the freeze is not a wedge; got true")
	}
}

// An all-terminal set has no pending check to be wedged on — pendingAllWedged is
// false, so the stall path never fires on a run that has already settled.
func TestNotPendingAllWedgedWhenNothingPending(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketPass},
		{Name: "deploy", Bucket: BucketSkipping},
	}
	if pendingAllWedged(checks) {
		t.Fatalf("an all-terminal set has nothing pending to wedge on; got true")
	}
}
