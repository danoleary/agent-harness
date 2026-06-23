package session

import (
	"testing"
	"time"

	"github.com/beherd/agent-harness/internal/sandbox"
)

// A 137 (OOM-kill) on the first attempt is transient (BEH-524): the helper must
// sleep the backoff and retry, surfacing the eventual success.
func TestRetryOnOOMKill_RetriesThenSucceeds(t *testing.T) {
	var slept []time.Duration
	codes := []int{sandbox.ExitOOMKill, 0}
	out, attempts := RetryOnOOMKill(3, 2*time.Second, func(d time.Duration) { slept = append(slept, d) },
		func(attempt int) Outcome { return Outcome{ExitCode: codes[attempt-1]} })

	if out.ExitCode != 0 {
		t.Fatalf("ExitCode = %d, want 0 (the retry's success)", out.ExitCode)
	}
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
	if len(slept) != 1 || slept[0] != 2*time.Second {
		t.Fatalf("slept = %v, want one 2s backoff between the two attempts", slept)
	}
}

// A persistent OOM exhausts the retries and surfaces the 137 (the caller then
// degrades to warn-and-continue / red gate) — with a backoff only *between*
// attempts, never after the last.
func TestRetryOnOOMKill_ExhaustsThenSurfaces137(t *testing.T) {
	var slept []time.Duration
	out, attempts := RetryOnOOMKill(3, time.Second, func(d time.Duration) { slept = append(slept, d) },
		func(attempt int) Outcome { return Outcome{ExitCode: sandbox.ExitOOMKill} })

	if out.ExitCode != sandbox.ExitOOMKill {
		t.Fatalf("ExitCode = %d, want %d (the surfaced OOM)", out.ExitCode, sandbox.ExitOOMKill)
	}
	if attempts != 3 {
		t.Fatalf("attempts = %d, want 3 (all of maxAttempts)", attempts)
	}
	if len(slept) != 2 {
		t.Fatalf("slept %d times, want 2 (between 3 attempts, none after the last)", len(slept))
	}
}

// A real failure (a non-137 code the process itself returned) is final — never
// retried, so a genuine typecheck/test error fails fast.
func TestRetryOnOOMKill_RealFailureNotRetried(t *testing.T) {
	calls := 0
	out, attempts := RetryOnOOMKill(3, time.Second, func(time.Duration) { t.Fatal("must not sleep on a real failure") },
		func(attempt int) Outcome { calls++; return Outcome{ExitCode: 1} })

	if out.ExitCode != 1 || attempts != 1 || calls != 1 {
		t.Fatalf("ExitCode=%d attempts=%d calls=%d, want 1/1/1 (no retry on a real failure)", out.ExitCode, attempts, calls)
	}
}

// A first-attempt success returns immediately with no backoff.
func TestRetryOnOOMKill_SuccessFirstTry(t *testing.T) {
	out, attempts := RetryOnOOMKill(3, time.Second, func(time.Duration) { t.Fatal("must not sleep on first-try success") },
		func(attempt int) Outcome { return Outcome{ExitCode: 0} })

	if out.ExitCode != 0 || attempts != 1 {
		t.Fatalf("ExitCode=%d attempts=%d, want 0/1", out.ExitCode, attempts)
	}
}
