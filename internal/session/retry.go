package session

import (
	"time"

	"github.com/beherd/agent-harness/internal/sandbox"
)

// RetryOnOOMKill calls run (passing the 1-based attempt index) and, while the
// container was OOM-killed (exit 137 — sandbox.ExitOOMKill) and attempts remain,
// sleeps `backoff` and retries, up to maxAttempts total. A 137 is environmental
// and transient (BEH-524: the same frozen `pnpm install` that 137'd under memory
// pressure succeeded in ~4s on a bare retry once memory freed), so a short retry
// recovers it instead of charging the failure downstream — a prep-install OOM
// otherwise dumps a ~10-min recovery install into the capped review session, and
// a gate-install OOM flips a green branch red with nothing pushed.
//
// Any non-137 outcome — a success or a real failure (a typecheck/test error the
// process itself returned) — returns immediately without retrying. sleep is
// injected so tests need not wait. Returns the final Outcome and the number of
// attempts actually run.
func RetryOnOOMKill(maxAttempts int, backoff time.Duration, sleep func(time.Duration), run func(attempt int) Outcome) (Outcome, int) {
	var out Outcome
	attempt := 0
	for attempt < maxAttempts {
		attempt++
		out = run(attempt)
		if out.ExitCode != sandbox.ExitOOMKill {
			return out, attempt
		}
		if attempt < maxAttempts {
			sleep(backoff)
		}
	}
	return out, attempt
}
