package ci

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestRerunWithoutActionsRunsDoesNotWait(t *testing.T) {
	// A non-Actions check (external status, no /actions/runs/<id> link) has no run
	// id, so there is nothing to re-trigger and nothing to wait for: Rerun must not
	// sleep and must not shell out to gh. (The has-run-id path waits one poll
	// interval to let GitHub flip the checks back to pending before the re-poll.)
	slept := 0
	d := &GhDriver{
		pollCfg: pollConfig{interval: 30 * time.Second},
		sleep:   func(time.Duration) { slept++ },
	}
	if err := d.Rerun([]Check{{Name: "external", Bucket: BucketFail, Link: "https://example.com/status"}}); err != nil {
		t.Fatalf("Rerun: %v", err)
	}
	if slept != 0 {
		t.Fatalf("slept %d times, want 0 when there are no Actions runs to re-trigger", slept)
	}
}

func TestInterpretChecksOutputParsesDespiteNonZeroExit(t *testing.T) {
	// gh pr checks exits non-zero when checks fail, but the JSON is on stdout.
	stdout := []byte(`[{"name":"lint","bucket":"fail"}]`)
	exitErr := errors.New("exit status 1")
	checks, err := interpretChecksOutput(stdout, nil, exitErr)
	if err != nil {
		t.Fatalf("expected the JSON to win over the benign non-zero exit, got %v", err)
	}
	if len(checks) != 1 || checks[0].Bucket != BucketFail {
		t.Fatalf("checks = %+v, want one fail", checks)
	}
}

func TestInterpretChecksOutputSurfacesRealGhFailure(t *testing.T) {
	// No parseable JSON on stdout → a real gh failure; surface the error + stderr.
	_, err := interpretChecksOutput([]byte(""), []byte("gh: authentication required"), errors.New("exit status 4"))
	if err == nil {
		t.Fatal("expected an error when stdout has no JSON")
	}
	if !strings.Contains(err.Error(), "authentication required") {
		t.Fatalf("error should surface gh's stderr, got %q", err.Error())
	}
}

func TestInterpretChecksOutputTreatsNoChecksYetAsTransient(t *testing.T) {
	// Right after the PR opens, Actions may not have registered checks yet: gh
	// exits non-zero with "no checks reported" and no JSON. That is a transient
	// pending state to wait through, not a hard failure that aborts the watch.
	_, err := interpretChecksOutput([]byte(""), []byte("no checks reported on the 'feat/x' branch"), errors.New("exit status 8"))
	if !errors.Is(err, errNoChecksYet) {
		t.Fatalf("err = %v, want errNoChecksYet", err)
	}
}

func TestInterpretChecksOutputFlagsUnreadableChecksAsUnobservable(t *testing.T) {
	// A fine-grained PAT can fetch/push/open the PR but cannot read check runs:
	// `gh pr checks` 403s with "Resource not accessible by personal access token".
	// That is a *permission* failure (the token will never see CI), distinct from
	// a real gh failure (auth required, bad branch) or a transient pending window —
	// it must surface as errChecksUnobservable so the watch can degrade, not abort.
	stderr := []byte("GraphQL: Resource not accessible by personal access token (repository.pullRequest.statusCheckRollup.contexts)")
	_, err := interpretChecksOutput([]byte(""), stderr, errors.New("exit status 1"))
	if !errors.Is(err, errChecksUnobservable) {
		t.Fatalf("err = %v, want errChecksUnobservable", err)
	}
}

func TestTruncateLogsKeepsTailWhenOverLimit(t *testing.T) {
	// CI failures show at the end of the log, so truncation keeps the tail.
	body := strings.Repeat("x", 100) + "THE ACTUAL ERROR"
	got := truncateLogs(body, 32)
	if !strings.Contains(got, "THE ACTUAL ERROR") {
		t.Fatalf("truncated logs dropped the trailing error: %q", got)
	}
	if !strings.Contains(got, "truncated") {
		t.Fatalf("truncation should leave a marker, got %q", got)
	}
	if len(got) > 32+64 { // limit + a short marker line
		t.Fatalf("truncated logs too long (%d): %q", len(got), got)
	}
}

func TestTruncateLogsLeavesShortLogsUntouched(t *testing.T) {
	body := "short log"
	if got := truncateLogs(body, 1000); got != body {
		t.Fatalf("short logs should pass through unchanged, got %q", got)
	}
}
