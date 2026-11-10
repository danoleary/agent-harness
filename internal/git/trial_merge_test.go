package git_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
)

// A trial merge is the branch's HEAD with origin/main merged in, in a throwaway
// worktree of its own: the branch, its worktree and origin are untouched, and
// removing it leaves nothing behind (#259).
func TestTrialMergeHoldsTheBranchMergedWithMainAndTouchesNothingElse(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("gh-1") // commits gh-1.txt
	tip := repo.Git(wt.Path(), "rev-parse", "HEAD")
	repo.Merge("sibling.txt", "a sibling\n", "a sibling merged")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	m, err := wt.TrialMerge("origin/main")
	if err != nil {
		t.Fatalf("TrialMerge: %v", err)
	}
	if len(m.Conflicts) != 0 {
		t.Errorf("a clean merge has no conflicts, got %v", m.Conflicts)
	}
	if m.Path() == wt.Path() {
		t.Fatal("the trial merge must not happen in the branch's own worktree")
	}
	for _, f := range []string{"gh-1.txt", "sibling.txt"} {
		if _, err := os.Stat(filepath.Join(m.Path(), f)); err != nil {
			t.Errorf("the trial merge holds both sides; %s: %v", f, err)
		}
	}
	if _, err := os.Stat(filepath.Join(wt.Path(), "sibling.txt")); err == nil {
		t.Error("the branch's own worktree must not gain main's changes")
	}
	if now := repo.Git(wt.Path(), "rev-parse", "HEAD"); now != tip {
		t.Errorf("the branch moved from %s to %s", tip, now)
	}

	if err := m.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(m.Path()); !os.IsNotExist(err) {
		t.Errorf("the trial merge's worktree is gone after Remove, stat err = %v", err)
	}
	if !wt.Clean() {
		t.Error("the branch's worktree stays clean")
	}
}

// A merge that conflicts names the conflicting paths rather than failing.
func TestTrialMergeNamesTheConflicts(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("gh-1")
	repo.Conflict("gh-1")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	m, err := wt.TrialMerge("origin/main")
	if err != nil {
		t.Fatalf("TrialMerge: %v", err)
	}
	defer func() { _ = m.Remove() }()
	if want := []string{"gh-1.txt"}; !reflect.DeepEqual(m.Conflicts, want) {
		t.Errorf("Conflicts = %v, want %v", m.Conflicts, want)
	}
}

// A trial merge a run left behind (it died mid-gate) does not block the next one.
func TestTrialMergeReplacesALeftoverOne(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("gh-1")
	repo.Merge("sibling.txt", "a sibling\n", "a sibling merged")
	repo.Git(repo.Dir, "fetch", "-q", "origin")
	if _, err := wt.TrialMerge("origin/main"); err != nil {
		t.Fatalf("first TrialMerge: %v", err)
	}

	m, err := wt.TrialMerge("origin/main")
	if err != nil {
		t.Fatalf("a leftover trial merge must not block the next: %v", err)
	}
	_ = m.Remove()
}

// A trial base is origin/main alone, in a throwaway worktree of its own, with
// nothing of the branch in it: the base a red re-gate is checked against (#250).
func TestTrialBaseHoldsMainAloneAndTouchesNothingElse(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("gh-1") // commits gh-1.txt
	tip := repo.Git(wt.Path(), "rev-parse", "HEAD")
	repo.Merge("sibling.txt", "a sibling\n", "a sibling merged")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	sha, err := wt.RefSHA("origin/main")
	if err != nil {
		t.Fatalf("RefSHA: %v", err)
	}
	if want := repo.OnOrigin("main"); sha != want {
		t.Errorf("RefSHA(origin/main) = %s, want %s", sha, want)
	}
	b, err := wt.TrialBase(sha)
	if err != nil {
		t.Fatalf("TrialBase: %v", err)
	}
	if b.Path() == wt.Path() {
		t.Fatal("the trial base must not be the branch's own worktree")
	}
	if _, err := os.Stat(filepath.Join(b.Path(), "sibling.txt")); err != nil {
		t.Errorf("the trial base holds main: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.Path(), "gh-1.txt")); err == nil {
		t.Error("the trial base must hold nothing of the branch")
	}
	if now := repo.Git(b.Path(), "rev-parse", "HEAD"); now != sha {
		t.Errorf("the trial base is at %s, want %s", now, sha)
	}
	if now := repo.Git(wt.Path(), "rev-parse", "HEAD"); now != tip {
		t.Errorf("the branch moved from %s to %s", tip, now)
	}
	if again, err := wt.TrialBase(sha); err != nil {
		t.Fatalf("a leftover trial base must not block the next: %v", err)
	} else {
		b = again
	}
	if err := b.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(b.Path()); !os.IsNotExist(err) {
		t.Errorf("the trial base's worktree is gone after Remove, stat err = %v", err)
	}
}
