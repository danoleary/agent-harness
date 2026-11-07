package git_test

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
)

// A security-alerts attempt starts from origin/main every time (ADR-0040,
// decision 3). Create re-attaches an existing local branch, which is the resume
// path a ticket wants, but here it would bring a failed attempt's commits back
// into the next session. CreateFresh resets the branch to origin/main instead.
func TestCreateFreshCutsTheBranchFromOriginMainOverAFailedAttempt(t *testing.T) {
	cases := []struct {
		name string
		// teardown leaves the failed attempt as the next run finds it.
		teardown func(*testing.T, *gittest.Repo, string)
	}{
		{"worktree still on disk", func(*testing.T, *gittest.Repo, string) {}},
		{"worktree removed, branch kept", func(t *testing.T, r *gittest.Repo, slug string) {
			if err := r.Worktree(slug).Remove(); err != nil {
				t.Fatalf("Remove: %v", err)
			}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repo := gittest.New(t)
			failed := repo.Handoff("lodash")
			tc.teardown(t, repo, "lodash")
			repo.Merge("main.txt", "main moved\n", "main moved")

			wt := repo.Worktree("lodash")
			if err := wt.CreateFresh(); err != nil {
				t.Fatalf("CreateFresh: %v", err)
			}
			if n := wt.CommitsAhead(); n != 0 {
				t.Errorf("CommitsAhead = %d, want 0: the failed attempt's commit came back", n)
			}
			head, _ := wt.HeadSHA()
			if main := repo.OnOrigin("main"); head != main {
				t.Errorf("HEAD = %s, want origin/main %s (fetched, not the stale ref)", head, main)
			}
			if wt.Branch() != failed.Branch() {
				t.Errorf("Branch = %q, want the attempt's branch %q", wt.Branch(), failed.Branch())
			}
		})
	}
}

// With nothing left behind, CreateFresh is a plain fresh worktree on origin/main.
func TestCreateFreshCreatesANewBranch(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("lodash")
	if err := wt.CreateFresh(); err != nil {
		t.Fatalf("CreateFresh: %v", err)
	}
	if !wt.Exists() || !wt.BranchExists() {
		t.Fatal("CreateFresh must leave a worktree on its branch")
	}
	if head, _ := wt.HeadSHA(); head != repo.OnOrigin("main") {
		t.Error("the new branch must be cut from origin/main")
	}
}

// A worktree holding uncommitted work is not discarded: git refuses the removal
// and CreateFresh fails, as CheckoutRemote does.
func TestCreateFreshRefusesAWorktreeWithUncommittedWork(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("lodash")
	repo.Dirty(wt)

	if err := repo.Worktree("lodash").CreateFresh(); err == nil {
		t.Fatal("CreateFresh must not discard uncommitted work")
	}
	if !wt.Exists() {
		t.Error("the dirty worktree must be left in place")
	}
}
