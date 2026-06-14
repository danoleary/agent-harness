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
