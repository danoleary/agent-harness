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
