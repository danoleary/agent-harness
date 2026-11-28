package git_test

import (
	"strings"
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
	if !ok || got.Tip != tip || got.Stale {
		t.Fatalf("SavedResolution(%s) = %+v, %v; want %s, current", head, got, ok, tip)
	}
	if err := wt.ResumeResolution(got.Tip); err != nil {
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
		t.Errorf("a resolution of another head must not be found, got %+v", got)
	}
}

// moveMain lands a commit on origin's main and fetches it into the checkout.
func moveMain(t *testing.T, repo *gittest.Repo, file, body string) string {
	t.Helper()
	repo.Merge(file, body, "main moved on again")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}
	return repo.Git(repo.Dir, "rev-parse", "origin/main")
}

// #479: once origin/main moves past a saved resolution's base, it is found as
// stale, naming the base it was resolved onto, and kept for a replay rather than
// dropped.
func TestSavedResolutionOnAStaleBaseIsKeptAsStale(t *testing.T) {
	repo := gittest.New(t)
	wt, head, tip := resolvedPR(t, repo)
	base := repo.Git(repo.Dir, "rev-parse", "origin/main")
	moveMain(t, repo, "another.txt", "more\n")

	got, ok := wt.SavedResolution(head)
	if !ok || got.Tip != tip || !got.Stale || got.Base != base {
		t.Fatalf("SavedResolution(%s) = %+v, %v; want stale %s on %s", head, got, ok, tip, base)
	}
	if out := repo.Git(repo.Dir, "for-each-ref", "refs/harness/resolved/"); out == "" {
		t.Error("the stale resolution must be kept for the replay")
	}
}

// #479: a stale saved resolution whose base moved by something it does not touch
// replays onto origin/main as it stands, carrying the resolved commits with it.
func TestReplayResolutionCarriesAStaleResolutionOntoTheNewBase(t *testing.T) {
	repo := gittest.New(t)
	wt, head, _ := resolvedPR(t, repo)
	main := moveMain(t, repo, "another.txt", "more\n")
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	saved, _ := wt.SavedResolution(head)

	if r := wt.ReplayResolution(saved.Tip, false); r.Result != gitpkg.RebaseClean {
		t.Fatalf("ReplayResolution = %+v, want clean", r)
	}
	if !wt.IsRebasedOnto(gitpkg.BaseRef) || !wt.Clean() {
		t.Errorf("the replayed resolution must sit cleanly on origin/main %s", main)
	}
	if got := repo.Git(wt.Path(), "show", "HEAD:alice-widget.txt"); got != "a change" {
		t.Errorf("the replay must carry the resolved change, got %q", got)
	}
}

// #479: a stale saved resolution that origin/main's new commits conflict with
// leaves the branch where it was, naming the paths git stopped on.
func TestReplayResolutionThatConflictsLeavesTheBranchAtItsHead(t *testing.T) {
	repo := gittest.New(t)
	wt, head, _ := resolvedPR(t, repo)
	moveMain(t, repo, "alice-widget.txt", "main's change\n")
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	saved, _ := wt.SavedResolution(head)

	r := wt.ReplayResolution(saved.Tip, false)
	if r.Result != gitpkg.RebaseConflict || strings.Join(r.Conflicted, ",") != "alice-widget.txt" {
		t.Fatalf("ReplayResolution = %+v, want a conflict in alice-widget.txt", r)
	}
	if now, _ := wt.HeadSHA(); now != head || !wt.Clean() {
		t.Errorf("the branch must be back cleanly on its head %s, got %s", head, now)
	}
}

// #479: a human's branch's saved resolution is a merge; its replay merges the
// new origin/main into it rather than rewriting it.
func TestReplayResolutionMergesTheNewBaseIntoAMergeResolution(t *testing.T) {
	repo := gittest.New(t)
	head := repo.Publish("alice/widget")
	repo.Merge("other.txt", "unrelated\n", "main moved on")
	wt := repo.Checkout().WorktreeOn("pr-12", "alice/widget")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	repo.Resolve(wt)
	tip, err := wt.SaveResolution(head)
	if err != nil {
		t.Fatalf("SaveResolution: %v", err)
	}
	moveMain(t, repo, "another.txt", "more\n")
	repo.Git(wt.Path(), "reset", "-q", "--hard", head)

	if r := wt.ReplayResolution(tip, true); r.Result != gitpkg.RebaseClean {
		t.Fatalf("ReplayResolution = %+v, want clean", r)
	}
	if !wt.IsRebasedOnto(gitpkg.BaseRef) {
		t.Error("origin/main must be in the replayed merge")
	}
	if parent := repo.Git(wt.Path(), "rev-parse", "HEAD^1"); parent != tip {
		t.Errorf("the replay must merge on top of the saved merge %s, not rewrite it; first parent %s", tip, parent)
	}
}
