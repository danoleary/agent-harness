package ci

import "testing"

func TestClassifyPendingWhenAnyCheckStillRunning(t *testing.T) {
	checks := []Check{
		{Name: "build", Bucket: BucketPass},
		{Name: "lint", Bucket: BucketPending},
	}
	if got := Classify(checks); got != Pending {
		t.Fatalf("Classify = %v, want Pending", got)
	}
}

func TestClassifyFailedWhenAnyTerminalFailure(t *testing.T) {
	checks := []Check{
		{Name: "build", Bucket: BucketPass},
		{Name: "lint", Bucket: BucketFail},
	}
	if got := Classify(checks); got != Failed {
		t.Fatalf("Classify = %v, want Failed", got)
	}
}

func TestClassifyCancelCountsAsFailure(t *testing.T) {
	checks := []Check{{Name: "e2e", Bucket: BucketCancel}}
	if got := Classify(checks); got != Failed {
		t.Fatalf("Classify = %v, want Failed", got)
	}
}

func TestClassifyPassedWhenAllGreenOrSkipped(t *testing.T) {
	checks := []Check{
		{Name: "build", Bucket: BucketPass},
		{Name: "optional", Bucket: BucketSkipping},
	}
	if got := Classify(checks); got != Passed {
		t.Fatalf("Classify = %v, want Passed", got)
	}
}

// A still-pending check outranks an already-failed one: the run is not terminal
// until everything settles, so the verdict must stay Pending so the poller waits.
func TestClassifyPendingOutranksFailure(t *testing.T) {
	checks := []Check{
		{Name: "lint", Bucket: BucketFail},
		{Name: "build", Bucket: BucketPending},
	}
	if got := Classify(checks); got != Pending {
		t.Fatalf("Classify = %v, want Pending (not terminal yet)", got)
	}
}

func TestClassifyNoChecksIsPassed(t *testing.T) {
	if got := Classify(nil); got != Passed {
		t.Fatalf("Classify(nil) = %v, want Passed (nothing red to wait on)", got)
	}
}
