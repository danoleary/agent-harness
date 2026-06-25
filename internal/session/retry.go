package session

import (
	"time"

	"github.com/beherd/agent-harness/internal/sandbox"
)

// RetryOnOOMKill calls run (passing the 1-based attempt index) and, while the
// container was OOM-killed (exit 137 — sandbox.ExitOOMKill) and attempts remain,
// sleeps the scheduled backoff and retries, up to maxAttempts total. A 137 is
// environmental and transient (BEH-524: the same frozen `pnpm install` that 137'd
// under memory pressure succeeded in ~4s on a bare retry once memory freed), so a
// retry recovers it instead of charging the failure downstream — a prep-install
// OOM otherwise dumps a ~10-min recovery install into the capped review session,
// and a gate-install OOM flips a green branch red with nothing pushed.
//
// backoff is a *schedule* keyed by the just-completed attempt number (1-based):
// after attempt N OOMs, the helper sleeps backoff(N) before attempt N+1. A fixed
// beat (ConstantBackoff) suits a light install whose footprint recovers in
// seconds; the heavier gate build needs ExponentialBackoff so a starved Docker VM
// gets progressively longer to reclaim memory — a fixed 10s was too short and
// memory pressure compounded across back-to-back retries (BEH-530).
//
// Any non-137 outcome — a success or a real failure (a typecheck/test error the
// process itself returned) — returns immediately without retrying. sleep is
// injected so tests need not wait. Returns the final Outcome and the number of
// attempts actually run.
func RetryOnOOMKill(maxAttempts int, backoff func(attempt int) time.Duration, sleep func(time.Duration), run func(attempt int) Outcome) (Outcome, int) {
	var out Outcome
	attempt := 0
	for attempt < maxAttempts {
		attempt++
		out = run(attempt)
		if out.ExitCode != sandbox.ExitOOMKill {
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
