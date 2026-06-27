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
//     it takes precedence over BOTH the exit code AND a present dropbox: the
//     synthetic cap abort replaces a genuine assistant turn, so any out.json that
//     exists alongside it can only be a stale prior-run file or the early `[]` the
//     skill writes before its analysis — never proof the retrospective completed.
//     Checking dropbox-present first would let that file silently mask the abort
//     into a false "ran, found nothing" success and skip the re-run (BEH-568).
//   - exitCode (BEH-536): the retrospective is a long, read-heavy step that hit
//     its wall-clock cap (exit 137) mid-investigation — after the analysis but
//     before its write. A 137 kill with no dropbox is "killed before writing —
//     retry", NOT "never ran" (mirrors ReviewQualitative's OOM branch). Combined
//     with the skill's incremental write, this leaves a clear, actionable signal.
//     Unlike the cap abort, a 137 is checked *after* dropbox-present: a present
//     dropbox there is genuine output written before a teardown kill, so it stands.
//
// Only a genuinely absent-and-not-killed dropbox keeps the "never ran" wording.
func Retrospective(dropboxExists, spendingCapAbort bool, exitCode int) Result {
	if spendingCapAbort {
		return Result{OK: false, Reason: "session aborted before running — spending cap reached, retry after reset"}
	}
	if dropboxExists {
		return Result{OK: true, Reason: "findings dropbox out.json present"}
	}
	if exitCode == oomExitCode {
		return Result{OK: false, Reason: "session killed (exit 137) before writing findings — likely OOM or wall-clock cap, retry"}
	}
	return Result{OK: false, Reason: "findings dropbox out.json was not written — retrospective never ran"}
}

// RetrospectiveInputs is the host-side ground truth that decides whether the
// retrospective has anything to work on at all. Its two fields mirror what the
// /retrospective skill reads: the feature branch (the diff) and the ticket's
// prior implementation/review session transcripts.
type RetrospectiveInputs struct {
	// BranchExists reports whether feat/<slug> resolves to a git revision in the
	// host checkout — i.e. the upstream /tdd step actually produced a branch.
	BranchExists bool
	// PriorTranscripts reports whether at least one implementation-*.jsonl or
	// review-*.jsonl transcript exists under logs/<ticket>/ — i.e. an upstream
	// session actually ran and left something to mine for friction.
	PriorTranscripts bool
}

// RetrospectivePreconditions decides whether it is worth launching the
// retrospective sandbox at all (BEH-553). A retrospective scheduled for a ticket
// whose upstream /tdd + /review steps produced neither a branch nor any session
// transcript has no inputs: it can only emit an empty [] that masks the
// misscheduling, or manufacture a self-referential finding about the missing
// inputs. So when BOTH inputs are absent the caller skips host-side, before
// spending the sandbox cap. OK == true means "proceed"; OK == false means "skip".
//
// The skip is BOTH-absent, deliberately NOT "either is absent" (the literal
// reading of the finding). The pipeline runs the retrospective even on a FAILED
// slice — "exactly the run worth mining for findings" — and a failed slice
// routinely has transcripts but no branch (the session crashed before creating
// the worktree). Skipping whenever the branch is missing would suppress those
// legitimate retrospectives, so a single real input is enough to proceed.
func RetrospectivePreconditions(in RetrospectiveInputs) Result {
	if !in.BranchExists && !in.PriorTranscripts {
		return Result{OK: false, Reason: "no upstream sessions to retrospect — feature branch does not resolve and no implementation/review transcripts exist; the /tdd + /review steps produced nothing"}
	}
	return Result{OK: true, Reason: "upstream inputs present — a feature branch and/or prior session transcripts exist to retrospect"}
}

// ReviewOutcome is the result of the harness's OWN host-side gate re-run after a
// review session plus whether that session actually reviewed — the inputs that
// together authorise a push (never the agent's self-report). GatesGreen is true iff
// `pnpm check && pnpm typecheck` passed in the throwaway container on the feature
// branch (DESIGN.md: ground truth = the harness's own gate run is green). WorktreeClean
// is true iff the worktree had no uncommitted changes when the gate ran. ReviewComplete
// is true iff the in-sandbox /review-worktree session emitted its seven-lens verdict
// (see ReviewQualitative) — a green gate proves the diff compiles but is NOT a review.
type ReviewOutcome struct {
	GatesGreen     bool
	WorktreeClean  bool
	ReviewComplete bool
}

// Review decides whether a reviewed branch may ship. A green gate over a clean
// worktree AND a completed qualitative review clear the push + PR; anything else
// blocks it (no branch reaches a PR on a failing gate or an unran review, and the
// worktree is kept for recovery). Because the inputs are only the harness's own
// gate result, the worktree's git state, and whether the review emitted a verdict,
// a branch can never be pushed on the agent's say-so (AC: no push on self-report).
//
// WorktreeClean is checked first because it qualifies the gate result: the gate
// runs against the worktree's working tree (committed + uncommitted), but the push
// ships only the committed branch tip. A dirty worktree therefore means the gate
// validated a different tree than would ship (e.g. a review session that edited
// but never committed), so its green/red verdict can't be trusted as the push gate.
//
// ReviewComplete is checked last and fails the gate closed (BEH-569): a review
// session killed before its verdict (a spending-cap abort, an OOM) leaves the diff
// with a green gate but ZERO qualitative review. Pushing then opens a PR that no
// one actually reviewed, while the gate re-run masquerades as the review signal.
// Blocking keeps the worktree for a resumed review rather than shipping unreviewed.
func Review(outcome ReviewOutcome) Result {
	if !outcome.WorktreeClean {
		return Result{OK: false, Reason: "worktree has uncommitted changes — the gate validated a different tree than would ship; not pushing"}
	}
	if !outcome.GatesGreen {
		return Result{OK: false, Reason: "harness gate re-run is red — not pushing"}
	}
	if !outcome.ReviewComplete {
		return Result{OK: false, Reason: "qualitative review never produced a verdict — gates green but the seven-lens review did not run; not pushing (fail-closed)"}
	}
	return Result{OK: true, Reason: "harness gate re-run is green and the qualitative review emitted its verdict — clear to push + open PR"}
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
