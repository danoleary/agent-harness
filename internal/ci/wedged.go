package ci

import "strings"

// StateExpected is GitHub's commit-status state for a required context that branch
// protection lists but for which no status has ever been reported on the PR head —
// the literal `state` string `gh pr checks --json state` emits ("EXPECTED"). It is
// the structural signature of a merge-queue/main-only required check: a context the
// repo requires, but whose workflow only runs on `merge_group`/`refs/heads/main`
// events, so GitHub never schedules a run for it on the PR and it sits in the pending
// bucket forever. Distinct from a real-but-not-yet-started check, which appears as a
// QUEUED/IN_PROGRESS check run, never as EXPECTED (BEH-614).
const StateExpected = "EXPECTED"

// isWedged reports whether a check is a structurally-wedged required context: pending
// with state EXPECTED. Such a check will never move on the PR head (it only runs in
// the merge queue / on main), so polling it to a terminal state is futile.
func isWedged(c Check) bool {
	return c.Bucket == BucketPending && strings.EqualFold(c.State, StateExpected)
}

// wedgedReady reports whether the only thing keeping CI from a terminal pass is one or
// more structurally-wedged required contexts (isWedged) — i.e. every real gate has gone
// green and all that remains pending is a merge-queue/main-only EXPECTED context. Such
// a PR is done and ready for the merge queue; watching it any longer just burns the
// poll budget on a check that will never run on the PR head (BEH-614).
//
// The "at least one real green check" guard is the safety against a false positive at
// PR-open: before any workflow has run, the required contexts can momentarily appear as
// EXPECTED with nothing yet green, and a real check that is going to run shows as a
// genuinely-pending (QUEUED/IN_PROGRESS) check run — both make this false, so the watch
// keeps polling until the real gates actually settle green. Only once they have, with
// just the EXPECTED wedge left, does the PR count as ready.
func wedgedReady(checks []Check) bool {
	wedged, realGreen := false, false
	for _, c := range checks {
		if isWedged(c) {
			wedged = true
			continue
		}
		switch c.Bucket {
		case BucketPass, BucketSkipping:
			realGreen = true
		default:
			// A non-wedged check is failing, cancelled, or genuinely still running —
			// not ready; let the normal classify/poll logic handle it.
			return false
		}
	}
	return wedged && realGreen
}

// hasRealPending reports whether at least one check is a real (non-EXPECTED) gate
// still in flight — pending, but not a structurally-wedged merge-queue/main-only
// context (isWedged). It is the signal the adaptive budget extension keys off (BEH-685):
// while a genuine required gate is still running (e.g. the browser-backed linting_and_tests,
// which can outlast the soft poll budget), the poll keeps waiting past the soft budget up to
// the hard ceiling rather than abandoning an about-to-go-green PR. Returns false for an
// empty/all-terminal set, and for a frozen set whose only pending checks are EXPECTED wedges
// (there the wedgedReady/stall paths, not the extension, decide the outcome).
func hasRealPending(checks []Check) bool {
	for _, c := range checks {
		if c.Bucket == BucketPending && !isWedged(c) {
			return true
		}
	}
	return false
}

// pendingAllWedged reports whether every still-pending check is a structural wedge
// (isWedged) — i.e. nothing genuinely running remains. It is the guard the stall
// detector uses before bailing a frozen-pending run as ErrPollStalled: the no-
// progress signal alone cannot tell a wedged merge-queue/main-only context apart
// from a real gate that is simply slow (e.g. "Linting and tests" running ~5min),
// because both sit in the `pending` bucket with an unchanging (name,bucket)
// signature for the whole run. Only when the frozen pending set is entirely
// EXPECTED wedges is the freeze genuinely terminal; if even one real run
// (QUEUED/IN_PROGRESS, not EXPECTED) is still in flight, the freeze is a slow-but-
// progressing gate and abandoning it would route an about-to-go-green PR to manual
// triage (BEH-623, the BEH-508 waste). Returns false for an empty/all-terminal set
// (no pending check to be wedged on).
func pendingAllWedged(checks []Check) bool {
	sawPending := false
	for _, c := range checks {
		if c.Bucket != BucketPending {
			continue
		}
		sawPending = true
		if !isWedged(c) {
			return false
		}
	}
	return sawPending
}
