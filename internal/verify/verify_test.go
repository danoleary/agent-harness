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

// Ground truth for retrospective is the *presence* of the findings dropbox: an
// empty `[]` is a valid "ran, found nothing" (still present → success); an
// absent file means the step never ran and is a failure (DESIGN.md).
func TestRetrospectivePassesWhenDropboxPresent(t *testing.T) {
	r := Retrospective(true, false)
	if !r.OK {
		t.Errorf("expected OK when dropbox present, got %+v", r)
	}
}

func TestRetrospectiveFailsWhenDropboxAbsent(t *testing.T) {
	r := Retrospective(false, false)
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

// BEH-494: a spending-cap abort killed the session before it could write the
// dropbox. The reason must be the distinct retry-after-reset class — NOT the
// misleading generic "never ran" (which reads as the agent misbehaving).
func TestRetrospectiveReportsSpendingCapAbort(t *testing.T) {
	r := Retrospective(false, true)
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
	r := Review(ReviewOutcome{GatesGreen: true, WorktreeClean: true})
	if !r.OK {
		t.Errorf("green gates over a clean worktree must clear the push gate, got %+v", r)
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
