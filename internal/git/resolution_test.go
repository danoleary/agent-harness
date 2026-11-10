package git_test

import (
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// resolvedPR is alice/widget checked out as the PR loop does and rebased onto an
// origin/main that moved on, saved as the resolution of the polled head, with
// the worktree removed as the run's cleanup leaves it (#252).
func resolvedPR(t *testing.T, repo *gittest.Repo) (wt gitpkg.Worktree, head, tip string) {
	t.Helper()
	head = repo.Publish("alice/widget")
	repo.Merge("other.txt", "unrelated\n", "main moved on")
	wt = repo.Checkout().WorktreeOn("pr-12", "alice/widget")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	if res := wt.Rebase(); res != gitpkg.RebaseClean {
		t.Fatalf("Rebase = %v", res)
	}
	tip, err := wt.SaveResolution(head)
	if err != nil {
		t.Fatalf("SaveResolution: %v", err)
	}
	if err := wt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	return wt, head, tip
}

// A saved resolution outlives the checkout that resets the branch to origin's
// tip, and the next run at the same head, on the same origin/main, finds it.
func TestSavedResolutionSurvivesTheNextCheckout(t *testing.T) {
	repo := gittest.New(t)
	wt, head, tip := resolvedPR(t, repo)
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	if now, _ := wt.HeadSHA(); now != head {
		t.Fatalf("the checkout starts at origin's tip %s, got %s", head, now)
	}

	got, ok := wt.SavedResolution(head)
	if !ok || got != tip {
		t.Fatalf("SavedResolution(%s) = %s, %v; want %s", head, got, ok, tip)
	}
	if err := wt.ResumeResolution(got); err != nil {
		t.Fatalf("ResumeResolution: %v", err)
	}
	if now, _ := wt.HeadSHA(); now != tip || !wt.Clean() {
		t.Errorf("the worktree must sit cleanly at the saved resolution %s, got %s", tip, now)
	}
}

// A resolution saved for another head resolves nothing the branch has now.
func TestSavedResolutionIsKeyedOnTheHeadItResolved(t *testing.T) {
	repo := gittest.New(t)
	wt, _, _ := resolvedPR(t, repo)

	if got, ok := wt.SavedResolution("0000000000000000000000000000000000000000"); ok {
		t.Errorf("a resolution of another head must not be found, got %s", got)
	}
}

// Once origin/main moves past a saved resolution's base, it is dropped.
func TestSavedResolutionOnAStaleBaseIsDropped(t *testing.T) {
	repo := gittest.New(t)
	wt, head, _ := resolvedPR(t, repo)
	repo.Merge("another.txt", "more\n", "main moved on again")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}

	if got, ok := wt.SavedResolution(head); ok {
		t.Fatalf("a resolution onto a stale origin/main must not be resumed, got %s", got)
	}
	if out := repo.Git(repo.Dir, "for-each-ref", "refs/harness/resolved/"); out != "" {
		t.Errorf("the stale resolution must be dropped, got %s", out)
	}
}
