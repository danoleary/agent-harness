package verify

import (
	"strings"
	"testing"
)

// The regression that motivates the gate: the branch reached origin but its
// `gh pr create` timed out, so no PR exists. Reaping here destroys the worktree the
// loop's committed-fix recovery needs, and the ticket loops until the breaker trips.
func TestWorktreeReapKeepsAPushedBranchWithNoPR(t *testing.T) {
	got := WorktreeReap(WorktreeReapOutcome{BranchPushed: true, PRExists: false})
	if got.Reap {
		t.Fatal("Reap = true for a pushed branch with no PR, want false (the recovery needs the worktree)")
	}
	if !strings.Contains(got.Reason, "no PR") {
		t.Errorf("Reason = %q, want it to name the missing PR", got.Reason)
	}
}

// An unpushed branch is the pre-existing keep case — unpushed work is never lost.
// It must stay a keep even though PRExists is trivially false.
func TestWorktreeReapKeepsAnUnpushedBranch(t *testing.T) {
	got := WorktreeReap(WorktreeReapOutcome{BranchPushed: false, PRExists: false})
	if got.Reap {
		t.Fatal("Reap = true for an unpushed branch, want false")
	}
	if !strings.Contains(got.Reason, "not pushed") {
		t.Errorf("Reason = %q, want it to name the unpushed branch", got.Reason)
	}
}

// The normal ship path: pushed and PR'd, so the PR captures everything and the
// worktree is pure disk cost.
func TestWorktreeReapReapsAPushedBranchWithAPR(t *testing.T) {
	got := WorktreeReap(WorktreeReapOutcome{BranchPushed: true, PRExists: true})
	if !got.Reap {
		t.Fatalf("Reap = false for a pushed branch with a PR, want true (reason: %q)", got.Reason)
	}
}

// PRExists without BranchPushed is incoherent ground truth (gh answered for a
// branch git says never reached origin). Prefer the conservative keep — a stale
// breadcrumb costs disk, a wrong reap costs the work.
func TestWorktreeReapPrefersKeepOnIncoherentGroundTruth(t *testing.T) {
	if WorktreeReap(WorktreeReapOutcome{BranchPushed: false, PRExists: true}).Reap {
		t.Fatal("Reap = true for an unpushed branch that reports a PR, want false")
	}
}
