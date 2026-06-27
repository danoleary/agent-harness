// Package verify decides whether a finished tdd session really did its job,
// judged against git ground truth rather than the agent's self-report.
package verify

import "fmt"

// oomExitCode is the container exit code for a SIGKILL (128+9), which the sandbox
// memory-pressure OOM-killer produces — the kill that struck the BEH-499 review
// session mid-gate (BEH-407/477/491/519 document the same class for build/typecheck).
const oomExitCode = 137

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
//
// Two parameters distinguish the failure class when the dropbox is absent, so a
// kill the agent couldn't avoid isn't mislabelled as the agent skipping the step:
//
//   - spendingCapAbort (BEH-494): a session killed by a billing/usage cap before
//     doing any work never gets the chance to write the dropbox, so the generic
//     "never ran" reads as the agent misbehaving. When the cap fired, report the
//     distinct retry-after-reset class instead. It is the most specific cause, so
//     it takes precedence over the exit code.
//   - exitCode (BEH-536): the retrospective is a long, read-heavy step that hit
//     its wall-clock cap (exit 137) mid-investigation — after the analysis but
//     before its write. A 137 kill with no dropbox is "killed before writing —
//     retry", NOT "never ran" (mirrors ReviewQualitative's OOM branch). Combined
//     with the skill's incremental write, this leaves a clear, actionable signal.
//
// Only a genuinely absent-and-not-killed dropbox keeps the "never ran" wording.
func Retrospective(dropboxExists, spendingCapAbort bool, exitCode int) Result {
	if dropboxExists {
		return Result{OK: true, Reason: "findings dropbox out.json present"}
	}
	if spendingCapAbort {
		return Result{OK: false, Reason: "session aborted before running — spending cap reached, retry after reset"}
	}
	if exitCode == oomExitCode {
		return Result{OK: false, Reason: "session killed (exit 137) before writing findings — likely OOM or wall-clock cap, retry"}
	}
	return Result{OK: false, Reason: "findings dropbox out.json was not written — retrospective never ran"}
}

// RetrospectiveHasInputs is the *pre*condition counterpart to Retrospective's
// post-check: it decides whether launching the retrospective sandbox is worth it
// at all, before any container starts. The session reads a ticket's prior
// implementation/review transcripts and studies the diff on its feature branch; if
// there is no transcript to read AND/OR no branch to resolve, it can only conclude
// "nothing to read" — so the harness should skip it rather than burn a full sandbox
// launch to rediscover that (BEH-552: a retrospective was dispatched for BEH-318,
// whose log dir held only the retro's own stream and whose feat/ branch never
// existed).
//
// Both inputs are required to proceed. In the normal pipeline both always hold by
// the time the retrospective runs — the implementation session creates feat/<slug>
// at step 0 and tees its transcript — so a skip only fires on an anomalous
// standalone dispatch against a ticket whose pipeline produced no artifacts. A
// genuinely-failed slice (implementation died after creating the branch) keeps both
// inputs and is still retrospected: that failure is exactly what's worth mining.
//
// OK == true means "inputs present, launch". When OK is false the Reason names
// every missing input so the skip diagnostic is unambiguous.
func RetrospectiveHasInputs(hasTranscripts, branchResolves bool) Result {
	switch {
	case hasTranscripts && branchResolves:
		return Result{OK: true, Reason: "prior pipeline transcripts present and feature branch resolves"}
	case !hasTranscripts && !branchResolves:
		return Result{OK: false, Reason: "no prior implementation/review transcripts and no feature branch — nothing to study"}
	case !hasTranscripts:
		return Result{OK: false, Reason: "no prior implementation/review transcripts to study"}
	default:
		return Result{OK: false, Reason: "feature branch does not resolve — no diff to study"}
	}
}

// ReviewOutcome is the result of the harness's OWN host-side gate re-run after a
// review session — the only thing that may authorise a push (never the agent's
// self-report). GatesGreen is true iff `pnpm check && pnpm typecheck` passed in the
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

// ReviewCompleteness reports whether the in-sandbox /review-worktree session
// actually performed its qualitative seven-lens pass. It is deliberately separate
// from Review (the push gate): the push is authorised by the harness's own host-
// side gate re-run, but a green gate only proves the diff compiles/lints — it says
// nothing about whether the human-style review ran. Both must be reported so a
// review killed before its verdict isn't silently treated as a full review pass.
type ReviewCompleteness struct {
	Complete bool
	Reason   string
}

// ReviewQualitative classifies whether the review session emitted its verdict —
// the "## Review:" report that ends the seven-lens pass (BEH-525). The verdict is
// the only proof the lenses ran, so its presence means complete regardless of how
// the container exited; its absence means incomplete regardless of a green host-
// side gate. The OOM case (exit 137) is the one the BEH-499 review hit — killed
// mid-gate before reaching the report — so it gets a distinct, named reason; any
// other end before the verdict is reported with its exit code but not mislabelled
// as an OOM.
func ReviewQualitative(exitCode int, verdictEmitted bool) ReviewCompleteness {
	if verdictEmitted {
		return ReviewCompleteness{Complete: true, Reason: "review session emitted its seven-lens verdict"}
	}
	if exitCode == oomExitCode {
		return ReviewCompleteness{Complete: false, Reason: "review session OOM-killed (exit 137) before emitting a verdict — gates green but qualitative review incomplete"}
	}
	return ReviewCompleteness{Complete: false, Reason: fmt.Sprintf("review session exited %d before emitting a verdict — qualitative review incomplete", exitCode)}
}
