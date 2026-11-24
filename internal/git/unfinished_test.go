package git_test

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// conflicting is slug's handoff worktree with origin/main fetched after a
// sibling changed the handoff's own file, so merging, rebasing or picking across
// the two stops on it. It returns the sibling's commit on origin/main.
func conflicting(t *testing.T) (*gittest.Repo, gitpkg.Worktree, string) {
	t.Helper()
	repo := gittest.New(t)
	wt := repo.Handoff("gh-428")
	repo.Conflict("gh-428")
	repo.Git(repo.Dir, "fetch", "-q", "origin", "main")
	return repo, wt, repo.Git(repo.Dir, "rev-parse", "origin/main")
}

// #428: a merge, rebase or cherry-pick a session left stopped on a conflict is
// aborted, named, and the worktree is back on the branch's own tip, clean.
func TestAbortUnfinishedAbortsAStoppedOperation(t *testing.T) {
	for _, tc := range []struct {
		op   string
		stop func(*gittest.Repo, gitpkg.Worktree, string)
	}{
		{"merge", func(r *gittest.Repo, wt gitpkg.Worktree, _ string) { r.UnfinishedMerge(wt, "origin/main") }},
		{"rebase", func(r *gittest.Repo, wt gitpkg.Worktree, _ string) { r.UnfinishedRebase(wt, "origin/main") }},
		{"cherry-pick", func(r *gittest.Repo, wt gitpkg.Worktree, sibling string) { r.UnfinishedCherryPick(wt, sibling) }},
	} {
		t.Run(tc.op, func(t *testing.T) {
			repo, wt, sibling := conflicting(t)
			tip, _ := wt.HeadSHA()
			tc.stop(repo, wt, sibling)

			aborted, err := wt.AbortUnfinished()

			if err != nil || aborted != tc.op {
				t.Fatalf("AbortUnfinished = %q, %v; want the %s aborted", aborted, err, tc.op)
			}
			if head, _ := wt.HeadSHA(); head != tip || !wt.Clean() {
				t.Errorf("the worktree is at %s (clean %v), want the branch's tip %s, clean", head, wt.Clean(), tip)
			}
			if branch := repo.Git(wt.Path(), "rev-parse", "--abbrev-ref", "HEAD"); branch != wt.Branch() {
				t.Errorf("the worktree is on %s, want %s", branch, wt.Branch())
			}
		})
	}
}

// Ordinary uncommitted work, with nothing stopped, is not touched: it is the
// recovery checkpoint's to save (#246).
func TestAbortUnfinishedLeavesOrdinaryWorkAlone(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("gh-428")
	repo.Dirty(wt)

	aborted, err := wt.AbortUnfinished()

	if err != nil || aborted != "" {
		t.Fatalf("AbortUnfinished = %q, %v; want nothing aborted", aborted, err)
	}
	if _, err := os.Stat(filepath.Join(wt.Path(), "uncommitted.txt")); err != nil || wt.Clean() {
		t.Errorf("the uncommitted work must be left in place: %v", err)
	}
}

// An abort git refuses — here its index is locked — leaves the operation
// stopped, so it is an error naming the operation, not a clean worktree.
func TestAbortUnfinishedFailsWhenTheAbortDoes(t *testing.T) {
	repo, wt, _ := conflicting(t)
	repo.UnfinishedMerge(wt, "origin/main")
	lock := filepath.Join(repo.Git(wt.Path(), "rev-parse", "--absolute-git-dir"), "index.lock")
	if err := os.WriteFile(lock, nil, 0o644); err != nil {
		t.Fatal(err)
	}

	aborted, err := wt.AbortUnfinished()

	if err == nil || aborted != "merge" || !strings.Contains(err.Error(), "merge --abort") {
		t.Fatalf("AbortUnfinished = %q, %v; want the refused merge abort", aborted, err)
	}
}
