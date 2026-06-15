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
	r := Retrospective(true)
	if !r.OK {
		t.Errorf("expected OK when dropbox present, got %+v", r)
	}
}

func TestRetrospectiveFailsWhenDropboxAbsent(t *testing.T) {
	r := Retrospective(false)
	if r.OK {
		t.Error("expected failure when dropbox absent")
	}
	if !regexp.MustCompile(`(?i)out\.json|dropbox|findings`).MatchString(r.Reason) {
		t.Errorf("reason %q does not point at the missing dropbox", r.Reason)
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
