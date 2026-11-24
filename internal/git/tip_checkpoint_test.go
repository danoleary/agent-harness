package git

import (
	"slices"
	"testing"
)

// #431: the recovery checkpoints at the branch tip are work no session handed
// off. The read names the newest one, the commit the run of them sits on, and
// the paths they change, so review can be told what it is about to inherit.
func TestTipCheckpointNamesTheUnhandedWorkAtTheTip(t *testing.T) {
	repo := seedBranchRepo(t, "gh-431", "the handoff (#431)", "checkpoint(harness): recover uncommitted tdd session work (#431)", "checkpoint(harness): recover uncommitted review session work (#431)")
	wt := Open(repo, "feat").Worktree("gh-431")

	got, ok := wt.TipCheckpoint()

	if !ok {
		t.Fatal("a branch whose tip is a recovery checkpoint has unhanded work at its tip")
	}
	if want := revParse(t, repo, "feat/gh-431"); got.Commit != want {
		t.Errorf("Commit = %s, want the tip %s", got.Commit, want)
	}
	if want := revParse(t, repo, "feat/gh-431~2"); got.Since != want {
		t.Errorf("Since = %s, want the handoff %s the checkpoints sit on", got.Since, want)
	}
	if want := []string{"ahead-1", "ahead-2"}; !slices.Equal(got.Paths, want) {
		t.Errorf("Paths = %q, want %q: what the checkpoints changed, not the handoff", got.Paths, want)
	}
}

// A checkpoint some session later built on was handed off by that session: only
// a run of checkpoints at the very tip is unhanded. A branch of nothing but
// checkpoints sits on its base.
func TestTipCheckpointIsOnlyTheRunAtTheTip(t *testing.T) {
	repo := seedBranchRepo(t, "gh-431", "checkpoint(harness): recover", "the handoff (#431)")
	if _, ok := Open(repo, "feat").Worktree("gh-431").TipCheckpoint(); ok {
		t.Error("a checkpoint under a handoff commit is not unhanded work")
	}

	repo = seedBranchRepo(t, "gh-431", "checkpoint(harness): recover")
	got, ok := Open(repo, "feat").Worktree("gh-431").TipCheckpoint()
	if want := revParse(t, repo, "origin/main"); !ok || got.Since != want {
		t.Errorf("TipCheckpoint = %+v, %v; want it to sit on the base %s", got, ok, want)
	}

	repo = seedBranchRepo(t, "gh-431")
	if _, ok := Open(repo, "feat").Worktree("gh-431").TipCheckpoint(); ok {
		t.Error("a branch with nothing ahead has no checkpoint at its tip")
	}
}
