package ci

import (
	"errors"
	"time"
)

// ErrPollTimeout is returned when CI checks never reach a terminal state within
// the poll budget — the run is wedged/stuck pending, distinct from a clean
// pass or fail. The caller treats it as a non-success that keeps the PR.
var ErrPollTimeout = errors.New("ci checks did not reach a terminal state within the poll budget")

// pollConfig frames one poll: how often to re-check and the wall-clock cap.
type pollConfig struct {
	interval time.Duration
	budget   time.Duration
}

// poll re-runs fetch until its checks reach a terminal verdict (Passed/Failed),
// sleeping `interval` between pending re-checks, bounded by `budget`. It returns
// the terminal verdict and the checks that produced it. A fetch error aborts
// immediately (a real gh failure — auth, bad PR — not something more polling
// fixes). If the budget is spent while still pending it returns (Pending, last
// checks, ErrPollTimeout). The clock is injected so the timing is deterministic
// in tests (mirrors git.withRetry).
func poll(fetch func() ([]Check, error), cfg pollConfig, sleep func(time.Duration), now func() time.Time) (Verdict, []Check, error) {
	deadline := now().Add(cfg.budget)
	for {
		checks, err := fetch()
		switch {
		case err == nil:
			if v := Classify(checks); v != Pending {
				return v, checks, nil
			}
			// pending → wait below
		case errors.Is(err, errNoChecksYet):
			// Checks not registered yet (just after the PR opened) — treat like
			// pending and keep waiting; drop the empty checks so a timeout reports
			// nothing rather than a stale set.
			checks = nil
		default:
			return Pending, nil, err
		}
		// Still pending — stop if the next interval would carry us past the
		// budget (no point sleeping toward a deadline we can't beat).
		if !now().Add(cfg.interval).Before(deadline) {
			return Pending, checks, ErrPollTimeout
		}
		sleep(cfg.interval)
	}
}
