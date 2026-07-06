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
	// DisjointHistory reports whether `feat/<slug>` shares NO common ancestor with
	// origin/main (an empty `git merge-base`). A disjoint branch is "ahead" by all
	// of its own commits (963 in BEH-355), so it passes the CommitsAhead check, yet
	// it is never a real single-ticket handoff and its rebase collides immediately.
	DisjointHistory bool
}

// Result is the outcome of checking a tdd session against ground truth.
type Result struct {
	OK     bool
	Reason string
	// RecommendClose marks the BEH-603 no-op disposition: a clean, gate-green,
	// reviewed branch whose committed tip makes zero net change against origin/main
	// (an empty `git diff origin/main`). It is NOT a push (OK stays false — there is
	// nothing to ship) and NOT a plain failure — the agents correctly concluded the
	// ticket should be closed as a duplicate/superseded. Only Review sets it; it is
	// false everywhere else.
	RecommendClose bool
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
	// A disjoint history (no common ancestor with origin/main) is checked AFTER the
	// commits-ahead gate because a disjoint branch always reads as ahead — by all of
	// its own commits. It is a suspect ground truth, never a healthy handoff: the
	// pre-push rebase would try to replay every disjoint commit onto main and collide
	// immediately, and the BEH-355 incident showed it masquerading as a 963-commits-
	// ahead success. Fail it here so the slice never reaches the push path (BEH-597).
	if truth.DisjointHistory {
		return Result{OK: false, Reason: "branch shares no common ancestor with origin/main (disjoint history / empty merge-base) — a suspect ground truth, not a real handoff"}
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
// Three signals distinguish the failure class when the dropbox is absent, so a
// crash the agent couldn't avoid isn't mislabelled as the agent skipping the step:
//
//   - SpendingCapAbort (BEH-494): a session killed by a billing/usage cap before
//     doing any work never gets the chance to write the dropbox, so the generic
//     "never ran" reads as the agent misbehaving. When the cap fired, report the
//     distinct retry-after-reset class instead. It is the most specific cause, so
//     it takes precedence over BOTH the exit code AND a present dropbox: the
//     synthetic cap abort replaces a genuine assistant turn, so any out.json that
//     exists alongside it can only be a stale prior-run file or the early `[]` the
//     skill writes before its analysis — never proof the retrospective completed.
//     Checking dropbox-present first would let that file silently mask the abort
//     into a false "ran, found nothing" success and skip the re-run (BEH-568).
//   - ExitCode (BEH-536): the retrospective is a long, read-heavy step that hit
//     its wall-clock cap (exit 137) mid-investigation — after the analysis but
//     before its write. A 137 kill with no dropbox is "killed before writing —
//     retry", NOT "never ran" (mirrors ReviewQualitative's OOM branch). Combined
//     with the skill's incremental write, this leaves a clear, actionable signal.
//     Unlike the cap abort, a 137 is checked *after* dropbox-present: a present
//     dropbox there is genuine output written before a teardown kill, so it stands.
//   - TurnZeroNoOp (BEH-709): the session exited 0 with is_error=false but did zero
//     real work — the pinned CLI's turn-0 no-op, where a `!`+backtick directive in
//     the prompt errored host-of-sandbox and degenerated the whole session before
//     it could write. Its exit-0/success shape otherwise falls through to the plain
//     "never ran", masking a genuine crash as the agent skipping the step; naming it
//     distinctly (and as a retry, since the trigger is content/parse nondeterministic)
//     stops that. Checked after the 137 kill: a turn-0 no-op is exit 0 by definition,
//     so the two never collide.
//
// Only a genuinely absent-and-not-crashed dropbox keeps the "never ran" wording.
func Retrospective(o RetrospectiveOutcome) Result {
	if o.SpendingCapAbort {
		return Result{OK: false, Reason: "session aborted before running — spending cap reached, retry after reset"}
	}
	if o.DropboxExists {
		return Result{OK: true, Reason: "findings dropbox out.json present"}
	}
	if o.ExitCode == oomExitCode {
		return Result{OK: false, Reason: "session killed (exit 137) before writing findings — likely OOM or wall-clock cap, retry"}
	}
	if o.TurnZeroNoOp {
		return Result{OK: false, Reason: "session turn-0-crashed (exited 0 with no output after ≤3 turns) before writing findings — the prompt likely poisoned the session, retry"}
	}
	return Result{OK: false, Reason: "findings dropbox out.json was not written — retrospective never ran"}
}

// RetrospectiveOutcome is the ground truth the retrospective completion check reads
// to decide whether the session really ran and, if not, which failure class it was.
// It mirrors what retrospective.go knows right after the session and its dropbox
// write (BEH-709 added TurnZeroNoOp to the pre-existing dropbox/cap/exit signals).
type RetrospectiveOutcome struct {
	// DropboxExists reports whether /findings/out.json is present — the ground-truth
	// proof the retrospective ran (an empty `[]` still counts).
	DropboxExists bool
	// SpendingCapAbort marks a session killed by a billing/usage cap before doing any
	// work — its own retry-after-reset class.
	SpendingCapAbort bool
	// ExitCode is the session's container exit code (137 = OOM / wall-clock kill).
	ExitCode int
	// TurnZeroNoOp marks the pinned CLI's turn-0 no-op (BEH-709): exit 0 with
	// is_error=false but zero real work, because the prompt's `!`+backtick directive
	// crashed the session before it could write findings.
	TurnZeroNoOp bool
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
// every config-declared named gate (herd: `pnpm run check`, `pnpm run typecheck`;
// BEH-634) passed in the throwaway containers on the feature branch (DESIGN.md:
// ground truth = the harness's own gate run is green). WorktreeClean
// is true iff the worktree had no uncommitted changes when the gate ran. ReviewComplete
// is true iff the in-sandbox /review-worktree session emitted its seven-lens verdict
// (see ReviewQualitative) — a green gate proves the diff compiles but is NOT a review.
type ReviewOutcome struct {
	GatesGreen     bool
	WorktreeClean  bool
	ReviewComplete bool
	// ReviewBlocked is true iff the review session emitted its verdict but declared
	// a blocked disposition (BEH-580) — an unresolved Blocker/Important finding it
	// could not autonomously resolve. A blocked review is *complete* (the lenses
	// ran), so ReviewComplete is also true; the two are distinct signals.
	ReviewBlocked bool
	// EmptyDiff is true iff the branch's committed tip makes zero net change against
	// origin/main (an empty `git diff origin/main` — BEH-603). It drives the
	// recommend-close disposition: a clean, gate-green, reviewed branch with nothing
	// to ship should have its ticket closed as a duplicate/superseded, not opened as
	// an empty-commit PR.
	EmptyDiff bool
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
// EmptyDiff is checked next — after the clean-tree gate, before the gate/review
// checks (BEH-603). A clean worktree whose committed tip makes zero net change
// against origin/main has nothing to ship: the gate is trivially green and any
// review was over an empty diff. Rather than open the empty-commit PR the harness
// previously did (PR #642), Review returns the recommend-close disposition
// (OK false, RecommendClose true) so the caller surfaces a handoff for a human to
// close the ticket as a duplicate/superseded. Checking it after WorktreeClean keeps
// a dirty tree — where the real change may still be uncommitted — from ever
// recommending the close of a live ticket.
//
// ReviewComplete is checked before ReviewBlocked and fails the gate closed
// (BEH-569): a review session killed before its verdict (a spending-cap abort, an
// OOM) leaves the diff with a green gate but ZERO qualitative review. Pushing then
// opens a PR that no one actually reviewed, while the gate re-run masquerades as the
// review signal. Blocking keeps the worktree for a resumed review rather than
// shipping unreviewed.
//
// ReviewBlocked is checked last and also fails closed (BEH-580): a review that DID
// run but declared a blocked disposition found a Blocker/Important finding it could
// not autonomously resolve. The autonomous pipeline has no human to answer the
// skill's approval prompt, so pushing would open a PR with the finding unaddressed
// (the BEH-439 leak). Blocking keeps the worktree for a human decision — distinct
// from the BEH-569 case (there the review never ran; here it ran and found a real,
// unresolved issue).
func Review(outcome ReviewOutcome) Result {
	if !outcome.WorktreeClean {
		return Result{OK: false, Reason: "worktree has uncommitted changes — the gate validated a different tree than would ship; not pushing"}
	}
	// Empty-diff recommend-close (BEH-603) is checked right after the clean-tree gate
	// and before the gate/review checks: on a clean worktree with zero net diff there
	// is simply nothing to ship, so the gate is trivially green and any review was over
	// an empty diff — recommend-close is the correct terminal outcome regardless. It is
	// checked AFTER WorktreeClean so a dirty tree (the real change may be uncommitted)
	// never recommends closing a live ticket.
	if outcome.EmptyDiff {
		return Result{OK: false, RecommendClose: true, Reason: "branch makes zero net change against origin/main (empty diff) — nothing to ship; recommend closing the ticket as a duplicate/superseded rather than opening an empty-commit PR"}
	}
	if !outcome.GatesGreen {
		return Result{OK: false, Reason: "harness gate re-run is red — not pushing"}
	}
	if !outcome.ReviewComplete {
		return Result{OK: false, Reason: "qualitative review never produced a verdict — gates green but the seven-lens review did not run; not pushing (fail-closed)"}
	}
	if outcome.ReviewBlocked {
		return Result{OK: false, Reason: "review verdict declared a blocked disposition — an unresolved blocker/important finding needs a human decision; not pushing (fail-closed)"}
	}
	return Result{OK: true, Reason: "harness gate re-run is green and the qualitative review emitted a clear verdict — clear to push + open PR"}
}

// PostRebasePush is the second empty-diff gate, checked AFTER the pre-push rebase
// replays the branch onto the latest origin/main and BEFORE the push + `gh pr create`
// (BEH-680). The BEH-603 EmptyDiff check in Review runs on the pre-rebase tree, so it
// cannot see a branch that collapses to zero net change DURING the replay: a sibling
// PR merged the same fix while the multi-minute gate ran, or the BEH-581
// conflict-resolution session skipped a now-empty commit, leaving the branch
// identical to origin/main. Pushing that branch and running `gh pr create` hard-fails
// with "No commits between main and feat/…" — a wasted push, a hard error, and a
// misleading "open the PR manually" hint for a branch that has nothing to open a PR
// for. When emptyDiff is true, route to the same recommend-close disposition Review
// uses (OK false, RecommendClose true) so the ticket is handed off for a human to
// close as superseded rather than pushed. A non-empty branch is the normal ship path
// (OK true), clearing the push. emptyDiff is read host-side by BranchDiffEmpty after
// the rebase succeeds; a clean worktree is the rebase precondition, so — unlike
// Review — no WorktreeClean re-check is needed here.
func PostRebasePush(emptyDiff bool) Result {
	if emptyDiff {
		return Result{OK: false, RecommendClose: true, Reason: "rebase collapsed the branch to zero net change against origin/main (empty diff) — nothing to ship; recommend closing the ticket as superseded rather than pushing an empty branch that `gh pr create` cannot open"}
	}
	return Result{OK: true, Reason: "branch still carries a real diff after the rebase — clear to push + open PR"}
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

// ReviewRetryInputs is the ground truth that decides whether a review session which
// ended without a verdict should be re-launched in-stage over the same unchanged
// worktree (BEH-624). It mirrors what review.go knows right after the session and
// its host-side gate re-run.
type ReviewRetryInputs struct {
	// VerdictEmitted is true iff the session already produced its "## Review:"
	// verdict — there is nothing to re-launch for.
	VerdictEmitted bool
	// ExitCode is the review session's container exit code.
	ExitCode int
	// SpendingCapAbort is true iff a billing/usage cap killed the session before it
	// did any work — its own retry-after-reset class, handled separately.
	SpendingCapAbort bool
	// WorktreeClean is true iff the worktree had no uncommitted changes after the
	// session — the gate validated exactly the committed tip that would ship.
	WorktreeClean bool
	// GatesGreen is true iff the harness's own host-side gate re-run passed.
	GatesGreen bool
}

// ReviewRetryDecision is ReviewVerdictRetry's verdict: whether to re-launch the
// review session, and why (for the run log).
type ReviewRetryDecision struct {
	Retry  bool
	Reason string
}

// ReviewVerdictRetry decides whether a review session that ended WITHOUT its verdict
// is eligible for a bounded in-stage re-launch over the same unchanged worktree
// (BEH-624) — the one incompleteness class that previously had no in-stage recovery
// and instead discarded a fully verified, gate-green diff to be re-reviewed from
// scratch on a later whole-pipeline dispatch.
//
// It is the cheapest case to retry, so the bar is deliberately narrow: re-launch
// ONLY when the diff is already proven good and the review merely stopped a turn
// short of printing the verdict. Every other condition routes elsewhere:
//
//   - VerdictEmitted → there is nothing to retry (handled by ReviewQualitative).
//   - A non-zero ExitCode (an OOM 137, a crash) is NOT this class — it is the
//     killed-before-verdict case ReviewQualitative already names; re-launching over a
//     possibly-corrupt run is not the cheap byte-identical retry this targets.
//   - SpendingCapAbort is its own retry-after-reset class (the caller defers until
//     the cap resets), never an in-stage re-launch that would burn the same cap.
//   - A dirty worktree means the gate validated a different tree than would ship, so
//     the diff is not the proven-good artifact this retry assumes.
//   - Red gates mean the diff itself is broken — re-running the review can't make it
//     shippable, so it falls through to fail-closed with the worktree kept.
func ReviewVerdictRetry(in ReviewRetryInputs) ReviewRetryDecision {
	if in.VerdictEmitted {
		return ReviewRetryDecision{Retry: false, Reason: "review already emitted its verdict — nothing to re-launch"}
	}
	if in.ExitCode == 0 && !in.SpendingCapAbort && in.WorktreeClean && in.GatesGreen {
		return ReviewRetryDecision{Retry: true, Reason: "review exited cleanly (code 0) one turn short of its verdict over a clean, gate-green worktree — re-launching to reach the verdict (BEH-624)"}
	}
	return ReviewRetryDecision{Retry: false, Reason: "review without a verdict is not the cheap clean-exit case (non-zero exit, spending-cap abort, dirty worktree, or red gate) — falling through to fail-closed"}
}

// RebaseResolutionOutcome is the git ground truth a sandboxed pre-push
// conflict-resolution session left behind (BEH-581), the inputs that decide
// whether the rebased branch may proceed to a re-gate + push. SessionExit is the
// resolution session's container exit code; SpendingCapAbort marks the session
// killed by an active billing cap before it could resolve anything; WorktreeClean
// is true iff the worktree had no uncommitted changes afterwards; Rebased is true
// iff origin/main is now an ancestor of the branch tip (the rebase actually
// landed, vs. a session that gave up and `rebase --abort`ed back to the stale tip).
type RebaseResolutionOutcome struct {
	SessionExit      int
	SpendingCapAbort bool
	WorktreeClean    bool
	Rebased          bool
}

// RebaseResolutionResult is RebaseResolution's verdict. SpendingCapAbort
// distinguishes the retry-after-reset class (the session never ran) so the caller
// defers quietly instead of filing a "couldn't resolve the conflict" breadcrumb.
type RebaseResolutionResult struct {
	OK               bool
	Reason           string
	SpendingCapAbort bool
}

// RebaseResolution decides whether a sandboxed conflict-resolution session
// actually rebased the branch onto origin/main cleanly — the gate on whether the
// pre-push path re-gates + pushes, or keeps the worktree for a human (BEH-581).
// Like every other harness gate it is judged on ground truth, never the agent's
// self-report: a green verdict requires a clean session exit, a clean worktree,
// AND the branch genuinely rebased onto base.
//
// Order matters. A spending-cap abort wins outright — the session took no real
// action, so the worktree's incidental state says nothing, and it is the one
// failure that must NOT surface as a content conflict (it just retries after the
// cap resets). A non-zero exit is next (the session crashed/timed out). Then a
// dirty worktree (unresolved or uncommitted conflict — the more actionable signal
// than the not-rebased one that also holds when dirty). Finally the silent trap
// the whole guard exists for: a CLEAN worktree still on the stale base, because
// the session resolved nothing and `rebase --abort`ed — WorktreeClean alone would
// wave that through and push a still-stale branch.
func RebaseResolution(o RebaseResolutionOutcome) RebaseResolutionResult {
	if o.SpendingCapAbort {
		return RebaseResolutionResult{OK: false, SpendingCapAbort: true, Reason: "conflict-resolution session aborted before resolving — spending cap reached, retry after reset"}
	}
	if o.SessionExit != 0 {
		return RebaseResolutionResult{OK: false, Reason: fmt.Sprintf("conflict-resolution session exited %d before resolving the rebase conflict", o.SessionExit)}
	}
	if !o.WorktreeClean {
		return RebaseResolutionResult{OK: false, Reason: "conflict-resolution session left the worktree dirty (unresolved or uncommitted conflict) — rebase did not cleanly complete"}
	}
	if !o.Rebased {
		return RebaseResolutionResult{OK: false, Reason: "conflict-resolution session left the branch on its stale base (origin/main is not an ancestor) — it aborted the rebase rather than resolving it"}
	}
	return RebaseResolutionResult{OK: true, Reason: "conflict resolved and branch rebased onto origin/main — re-gating before push"}
}
