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

// ErrWedgedReadyForMergeQueue is returned the instant a poll snapshot is wedged-ready
// (wedgedReady): every real gate is green and the only thing still pending is a
// structurally-wedged required context — a merge-queue/main-only check GitHub lists as
// EXPECTED but never schedules a run for on the PR head. Unlike ErrPollStalled (a
// non-success a human must triage), this is a SUCCESS the caller maps to a green-
// equivalent "ready for the merge queue" pass: the PR has validated everything that
// will ever run on it. It short-circuits on the first such poll rather than burning the
// stall window or budget on a check that will never move on the PR (BEH-614).
var ErrWedgedReadyForMergeQueue = errors.New("ci ready for merge queue (real gates green; only a structurally-wedged required context remains pending)")

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
			// Pending. Before waiting, check for the structural wedge: if every real
			// gate is green and the only thing left pending is a merge-queue/main-only
			// EXPECTED context, the PR is ready for the merge queue — short-circuit now
			// rather than burn the stall window/budget on a check that will never move
			// on the PR head (BEH-614).
			if wedgedReady(checks) {
				return Pending, checks, ErrWedgedReadyForMergeQueue
			}
			// otherwise → stall/budget checks below
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
		// instead of waiting out the full budget on it. But only once the snapshot
		// carries stall evidence (stallEvidence): an all-pending snapshot with nothing
		// settled and no EXPECTED mark is a slow CI cold start — the runners simply have
		// not picked the jobs up yet — and is byte-for-byte identical in signature to a
		// real wedge, so firing on it abandons a PR that would go green on its own
		// (BEH-620). Until real progress or an EXPECTED context appears, keep polling to
		// the budget.
		if cfg.stall > 0 && stallEvidence(checks) {
			sig := checksSignature(checks)
			if !haveSig || sig != lastSig {
				lastSig, stalledSince, haveSig = sig, now(), true
			} else if !now().Before(stalledSince.Add(cfg.stall)) && pendingAllWedged(checks) {
				// Only bail as wedged once the frozen pending set is *entirely* EXPECTED
				// wedges. A still-running real gate (pending, not EXPECTED) holds the same
				// signature for its whole run, so a freeze alone is not proof of a wedge —
				// keep polling to the budget rather than abandoning an about-to-go-green PR
				// (BEH-623).
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

// stallEvidence reports whether a frozen pending snapshot carries enough signal to
// be treated as a genuine stall rather than a slow CI cold start. A real wedge always
// leaves one of two marks: at least one real (non-EXPECTED) gate has already settled
// into a terminal bucket — typically a green pass/skip, since a fail would have
// classified the whole run already — or a structurally-wedged EXPECTED context is
// present (a merge-queue/main-only required check that will never run on the PR head).
// An all-pending snapshot with neither mark is a queue that simply has not picked the
// jobs up yet; freezing on it is a slow start, not a wedge, so the poller keeps waiting
// to the budget instead of abandoning a PR that would go green on its own (BEH-620).
// This mirrors the evidence wedgedReady requires before short-circuiting a clean wedge.
func stallEvidence(checks []Check) bool {
	for _, c := range checks {
		if isWedged(c) || c.Bucket != BucketPending {
			return true
		}
	}
	return false
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
