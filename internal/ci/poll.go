package ci

import (
	"errors"
	"sort"
	"strings"
	"time"
)

// ErrPollTimeout is returned when CI checks never reach a terminal state within
// the poll budget — the run is wedged/stuck pending, distinct from a clean
// pass or fail. The caller treats it as a non-success that keeps the PR.
var ErrPollTimeout = errors.New("ci checks did not reach a terminal state within the poll budget")

// ErrPollStalled is returned when the check set stops changing for the stall
// window while still pending — the live run has settled but a context is wedged
// pending (the classic merge-queue / branch-protection trap: a job gated on
// `merge_group` or `refs/heads/main` is reported as an expected-but-never-run
// check on the PR, so it sits in the pending bucket forever). It is a faster,
// more-informative variant of ErrPollTimeout: same non-success outcome (keep the
// PR for a human), but it bails as soon as progress stops rather than burning the
// full budget on a check that will never move on a PR.
var ErrPollStalled = errors.New("ci checks stalled while still pending (no progress within the stall window)")

// pollConfig frames one poll: how often to re-check, the overall wall-clock cap,
// and the no-progress (stall) window after which an unchanging-but-still-pending
// check set is treated as wedged. A zero stall disables stall detection (budget
// is then the only bound), which keeps the older callers/tests that don't set it
// behaving exactly as before.
type pollConfig struct {
	interval time.Duration
	budget   time.Duration
	stall    time.Duration
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
	// Stall tracking: the signature of the last pending snapshot and the time it
	// was first observed. While the snapshot keeps changing (jobs flipping
	// pending→pass as the run progresses) stalledSince keeps resetting, so a
	// genuinely-progressing run is never cut short. Once it freezes, the elapsed
	// time accumulates toward cfg.stall.
	var lastSig string
	var stalledSince time.Time
	haveSig := false
	for {
		checks, err := fetch()
		switch {
		case err == nil:
			if v := Classify(checks); v != Pending {
				return v, checks, nil
			}
			// pending → stall/budget checks below
		case errors.Is(err, errNoChecksYet):
			// Checks not registered yet (just after the PR opened) — treat like
			// pending and keep waiting; drop the empty checks so a timeout reports
			// nothing rather than a stale set.
			checks = nil
		default:
			return Pending, nil, err
		}

		// Still pending. Detect a stalled run: if the check set has not changed for
		// the stall window, a context is wedged pending and will not move on this PR
		// (a merge-queue/main-only job reported as expected-but-unrun). Bail now
		// instead of waiting out the full budget on it.
		if cfg.stall > 0 {
			sig := checksSignature(checks)
			if !haveSig || sig != lastSig {
				lastSig, stalledSince, haveSig = sig, now(), true
			} else if !now().Before(stalledSince.Add(cfg.stall)) {
				return Pending, checks, ErrPollStalled
			}
		}

		// Stop if the next interval would carry us past the budget (no point
		// sleeping toward a deadline we can't beat).
		if !now().Add(cfg.interval).Before(deadline) {
			return Pending, checks, ErrPollTimeout
		}
		sleep(cfg.interval)
	}
}

// checksSignature is an order-independent fingerprint of a pending snapshot —
// each check's name+bucket, sorted — so stall detection keys off real changes
// (a job flipping bucket, appearing, or disappearing) and not gh's arbitrary
// ordering between polls.
func checksSignature(checks []Check) string {
	parts := make([]string, len(checks))
	for i, c := range checks {
		parts[i] = c.Name + "\x00" + c.Bucket
	}
	sort.Strings(parts)
	return strings.Join(parts, "\x1f")
}
