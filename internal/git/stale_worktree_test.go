package git_test

import (
	"os"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
)

// A worktree left holding a PR's branch blocks the PR loop's checkout (#231). It
// is safe to remove only when removing it loses nothing: a clean tree whose HEAD
// origin's tip already contains. Anything else is reported with its reason.
func TestWorktreeSafeToRemove(t *testing.T) {
	cases := []struct {
		name string
		// leave shapes the worktree after its branch was pushed, and returns
		// origin's tip for the branch.
		leave func(r *gittest.Repo, path, tip string) string
		// want is "" for safe, else a fragment of the reason.
		want string
	}{
		{"clean at origin's tip", func(_ *gittest.Repo, _, tip string) string { return tip }, ""},
		{"clean, behind origin's tip", func(r *gittest.Repo, path, tip string) string {
			r.Git(path, "commit", "-q", "--allow-empty", "-m", "pushed from elsewhere")
			r.Git(path, "push", "-q", "origin", "HEAD")
			newTip := r.Git(path, "rev-parse", "HEAD")
			r.Git(path, "reset", "-q", "--hard", tip)
			return newTip
		}, ""},
		{"readiness sentinel only", func(r *gittest.Repo, path, tip string) string {
			if err := os.WriteFile(path+"/.worktree-ready", nil, 0o644); err != nil {
				t.Fatal(err)
			}
			return tip
		}, ""},
		{"dirty", func(r *gittest.Repo, path, tip string) string {
			if err := os.WriteFile(path+"/half-a-fix.txt", []byte("wip\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			return tip
		}, "uncommitted changes"},
		{"ahead of origin", func(r *gittest.Repo, path, tip string) string {
			r.Git(path, "commit", "-q", "--allow-empty", "-m", "never pushed")
			return tip
		}, "not on origin"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := gittest.New(t)
			wt := r.Handoff("beh-1305")
			if err := wt.Push(); err != nil {
				t.Fatalf("push: %v", err)
			}
			tip := tc.leave(r, wt.Path(), r.OnOrigin(wt.Branch()))
			co := r.Checkout()

			err := co.WorktreeSafeToRemove(wt.Path(), tip)
			if tc.want != "" {
				if err == nil || !strings.Contains(err.Error(), tc.want) {
					t.Fatalf("WorktreeSafeToRemove = %v, want a reason naming %q", err, tc.want)
				}
				return
			}
			if err != nil {
				t.Fatalf("WorktreeSafeToRemove = %v, want safe", err)
			}
			if err := co.RemoveWorktree(wt.Path()); err != nil {
				t.Fatalf("RemoveWorktree: %v", err)
			}
			branches, err := co.CheckedOutBranches()
			if err != nil {
				t.Fatal(err)
			}
			if path, ok := branches[wt.Branch()]; ok || wt.Exists() {
				t.Errorf("%s still checked out at %s after removal", wt.Branch(), path)
			}
			if r.OnOrigin(wt.Branch()) == "" {
				t.Error("removing the worktree must leave the branch on origin")
			}
		})
	}
}

// The main checkout is never removed, whatever it has checked out: a human's
// branch there is theirs.
func TestTheMainCheckoutIsNeverSafeToRemove(t *testing.T) {
	r := gittest.New(t)
	tip := r.Git(r.Dir, "rev-parse", "HEAD")
	if err := r.Checkout().WorktreeSafeToRemove(r.Dir, tip); err == nil || !strings.Contains(err.Error(), "main checkout") {
		t.Errorf("WorktreeSafeToRemove(main checkout) = %v, want refused as the main checkout", err)
	}
}

// A worktree whose directory is already gone is safe: removal prunes git's record
// of it, which is all that still holds the branch.
func TestAPrunableWorktreeIsRemovedByPruning(t *testing.T) {
	r := gittest.New(t)
	wt := r.Handoff("beh-1198")
	if err := wt.Push(); err != nil {
		t.Fatalf("push: %v", err)
	}
	if err := os.RemoveAll(wt.Path()); err != nil {
		t.Fatal(err)
	}
	co := r.Checkout()
	if err := co.WorktreeSafeToRemove(wt.Path(), r.OnOrigin(wt.Branch())); err != nil {
		t.Fatalf("WorktreeSafeToRemove = %v, want safe", err)
	}
	if err := co.RemoveWorktree(wt.Path()); err != nil {
		t.Fatalf("RemoveWorktree: %v", err)
	}
	if branches, _ := co.CheckedOutBranches(); branches[wt.Branch()] != "" {
		t.Errorf("%s still held by the pruned worktree", wt.Branch())
	}
}
