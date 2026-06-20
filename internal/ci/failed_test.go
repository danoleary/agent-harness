package ci

import (
	"strings"
	"testing"
)

func TestFailedChecksReturnsOnlyFailures(t *testing.T) {
	checks := []Check{
		{Name: "build", Bucket: BucketPass},
		{Name: "lint", Bucket: BucketFail},
		{Name: "e2e", Bucket: BucketCancel},
		{Name: "optional", Bucket: BucketSkipping},
	}
	failed := FailedChecks(checks)
	if len(failed) != 2 {
		t.Fatalf("got %d failed, want 2 (%+v)", len(failed), failed)
	}
	if failed[0].Name != "lint" || failed[1].Name != "e2e" {
		t.Fatalf("failed = %+v, want lint + e2e", failed)
	}
}

func TestFailedChecksEmptyWhenAllGreen(t *testing.T) {
	checks := []Check{{Name: "build", Bucket: BucketPass}}
	if got := FailedChecks(checks); len(got) != 0 {
		t.Fatalf("got %d failed, want 0", len(got))
	}
}

func TestSummarizeListsFailingChecksAndLinks(t *testing.T) {
	failed := []Check{
		{Name: "lint", Bucket: BucketFail, Link: "https://github.com/beherd/herd/actions/runs/100/job/2"},
		{Name: "e2e", Bucket: BucketCancel, Link: "https://github.com/beherd/herd/actions/runs/101/job/3"},
	}
	s := Summarize(failed)
	for _, want := range []string{"lint", "e2e", "fail", "cancel", "runs/100", "runs/101"} {
		if !strings.Contains(s, want) {
			t.Fatalf("Summarize missing %q:\n%s", want, s)
		}
	}
}
