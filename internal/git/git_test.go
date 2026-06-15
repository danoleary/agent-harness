package git

import (
	"errors"
	"testing"
	"time"
)

// noSleep is the injected sleeper for tests — never actually waits.
func noSleep(time.Duration) {}

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

func TestPushUsesNoVerifyAndCorrectArgs(t *testing.T) {
	run, calls := scriptedRunner(0, nil)
	if err := push("/herd", "beh-329", run, noSleep); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one attempt, got %d", len(*calls))
	}
	got := (*calls)[0]
	want := []string{"git", "-C", "/herd", "push", "--no-verify", "origin", "feat/beh-329"}
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
	run, calls := scriptedRunner(2, errors.New("exit status 128"))
	if err := push("/herd", "beh-329", run, noSleep); err != nil {
		t.Fatalf("expected eventual success, got %v", err)
	}
	if len(*calls) != 3 {
		t.Fatalf("expected 3 attempts (2 fail + 1 ok), got %d", len(*calls))
	}
}

func TestPushReturnsLastErrorAfterExhausting(t *testing.T) {
	boom := errors.New("exit status 128")
	run, calls := scriptedRunner(remoteAttempts, boom)
	err := push("/herd", "beh-329", run, noSleep)
	if !errors.Is(err, boom) {
		t.Fatalf("expected the last error %v, got %v", boom, err)
	}
	if len(*calls) != remoteAttempts {
		t.Fatalf("expected %d attempts, got %d", remoteAttempts, len(*calls))
	}
}

func TestFetchMainRetriesThenSucceeds(t *testing.T) {
	run, calls := scriptedRunner(1, errors.New("exit status 128"))
	if err := fetchMain("/herd", run, noSleep); err != nil {
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

func TestWithRetrySleepsBetweenButNotAfterFinalAttempt(t *testing.T) {
	sleeps := 0
	count := func(time.Duration) { sleeps++ }
	err := withRetry(func() error { return errors.New("always") }, count)
	if err == nil {
		t.Fatal("expected an error when op always fails")
	}
	// remoteAttempts tries => remoteAttempts-1 sleeps (no trailing sleep).
	if sleeps != remoteAttempts-1 {
		t.Fatalf("expected %d sleeps, got %d", remoteAttempts-1, sleeps)
	}
}

func TestWithRetryNoSleepOnFirstSuccess(t *testing.T) {
	sleeps := 0
	count := func(time.Duration) { sleeps++ }
	if err := withRetry(func() error { return nil }, count); err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	if sleeps != 0 {
		t.Fatalf("expected no sleeps on immediate success, got %d", sleeps)
	}
}
