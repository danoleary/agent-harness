package session

import (
	"time"
)

// RetryTransient calls run (passing the 1-based attempt index) and, while the
// outcome is an environmental, transient failure (Outcome.Retryable — the 137
// OOM-kill of BEH-524 or a transient exit-125 launch failure, overlay2/read-only-fs
// of BEH-542) and attempts remain, sleeps the scheduled backoff and retries, up to
// maxAttempts total. Both classes are the host momentarily wedging, not a code
// fault: the same `docker run` / `pnpm install` succeeds on a bare retry once the
// host recovers — so a retry recovers it instead of charging the failure
// downstream (a prep-install OOM otherwise dumps a ~10-min recovery install into
// the capped review session; a worktree-creation 125 otherwise discards the whole
// ticket — BEH-543).
//
// backoff is a *schedule* keyed by the just-completed attempt number (1-based):
// after attempt N fails transiently, the helper sleeps backoff(N) before attempt
// N+1. A fixed beat (ConstantBackoff) suits a light install whose footprint
// recovers in seconds; the heavier gate build needs ExponentialBackoff so a
// starved Docker VM gets progressively longer to reclaim memory — a fixed 10s was
// too short and memory pressure compounded across back-to-back retries (BEH-530).
//
// Any non-retryable outcome — a success or a real failure (a typecheck/test error
// the process itself returned, or a genuine 125 such as a daemon-down) — returns
// immediately without retrying. sleep is injected so tests need not wait. Returns
// the final Outcome and the number of attempts actually run.
func RetryTransient(maxAttempts int, backoff func(attempt int) time.Duration, sleep func(time.Duration), run func(attempt int) Outcome) (Outcome, int) {
	var out Outcome
	attempt := 0
	for attempt < maxAttempts {
		attempt++
		out = run(attempt)
		if !out.Retryable() {
			return out, attempt
		}
		if attempt < maxAttempts {
			sleep(backoff(attempt))
		}
	}
	return out, attempt
}

// ConstantBackoff is a fixed-duration retry schedule — the same beat between every
// attempt, regardless of attempt number.
func ConstantBackoff(d time.Duration) func(attempt int) time.Duration {
	return func(int) time.Duration { return d }
}

// ExponentialBackoff doubles `base` each retry (base, 2×base, 4×base, …), capped
// at `maxWait`. The doubling gives a memory-starved Docker VM progressively longer
// to reclaim between gate retries (BEH-530); the cap bounds the wait (and sidesteps
// shift overflow on large attempt numbers).
func ExponentialBackoff(base, maxWait time.Duration) func(attempt int) time.Duration {
	return func(attempt int) time.Duration {
		d := base
		for i := 1; i < attempt; i++ {
			d *= 2
			if d >= maxWait {
				return maxWait
			}
		}
		if d > maxWait {
			return maxWait
		}
		return d
	}
}
