package verify

import (
	"regexp"
	"testing"
)

func TestTddPassesWhenWorktreeAndCommitAhead(t *testing.T) {
	r := Tdd(GroundTruth{WorktreeExists: true, CommitsAhead: 1})
	if !r.OK {
		t.Errorf("expected OK, got %+v", r)
	}
}

func TestTddFailsWhenNoWorktree(t *testing.T) {
	r := Tdd(GroundTruth{WorktreeExists: false, CommitsAhead: 0})
	if r.OK {
		t.Error("expected failure when worktree absent")
	}
	if !regexp.MustCompile(`(?i)worktree`).MatchString(r.Reason) {
		t.Errorf("reason %q does not mention worktree", r.Reason)
	}
}

func TestTddFailsWhenNoCommitAhead(t *testing.T) {
	r := Tdd(GroundTruth{WorktreeExists: true, CommitsAhead: 0})
	if r.OK {
		t.Error("expected failure when no commit ahead")
	}
	if !regexp.MustCompile(`(?i)commit`).MatchString(r.Reason) {
		t.Errorf("reason %q does not mention commit", r.Reason)
	}
}

// BEH-597: a branch that shares NO common ancestor with origin/main (an empty
// `git merge-base feat/<slug> origin/main`) is a disjoint history — the BEH-355
// 963-commits-ahead condition. It satisfies the worktree + commits-ahead checks
// (it is "ahead" by all of its own commits), so without an explicit disjoint
// signal it sails through as a healthy pass and the pre-push rebase then collides
// trying to replay every disjoint commit. A disjoint history is never a real
// single-ticket handoff; it must be a suspect/FAILED ground truth, not success.
func TestTddFailsWhenHistoryDisjoint(t *testing.T) {
	r := Tdd(GroundTruth{WorktreeExists: true, CommitsAhead: 963, DisjointHistory: true})
	if r.OK {
		t.Error("a disjoint-history branch (no common ancestor) must NOT pass as a healthy handoff")
	}
	if !regexp.MustCompile(`(?i)disjoint|common ancestor|merge.base`).MatchString(r.Reason) {
		t.Errorf("reason %q should name the disjoint history / missing common ancestor", r.Reason)
	}
}

// Ground truth for retrospective is the *presence* of the findings dropbox: an
// empty `[]` is a valid "ran, found nothing" (still present → success); an
// absent file means the step never ran and is a failure (DESIGN.md).
func TestRetrospectivePassesWhenDropboxPresent(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{DropboxExists: true})
	if !r.OK {
		t.Errorf("expected OK when dropbox present, got %+v", r)
	}
}

func TestRetrospectiveFailsWhenDropboxAbsent(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{})
	if r.OK {
		t.Error("expected failure when dropbox absent")
	}
	if !regexp.MustCompile(`(?i)out\.json|dropbox|findings`).MatchString(r.Reason) {
		t.Errorf("reason %q does not point at the missing dropbox", r.Reason)
	}
	if !regexp.MustCompile(`(?i)never ran`).MatchString(r.Reason) {
		t.Errorf("a plain missing dropbox should read as 'never ran', got %q", r.Reason)
	}
}

// BEH-536: the retrospective is a long, read-heavy step that hit its wall-clock
// cap (exit 137) mid-investigation — after doing the analysis but before its
// (deferred) write. An absent dropbox caused by a 137 kill must read as the
// distinct "killed before writing — retry" class, NOT the misleading "never ran"
// (which reads as the agent skipping the step). This mirrors the spending-cap
// handling below and ReviewQualitative's OOM branch.
func TestRetrospectiveReportsKilledBeforeWrite(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{ExitCode: 137})
	if r.OK {
		t.Error("a 137-killed session wrote no dropbox — still a failure")
	}
	if !regexp.MustCompile(`(?i)137|kill`).MatchString(r.Reason) {
		t.Errorf("reason %q should name the kill (exit 137)", r.Reason)
	}
	if !regexp.MustCompile(`(?i)retry`).MatchString(r.Reason) {
		t.Errorf("reason %q should signal a retry, not a permanent failure", r.Reason)
	}
	if regexp.MustCompile(`(?i)never ran`).MatchString(r.Reason) {
		t.Errorf("a 137 kill must NOT use the misleading 'never ran' wording, got %q", r.Reason)
	}
}

// BEH-709: the session exited 0 with is_error=false but did zero real work — the
// pinned CLI's turn-0 no-op, where a `!`+backtick directive in the prompt crashed
// the session before it could write findings. Its exit-0/success shape would
// otherwise fall through to the plain "never ran", masking a genuine crash as the
// agent skipping the step. It must read distinctly — name the turn-0 crash, signal
// a retry (the trigger is nondeterministic), and NOT use the "never ran" wording.
func TestRetrospectiveReportsTurnZeroCrash(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{TurnZeroNoOp: true})
	if r.OK {
		t.Error("a turn-0 no-op wrote no dropbox — still a failure")
	}
	if !regexp.MustCompile(`(?i)turn-0|crash`).MatchString(r.Reason) {
		t.Errorf("reason %q should name the turn-0 crash", r.Reason)
	}
	if !regexp.MustCompile(`(?i)retry`).MatchString(r.Reason) {
		t.Errorf("reason %q should signal a retry, not a permanent skip", r.Reason)
	}
	if regexp.MustCompile(`(?i)never ran`).MatchString(r.Reason) {
		t.Errorf("a turn-0 crash must NOT use the misleading 'never ran' wording, got %q", r.Reason)
	}
}

// A turn-0 no-op that somehow coexists with a PRESENT dropbox must let the genuine
// output stand: a real out.json is proof the session did work, so dropbox-present
// is checked before the turn-0 branch (BEH-709). Guards the precedence order.
func TestRetrospectivePresentDropboxWinsOverTurnZero(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{DropboxExists: true, TurnZeroNoOp: true})
	if !r.OK {
		t.Errorf("a present dropbox is genuine output and must stand over the turn-0 flag, got %+v", r)
	}
}

// BEH-553: the retrospective has nothing to work on when its two inputs — the
// feature branch (the diff) and the prior implementation/review transcripts —
// are BOTH absent. Launching the sandbox then can only emit an empty [] that
// masks the misscheduling or manufacture a self-referential finding, so the
// stage skips host-side before paying the cap.
func TestRetrospectivePreconditionsSkipWhenNoInputs(t *testing.T) {
	r := RetrospectivePreconditions(RetrospectiveInputs{BranchExists: false, PriorTranscripts: false})
	if r.OK {
		t.Error("expected NOT-OK (skip) when neither the branch nor any transcript exists")
	}
	if !regexp.MustCompile(`(?i)branch|transcript|upstream|produced nothing`).MatchString(r.Reason) {
		t.Errorf("reason %q should explain the missing upstream inputs", r.Reason)
	}
}

// A single real input is enough to proceed. The pipeline deliberately runs the
// retrospective even on a FAILED slice ("exactly the run worth mining for
// findings"), and a failed slice routinely has transcripts but no branch (the
// session crashed before creating the worktree) — so transcripts alone must not
// be skipped. Symmetrically a branch alone (a diff with no session logs) is also
// worth a pass. The skip is BOTH-absent, deliberately not either-absent.
func TestRetrospectivePreconditionsProceedWhenOnlyTranscripts(t *testing.T) {
	r := RetrospectivePreconditions(RetrospectiveInputs{BranchExists: false, PriorTranscripts: true})
	if !r.OK {
		t.Errorf("transcripts present (a mineable failed slice) must proceed, got %+v", r)
	}
}

func TestRetrospectivePreconditionsProceedWhenOnlyBranch(t *testing.T) {
	r := RetrospectivePreconditions(RetrospectiveInputs{BranchExists: true, PriorTranscripts: false})
	if !r.OK {
		t.Errorf("a feature branch present (a diff to review) must proceed, got %+v", r)
	}
}

func TestRetrospectivePreconditionsProceedWhenBothPresent(t *testing.T) {
	r := RetrospectivePreconditions(RetrospectiveInputs{BranchExists: true, PriorTranscripts: true})
	if !r.OK {
		t.Errorf("both inputs present must proceed, got %+v", r)
	}
}

// A spending-cap abort that also carries a 137 exit must still report the
// spending-cap class — the more specific, actionable cause (retry after the
// billing window resets, not just re-run now).
func TestRetrospectiveSpendingCapWinsOverExitCode(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{SpendingCapAbort: true, ExitCode: 137})
	if !regexp.MustCompile(`(?i)spending cap`).MatchString(r.Reason) {
		t.Errorf("spending-cap abort must take precedence over the generic 137 kill, got %q", r.Reason)
	}
}

// BEH-568: a spending-cap abort that coexists with a PRESENT out.json must still
// route to the retry-after-reset class, not be masked into a false "ran, found
// nothing" success. The synthetic cap abort replaces a genuine assistant turn, so
// a present dropbox can only be a stale prior-run file or the early `[]` the skill
// writes before its analysis — never proof the retrospective completed. The two
// signals disagree here; the abort is authoritative, so it must win over the
// dropbox-present check, exactly the disagreement that would otherwise let the
// dropbox silently skip the re-run.
func TestRetrospectiveSpendingCapWinsOverPresentDropbox(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{DropboxExists: true, SpendingCapAbort: true, ExitCode: 1})
	if r.OK {
		t.Error("a spending-cap abort must not be masked by a present (stale/[]) dropbox — expected a retry, got OK")
	}
	if !regexp.MustCompile(`(?i)spending cap`).MatchString(r.Reason) {
		t.Errorf("reason %q does not name the spending-cap abort", r.Reason)
	}
	if !regexp.MustCompile(`(?i)retry after reset`).MatchString(r.Reason) {
		t.Errorf("reason %q does not signal retry-after-reset", r.Reason)
	}
}

// BEH-494: a spending-cap abort killed the session before it could write the
// dropbox. The reason must be the distinct retry-after-reset class — NOT the
// misleading generic "never ran" (which reads as the agent misbehaving).
func TestRetrospectiveReportsSpendingCapAbort(t *testing.T) {
	r := Retrospective(RetrospectiveOutcome{SpendingCapAbort: true})
	if r.OK {
		t.Error("a spending-cap abort wrote no dropbox — still a failure")
	}
	if !regexp.MustCompile(`(?i)spending cap`).MatchString(r.Reason) {
		t.Errorf("reason %q does not name the spending-cap abort", r.Reason)
	}
	if !regexp.MustCompile(`(?i)retry after reset`).MatchString(r.Reason) {
		t.Errorf("reason %q does not signal retry-after-reset", r.Reason)
	}
	if regexp.MustCompile(`(?i)never ran`).MatchString(r.Reason) {
		t.Errorf("a cap abort must NOT use the misleading 'never ran' wording, got %q", r.Reason)
	}
}

func TestReviewPushesWhenGatesGreenAndWorktreeClean(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: true, ReviewComplete: true})
	if !r.OK {
		t.Errorf("green gates over a clean worktree with a completed review must clear the push gate, got %+v", r)
	}
}

func TestReviewBlocksPushWhenGatesRed(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: false, WorktreeClean: true})
	if r.OK {
		t.Error("gates red must NOT clear the push gate — no branch ships on a failing gate")
	}
	if !regexp.MustCompile(`(?i)gate`).MatchString(r.Reason) {
		t.Errorf("reason %q does not mention the gate result", r.Reason)
	}
}

// A dirty worktree means the gate validated a different tree than would ship, so
// the push is blocked even when the gate is green — the harness ships only what it
// actually verified.
func TestReviewBlocksPushWhenWorktreeDirty(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: false})
	if r.OK {
		t.Error("a dirty worktree must NOT clear the push gate even with green gates")
	}
	if !regexp.MustCompile(`(?i)uncommitted|worktree`).MatchString(r.Reason) {
		t.Errorf("reason %q does not mention the dirty worktree", r.Reason)
	}
}

// BEH-569: a green host-side gate over a clean worktree is NOT sufficient to ship
// a branch — the qualitative seven-lens review must also have produced its verdict.
// A review session killed before emitting "## Review:" (a spending-cap abort / OOM)
// leaves the diff with zero qualitative review; the push must fail closed so the PR
// is never opened on a gate re-run alone (the gate only proves the diff compiles —
// it is not a review).
func TestReviewBlocksPushWhenReviewIncompleteDespiteGreenGate(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: true, ReviewComplete: false})
	if r.OK {
		t.Error("an incomplete qualitative review (no verdict) must NOT clear the push gate even with green gates over a clean worktree")
	}
	if !regexp.MustCompile(`(?i)review|verdict`).MatchString(r.Reason) {
		t.Errorf("reason %q does not name the missing qualitative review/verdict", r.Reason)
	}
}

// BEH-580: a review that emitted its verdict but declared "Disposition: blocked" —
// an unresolved Blocker/Important finding it could not autonomously resolve — must
// NOT push, even with green gates over a clean worktree and a completed review. The
// autonomous pipeline has no human to answer the approval prompt, so an open finding
// would otherwise reach the PR unaddressed (the BEH-439 leak). Fail closed and keep
// the worktree for a human decision.
func TestReviewBlocksPushWhenReviewVerdictBlocked(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: true, ReviewComplete: true, ReviewBlocked: true})
	if r.OK {
		t.Error("a blocked review disposition must NOT clear the push gate even with green gates, a clean worktree, and a completed review")
	}
	if !regexp.MustCompile(`(?i)blocked|unresolved|human`).MatchString(r.Reason) {
		t.Errorf("reason %q does not name the blocked/unresolved finding", r.Reason)
	}
}

// BEH-603: a branch whose committed tip makes ZERO net change against origin/main
// (an empty `git diff origin/main` — the empty-commit branch BEH-365 produced) must
// NOT be pushed as a PR. The agents correctly concluded the ticket should be closed
// as a duplicate/superseded rather than ship an empty commit, but the harness had no
// terminal state for that, so it pushed PR #642. A clean worktree + green gate + a
// completed review over a zero-net-diff branch is the recommend-close disposition:
// not a push, not a plain failure, but a handoff for a human to close the ticket.
func TestReviewRecommendsCloseWhenDiffEmpty(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: true, ReviewComplete: true, EmptyDiff: true})
	if r.OK {
		t.Error("a zero-net-diff branch must NOT clear the push gate — there is nothing to ship")
	}
	if !r.RecommendClose {
		t.Error("a clean, gate-green, reviewed branch with an empty diff must be the recommend-close disposition")
	}
	if !regexp.MustCompile(`(?i)close|duplicate|empty|no.*change|nothing to ship`).MatchString(r.Reason) {
		t.Errorf("reason %q should explain the empty diff / recommend-close", r.Reason)
	}
}

// A dirty worktree takes precedence over the empty-diff check: the committed tip may
// be empty only because the real change is still UNCOMMITTED (the review-left-edits
// recovery path). Recommend-close must never fire on a dirty tree — that would
// strand uncommitted work and wrongly advise closing a live ticket.
func TestReviewDoesNotRecommendCloseWhenWorktreeDirty(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: false, ReviewComplete: true, EmptyDiff: true})
	if r.RecommendClose {
		t.Error("an empty committed diff with a DIRTY worktree must not recommend close — the real change may be uncommitted")
	}
	if r.OK {
		t.Error("a dirty worktree must still block the push")
	}
}

// A non-empty diff is the normal ship path — recommend-close must stay off so the
// branch pushes as usual.
func TestReviewDoesNotRecommendCloseWhenDiffPresent(t *testing.T) {
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: true, ReviewComplete: true, EmptyDiff: false})
	if r.RecommendClose {
		t.Error("a branch with a real diff must not be flagged recommend-close")
	}
	if !r.OK {
		t.Errorf("a clean, gate-green, reviewed branch with a real diff must clear the push gate, got %+v", r)
	}
}

// BEH-680: the pre-rebase EmptyDiff gate (BEH-603) runs before the pre-push rebase,
// so it cannot see a branch that collapses to zero net change DURING the replay — a
// sibling PR landed the same fix, or a conflict-resolution session skipped a
// now-empty commit, leaving the branch identical to origin/main. Pushing then and
// running `gh pr create` hard-fails with "No commits between main and feat/…". After
// the rebase, a re-check of the branch's emptiness must route to the same
// recommend-close disposition rather than push + open a PR that has nothing to open.
func TestPostRebasePushRecommendsCloseWhenBranchCollapsedToEmpty(t *testing.T) {
	r := PostRebasePush(true)
	if r.OK {
		t.Error("a branch the rebase collapsed to zero net diff must NOT clear the push gate — there is nothing to open a PR for")
	}
	if !r.RecommendClose {
		t.Error("a branch that became empty during the pre-push rebase must be the recommend-close disposition, not a push")
	}
	if !regexp.MustCompile(`(?i)close|empty|no.*change|nothing to (ship|open)|zero`).MatchString(r.Reason) {
		t.Errorf("reason %q should explain the post-rebase empty diff / recommend-close", r.Reason)
	}
}

// The normal path: the rebase replayed the branch and it still carries a real diff,
// so the push proceeds — recommend-close must stay off.
func TestPostRebasePushClearsWhenDiffPresent(t *testing.T) {
	r := PostRebasePush(false)
	if r.RecommendClose {
		t.Error("a branch that still has a real diff after the rebase must not be flagged recommend-close")
	}
	if !r.OK {
		t.Errorf("a non-empty branch must clear the post-rebase push gate, got %+v", r)
	}
}

// BEH-525: completeness of the qualitative review is independent of the push gate.
// When the review session emitted its verdict (the "## Review:" report), the
// seven-lens pass ran — complete, regardless of how the container exited.
func TestReviewQualitativeCompleteWhenVerdictEmitted(t *testing.T) {
	r := ReviewQualitative(0, true)
	if !r.Complete {
		t.Errorf("a verdict-emitting session is a complete review, got %+v", r)
	}
}

// The BEH-499 scenario: the review session was OOM-killed (exit 137) mid-gate
// before reaching the report. The host-side gate re-run may still be green, but
// the qualitative review never ran — it must be flagged incomplete, naming the OOM
// so a log reader isn't misled into thinking the green gate was a full review.
func TestReviewQualitativeFlagsOomBeforeVerdict(t *testing.T) {
	r := ReviewQualitative(137, false)
	if r.Complete {
		t.Error("an OOM before the verdict is NOT a complete review")
	}
	if !regexp.MustCompile(`(?i)137|oom`).MatchString(r.Reason) {
		t.Errorf("reason %q does not name the OOM/exit-137 cause", r.Reason)
	}
	if !regexp.MustCompile(`(?i)incomplete`).MatchString(r.Reason) {
		t.Errorf("reason %q does not flag the review as incomplete", r.Reason)
	}
}

// Any other end before the verdict (a non-OOM crash, or even a clean exit that
// never produced the report) is still an incomplete review — the report is the
// only proof the lenses ran. The exit code is named so the cause is traceable, but
// it must NOT be mislabelled as an OOM when it isn't.
func TestReviewQualitativeFlagsNonOomEndBeforeVerdict(t *testing.T) {
	r := ReviewQualitative(1, false)
	if r.Complete {
		t.Error("ending before the verdict is not a complete review, whatever the exit code")
	}
	if !regexp.MustCompile(`(?i)incomplete`).MatchString(r.Reason) {
		t.Errorf("reason %q does not flag the review as incomplete", r.Reason)
	}
	if regexp.MustCompile(`(?i)137|oom`).MatchString(r.Reason) {
		t.Errorf("a non-137 exit must not be labelled an OOM, got %q", r.Reason)
	}
}

// BEH-581: after a sandboxed pre-push conflict-resolution session, RebaseResolution
// decides whether the branch is cleanly rebased and ready to re-gate + push —
// judged entirely on git ground truth (session exit, worktree clean, branch
// actually rebased), never the agent's say-so.
func TestRebaseResolutionPassesWhenResolvedRebasedAndClean(t *testing.T) {
	r := RebaseResolution(RebaseResolutionOutcome{SessionExit: 0, WorktreeClean: true, Rebased: true})
	if !r.OK {
		t.Fatalf("a clean session that rebased onto base with a clean worktree should pass: %q", r.Reason)
	}
	if r.SpendingCapAbort {
		t.Error("a normal pass must not be flagged a spending-cap abort")
	}
}

// A spending-cap abort is its own retry-after-reset class: it took no real action,
// so it must NOT be reported as a content conflict (no Linear breadcrumb) — just
// deferred. It takes precedence over everything else, since a capped session never
// resolved anything regardless of the worktree's incidental state.
func TestRebaseResolutionSpendingCapTakesPrecedence(t *testing.T) {
	r := RebaseResolution(RebaseResolutionOutcome{SessionExit: 1, SpendingCapAbort: true, WorktreeClean: true, Rebased: false})
	if r.OK {
		t.Fatal("a spending-cap abort did not resolve the conflict — must not pass")
	}
	if !r.SpendingCapAbort {
		t.Fatal("a spending-cap abort must be flagged so the caller defers (no breadcrumb)")
	}
	if !regexp.MustCompile(`(?i)cap|retry`).MatchString(r.Reason) {
		t.Errorf("reason %q should name the retry-after-reset class", r.Reason)
	}
}

func TestRebaseResolutionFailsOnNonZeroSessionExit(t *testing.T) {
	r := RebaseResolution(RebaseResolutionOutcome{SessionExit: 2, WorktreeClean: true, Rebased: true})
	if r.OK {
		t.Fatal("a non-zero session exit means the resolution session failed — must not pass")
	}
	if r.SpendingCapAbort {
		t.Error("a plain non-zero exit is not a spending-cap abort")
	}
	if !regexp.MustCompile(`2`).MatchString(r.Reason) {
		t.Errorf("reason %q should carry the exit code", r.Reason)
	}
}

// The session ended clean but left the worktree dirty (unresolved/uncommitted
// conflict): the rebase did not cleanly complete. The dirty signal is reported
// even though the branch is also not rebased — dirty is the more actionable cause.
func TestRebaseResolutionFailsWhenWorktreeDirty(t *testing.T) {
	r := RebaseResolution(RebaseResolutionOutcome{SessionExit: 0, WorktreeClean: false, Rebased: false})
	if r.OK {
		t.Fatal("a dirty worktree means the rebase did not cleanly complete — must not pass")
	}
	if !regexp.MustCompile(`(?i)dirty|uncommitted|unresolved`).MatchString(r.Reason) {
		t.Errorf("reason %q should name the dirty/unresolved worktree", r.Reason)
	}
}

// The session left a CLEAN worktree but on the original stale tip — it aborted the
// rebase rather than resolving it. WorktreeClean alone would wave this through, so
// the not-rebased guard is what stops the harness pushing a stale-base branch.
func TestRebaseResolutionFailsWhenNotRebased(t *testing.T) {
	r := RebaseResolution(RebaseResolutionOutcome{SessionExit: 0, WorktreeClean: true, Rebased: false})
	if r.OK {
		t.Fatal("a clean worktree still on the stale base did not rebase — must not pass")
	}
	if !regexp.MustCompile(`(?i)rebas|abort|stale|base`).MatchString(r.Reason) {
		t.Errorf("reason %q should explain the branch was not rebased onto base", r.Reason)
	}
}

// BEH-624: the cheapest incompleteness class to recover. The review session exited
// cleanly (code 0) one turn short of its verdict, the worktree is clean, and the
// harness's own host-side gate is green — the diff is byte-identical and already
// verified, the review just needs its last few turns to print the verdict. That is
// eligible for a bounded in-stage re-launch over the same worktree, not a fall-
// through to fail-closed.
func TestReviewVerdictRetryEligibleOnCleanExitGreenGate(t *testing.T) {
	d := ReviewVerdictRetry(ReviewRetryInputs{
		VerdictEmitted:   false,
		ExitCode:         0,
		SpendingCapAbort: false,
		WorktreeClean:    true,
		GatesGreen:       true,
	})
	if !d.Retry {
		t.Errorf("a clean-exit, gate-green, clean-tree review without a verdict should be retried, got %+v", d)
	}
}

// A review that already emitted its verdict is complete — there is nothing to
// re-launch, regardless of the other inputs.
func TestReviewVerdictRetryNotEligibleWhenVerdictEmitted(t *testing.T) {
	d := ReviewVerdictRetry(ReviewRetryInputs{
		VerdictEmitted: true,
		ExitCode:       0,
		WorktreeClean:  true,
		GatesGreen:     true,
	})
	if d.Retry {
		t.Errorf("a review that already produced a verdict must not be re-launched, got %+v", d)
	}
}

// An OOM (exit 137) before the verdict is the killed-before-verdict class
// ReviewQualitative already names — NOT the cheap clean-exit retry this targets. A
// re-launch over a memory-pressured host is not the byte-identical recovery here.
func TestReviewVerdictRetryNotEligibleOnOomExit(t *testing.T) {
	d := ReviewVerdictRetry(ReviewRetryInputs{
		ExitCode:      137,
		WorktreeClean: true,
		GatesGreen:    true,
	})
	if d.Retry {
		t.Errorf("an OOM (137) before the verdict is not the clean-exit retry class, got %+v", d)
	}
}

// A spending-cap abort is its own retry-after-reset class — the session is killed
// before doing any work, so an in-stage re-launch would just burn the same cap.
func TestReviewVerdictRetryNotEligibleOnSpendingCapAbort(t *testing.T) {
	d := ReviewVerdictRetry(ReviewRetryInputs{
		ExitCode:         0,
		SpendingCapAbort: true,
		WorktreeClean:    true,
		GatesGreen:       true,
	})
	if d.Retry {
		t.Errorf("a spending-cap abort defers until the cap resets, never an in-stage re-launch, got %+v", d)
	}
}

// A dirty worktree means the gate validated a different tree than would ship, so the
// diff is not the proven-good artifact the cheap retry assumes. Fall through.
func TestReviewVerdictRetryNotEligibleWhenWorktreeDirty(t *testing.T) {
	d := ReviewVerdictRetry(ReviewRetryInputs{
		ExitCode:      0,
		WorktreeClean: false,
		GatesGreen:    true,
	})
	if d.Retry {
		t.Errorf("a dirty worktree is not the proven-good diff this retry assumes, got %+v", d)
	}
}

// Red gates mean the diff itself is broken — re-running the review cannot make it
// shippable, so it must fall through to fail-closed rather than re-launch.
func TestReviewVerdictRetryNotEligibleWhenGatesRed(t *testing.T) {
	d := ReviewVerdictRetry(ReviewRetryInputs{
		ExitCode:      0,
		WorktreeClean: true,
		GatesGreen:    false,
	})
	if d.Retry {
		t.Errorf("a red gate means the diff is broken — re-running the review can't fix it, got %+v", d)
	}
}
