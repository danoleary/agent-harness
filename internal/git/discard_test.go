package git_test

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
)

// Discard drops an untouched worktree and its branch, so the next Create cuts a
// fresh branch from the base main has moved to rather than re-attaching the old
// one (#125).
func TestDiscardDropsAnUntouchedWorktreeAndItsBranch(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-1")
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldBase, _ := wt.HeadSHA()

	if err := wt.Discard(); err != nil {
		t.Fatalf("Discard: %v", err)
	}
	if wt.Exists() || wt.BranchExists() {
		t.Fatal("Discard must remove both the worktree and its branch")
	}

	repo.Merge("fix.txt", "the repair\n", "repair main")
	if err := wt.Create(); err != nil {
		t.Fatalf("re-Create: %v", err)
	}
	if head, _ := wt.HeadSHA(); head == oldBase {
		t.Error("the re-created branch must be cut from the moved origin/main, not the discarded base")
	}
}

// A branch with commits holds a session's work; Discard refuses it.
func TestDiscardRefusesABranchWithWork(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("gh-2")

	if err := wt.Discard(); err == nil {
		t.Fatal("Discard must refuse a branch carrying commits ahead of origin/main")
	}
	if !wt.Exists() || !wt.BranchExists() {
		t.Error("a refused Discard must leave the worktree and branch in place")
	}
}
