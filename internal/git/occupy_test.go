package git_test

import (
	"os/exec"
	"path/filepath"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// lockOf is the lock the checkout's worktree list reports on path.
func lockOf(t *testing.T, r *gittest.Repo, path string) (locked bool, reason string) {
	t.Helper()
	list, err := r.Checkout().ListWorktrees()
	if err != nil {
		t.Fatal(err)
	}
	for _, w := range list {
		if filepath.Base(w.Path) == filepath.Base(path) {
			return w.Locked, w.LockReason
		}
	}
	t.Fatalf("%s is not in the worktree list", path)
	return false, ""
}

// #478: a worktree a run occupies is kept from any `git worktree remove` that is
// not forced twice, a sweep's included, and vacating it lets the run's own
// cleanup remove it.
func TestAnOccupiedWorktreeOutlivesASweepUntilItIsVacated(t *testing.T) {
	r := gittest.New(t)
	wt := r.Handoff("pr-478")
	vacate, err := wt.Occupy()
	if err != nil {
		t.Fatalf("Occupy: %v", err)
	}
	if locked, reason := lockOf(t, r, wt.Path()); !locked || reason != gitpkg.OccupiedReason {
		t.Errorf("occupied worktree lock = %v %q, want the occupied reason", locked, reason)
	}
	for _, sweep := range [][]string{{"worktree", "remove", wt.Path()}, {"worktree", "remove", "--force", wt.Path()}} {
		if out, err := exec.Command("git", append([]string{"-C", r.Dir}, sweep...)...).CombinedOutput(); err == nil {
			t.Fatalf("git %v removed an occupied worktree: %s", sweep, out)
		}
	}
	if !wt.Exists() {
		t.Fatal("the occupied worktree must still be on disk")
	}
	if err := vacate(); err != nil {
		t.Fatalf("vacate: %v", err)
	}
	if err := wt.Remove(); err != nil {
		t.Errorf("Remove after vacating: %v", err)
	}
}

// An occupancy lock a killed run left is lifted by the next run on the worktree, which
// holds its run lock; a lock someone else took is not.
func TestLiftStaleOccupancyLiftsOnlyTheHarnesssLock(t *testing.T) {
	r := gittest.New(t)
	stale, human, bare := r.Handoff("pr-1"), r.Handoff("pr-2"), r.Handoff("pr-3")
	r.Git(r.Dir, "worktree", "lock", "--reason", gitpkg.OccupiedReason, stale.Path())
	r.Git(r.Dir, "worktree", "lock", "--reason", "mine", human.Path())

	if lifted, err := stale.LiftStaleOccupancy(); err != nil || !lifted {
		t.Errorf("LiftStaleOccupancy on a killed run's lock = %v, %v; want it lifted", lifted, err)
	}
	if locked, _ := lockOf(t, r, stale.Path()); locked {
		t.Error("the killed run's lock is still there")
	}
	if lifted, err := human.LiftStaleOccupancy(); err != nil || lifted {
		t.Errorf("LiftStaleOccupancy on a human's lock = %v, %v; want it left", lifted, err)
	}
	if locked, reason := lockOf(t, r, human.Path()); !locked || reason != "mine" {
		t.Errorf("the human's lock = %v %q, want it left as it was", locked, reason)
	}
	if lifted, err := bare.LiftStaleOccupancy(); err != nil || lifted {
		t.Errorf("LiftStaleOccupancy on an unlocked worktree = %v, %v; want nothing lifted", lifted, err)
	}
	if lifted, err := r.Worktree("pr-gone").LiftStaleOccupancy(); err != nil || lifted {
		t.Errorf("LiftStaleOccupancy on no worktree = %v, %v; want nothing lifted", lifted, err)
	}
}
