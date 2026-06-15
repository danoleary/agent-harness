// Package verify decides whether a finished tdd session really did its job,
// judged against git ground truth rather than the agent's self-report.
package verify

// GroundTruth is the state of a finished tdd session, gathered from git.
type GroundTruth struct {
	// WorktreeExists reports whether `.claude/worktrees/<slug>` exists.
	WorktreeExists bool
	// CommitsAhead is the number of commits on `feat/<slug>` ahead of origin/main.
	CommitsAhead int
}

// Result is the outcome of checking a tdd session against ground truth.
type Result struct {
	OK     bool
	Reason string
}

// Tdd decides whether a tdd session really did its job. Success requires both a
// worktree and at least one commit on the feature branch — the agent's own
// "I'm done" is never authoritative (DESIGN.md: success is ground-truth).
func Tdd(truth GroundTruth) Result {
	if !truth.WorktreeExists {
		return Result{OK: false, Reason: "worktree was not created"}
	}
	if truth.CommitsAhead < 1 {
		return Result{OK: false, Reason: "no handoff commit ahead of origin/main"}
	}
	return Result{OK: true, Reason: "worktree present and branch is ahead of main"}
}

// Retrospective decides whether a retrospective session really ran, judged by
// the *presence* of its findings dropbox (`/findings/out.json`) rather than the
// agent's self-report. An empty `[]` is a valid "ran, found nothing" (the file
// is still present → success); an absent file means the step never ran and is a
// failure — the rule that stops a silently-skipped retrospective from
// masquerading as "no issues found" (DESIGN.md "Success is ground-truth").
func Retrospective(dropboxExists bool) Result {
	if !dropboxExists {
		return Result{OK: false, Reason: "findings dropbox out.json was not written — retrospective never ran"}
	}
	return Result{OK: true, Reason: "findings dropbox out.json present"}
}

// ReviewOutcome is the result of the harness's OWN host-side gate re-run after a
// review session — the only thing that may authorise a push (never the agent's
// self-report). GatesGreen is true iff `pnpm check && pnpm build` passed in the
// throwaway container on the feature branch (DESIGN.md: ground truth = the
// harness's own gate run is green; this is also the push gate). WorktreeClean is
// true iff the worktree had no uncommitted changes when the gate ran.
type ReviewOutcome struct {
	GatesGreen    bool
	WorktreeClean bool
}

// Review decides whether a reviewed branch may ship. Green gates over a clean
// worktree clear the push + PR; anything else blocks it (no branch reaches a PR
// on a failing gate, and the worktree is kept for recovery). Because the only
// inputs are the harness's own gate result and the worktree's git state, a branch
// can never be pushed on the agent's say-so (AC: no push on self-report).
//
// WorktreeClean is checked first because it qualifies the gate result: the gate
// runs against the worktree's working tree (committed + uncommitted), but the push
// ships only the committed branch tip. A dirty worktree therefore means the gate
// validated a different tree than would ship (e.g. a review session that edited
// but never committed), so its green/red verdict can't be trusted as the push gate.
func Review(outcome ReviewOutcome) Result {
	if !outcome.WorktreeClean {
		return Result{OK: false, Reason: "worktree has uncommitted changes — the gate validated a different tree than would ship; not pushing"}
	}
	if !outcome.GatesGreen {
		return Result{OK: false, Reason: "harness gate re-run is red — not pushing"}
	}
	return Result{OK: true, Reason: "harness gate re-run is green — clear to push + open PR"}
}
