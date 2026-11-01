package git_test

import (
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// twoCommits is the #164 branch: cut from origin/main and carrying two commits,
// the second adding second.txt. origin/main has not moved, so its replay should
// be a no-op apart from the commit SHAs.
func twoCommits(t *testing.T) (*gittest.Repo, gitpkg.Worktree, string) {
	t.Helper()
	repo := gittest.New(t)
	wt := repo.Handoff("gh-164")
	repo.Commit(wt, "second.txt", "more\n", "the second commit")
	tip, _ := wt.HeadSHA()
	return repo, wt, tip
}

// #164, replayed: the replay cherry-picked the first commit and then stopped on the
// second with no pick in progress. There was no content conflict, so the report
// must say it was a git failure, carry what git printed, name no conflicted paths,
// and leave the branch on its tip.
func TestRebaseDetailReportsAFailureWithNoPickInProgressAsAGitFailure(t *testing.T) {
	repo, wt, tip := twoCommits(t)
	repo.InTheWayOfReplay(wt, "second.txt", 1)

	got := wt.RebaseDetail()

	if got.Result != gitpkg.RebaseFailed {
		t.Fatalf("Result = %v, want a git failure: no paths were unmerged", got.Result)
	}
	if len(got.Conflicted) != 0 {
		t.Errorf("Conflicted = %v, want none", got.Conflicted)
	}
	if !strings.Contains(got.Output, "untracked working tree files would be overwritten") {
		t.Errorf("Output = %q, want git's own account of why the pick stopped", got.Output)
	}
	if after, _ := wt.HeadSHA(); after != tip || !wt.Clean() {
		t.Errorf("the branch must be back on its tip %s and clean, got %s (clean %v)", tip, after, wt.Clean())
	}
	// The failure was transient: the same replay, run again, applies.
	if again := wt.RebaseDetail(); again.Result != gitpkg.RebaseClean {
		t.Errorf("second replay = %+v, want clean", again)
	}
}

// A pick that halts with its change staged and nothing unmerged is not an empty
// commit. Skipping it as one would push the branch with that commit silently
// dropped.
func TestRebaseDetailNeverSkipsAHaltedPickThatIsNotEmpty(t *testing.T) {
	repo, wt, tip := twoCommits(t)
	repo.RefuseReplayedCommit(wt, "the second commit", 1)

	got := wt.RebaseDetail()

	if got.Result != gitpkg.RebaseFailed {
		t.Fatalf("Result = %v, want a git failure", got.Result)
	}
	if !strings.Contains(got.Output, "refused") {
		t.Errorf("Output = %q, want what git printed", got.Output)
	}
	if after, _ := wt.HeadSHA(); after != tip || !wt.Clean() {
		t.Errorf("the branch must be back on its tip %s with both commits, got %s (clean %v)", tip, after, wt.Clean())
	}
}

// A real content conflict still reports as one, names the unmerged file, and keeps
// what git printed.
func TestRebaseDetailReportsAContentConflictWithItsPathsAndOutput(t *testing.T) {
	repo, wt, _ := twoCommits(t)
	repo.Conflict("gh-164")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	got := wt.RebaseDetail()

	if got.Result != gitpkg.RebaseConflict {
		t.Fatalf("Result = %v, want a content conflict", got.Result)
	}
	if len(got.Conflicted) != 1 || got.Conflicted[0] != "gh-164.txt" {
		t.Errorf("Conflicted = %v, want [gh-164.txt]", got.Conflicted)
	}
	if !strings.Contains(got.Output, "CONFLICT") {
		t.Errorf("Output = %q, want git's conflict report", got.Output)
	}
}
