package ci

import (
	"reflect"
	"testing"
)

func TestRunIDsExtractsAndDedupes(t *testing.T) {
	// Two failing jobs of the SAME run plus a job of another run → two run ids.
	failed := []Check{
		{Name: "lint", Link: "https://github.com/beherd/herd/actions/runs/100/job/2"},
		{Name: "typecheck", Link: "https://github.com/beherd/herd/actions/runs/100/job/9"},
		{Name: "e2e", Link: "https://github.com/beherd/herd/actions/runs/101/job/3"},
	}
	got := RunIDs(failed)
	want := []string{"100", "101"}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("RunIDs = %v, want %v", got, want)
	}
}

func TestRunIDsHandlesRunOnlyLink(t *testing.T) {
	failed := []Check{{Name: "build", Link: "https://github.com/beherd/herd/actions/runs/55"}}
	if got := RunIDs(failed); !reflect.DeepEqual(got, []string{"55"}) {
		t.Fatalf("RunIDs = %v, want [55]", got)
	}
}

func TestRunIDsSkipsNonActionsLinks(t *testing.T) {
	// A third-party status check (e.g. an external CI) carries a non-Actions link
	// the harness can't `gh run` against — it must be skipped, not mis-parsed.
	failed := []Check{
		{Name: "vercel", Link: "https://vercel.com/beherd/deployments/abc"},
		{Name: "lint", Link: "https://github.com/beherd/herd/actions/runs/100/job/2"},
		{Name: "no-link", Link: ""},
	}
	if got := RunIDs(failed); !reflect.DeepEqual(got, []string{"100"}) {
		t.Fatalf("RunIDs = %v, want [100]", got)
	}
}

func TestRunIDsEmptyWhenNoneParseable(t *testing.T) {
	if got := RunIDs([]Check{{Name: "x", Link: "https://example.com/foo"}}); len(got) != 0 {
		t.Fatalf("RunIDs = %v, want empty", got)
	}
}
