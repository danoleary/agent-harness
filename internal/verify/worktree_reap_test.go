package verify

import (
	"strings"
	"testing"
)

// The regression that motivates the gate: the branch reached origin but its
// `gh pr create` timed out, so no PR exists. Reaping here destroys the worktree the
// loop's committed-fix recovery needs, and the ticket loops until the breaker trips.
func TestWorktreeReapKeepsAPushedBranchWithNoPR(t *testing.T) {
	got := WorktreeReap(&truth{t: t, pushed: true, pr: false}, slug)
	if got.OK {
		t.Fatal("reaped a pushed branch with no PR, want it kept (the recovery needs the worktree)")
	}
	if !strings.Contains(got.Reason, "no PR") {
		t.Errorf("Reason = %q, want it to name the missing PR", got.Reason)
	}
}

// An unpushed branch is the pre-existing keep case — unpushed work is never lost.
// It must stay a keep even though the PR read is trivially false, and the push check
// coming first is what keeps the gh round-trip off that path entirely.
func TestWorktreeReapKeepsAnUnpushedBranchWithoutAskingGh(t *testing.T) {
	g := &truth{t: t, pushed: false, pr: false}
	got := WorktreeReap(g, slug)
	if got.OK {
		t.Fatal("reaped an unpushed branch, want it kept")
	}
	if !strings.Contains(got.Reason, "not pushed") {
		t.Errorf("Reason = %q, want it to name the unpushed branch", got.Reason)
	}
	for _, read := range g.reads {
		if read == "PRExists" {
			t.Errorf("reads = %v, want an unpushed branch decided without the gh round-trip", g.reads)
		}
	}
}

// The normal ship path: pushed and PR'd, so the PR captures everything and the
// worktree is pure disk cost.
func TestWorktreeReapReapsAPushedBranchWithAPR(t *testing.T) {
	got := WorktreeReap(&truth{t: t, pushed: true, pr: true}, slug)
	if !got.OK {
		t.Fatalf("kept a pushed branch with a PR, want it reaped (reason: %q)", got.Reason)
	}
}

// A PR without a pushed branch is incoherent ground truth (gh answered for a
// branch git says never reached origin). Prefer the conservative keep — a stale
// breadcrumb costs disk, a wrong reap costs the work.
func TestWorktreeReapPrefersKeepOnIncoherentGroundTruth(t *testing.T) {
	if WorktreeReap(&truth{t: t, pushed: false, pr: true}, slug).OK {
		t.Fatal("reaped an unpushed branch that reports a PR, want it kept")
	}
}
