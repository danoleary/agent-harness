package git_test

import (
	"errors"
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// A worktree the last run left dirty makes git refuse to drop it (#247): that is
// the host's local state, not the remote's, so the error says so, naming the
// worktree and carrying git's own words.
func TestCheckoutRemoteNamesALocalStateRefusal(t *testing.T) {
	repo := gittest.New(t)
	repo.Publish("alice/widget")
	wt := repo.Checkout().WorktreeOn("pr-12", "alice/widget")
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("first CheckoutRemote: %v", err)
	}
	repo.Dirty(wt)

	_, err := wt.CheckoutRemote()

	var local *gitpkg.LocalStateError
	if !errors.As(err, &local) || local.Path != wt.Path() {
		t.Fatalf("CheckoutRemote = %v, want a LocalStateError on %s", err, wt.Path())
	}
	if !strings.Contains(err.Error(), wt.Path()) || !strings.Contains(err.Error(), "modified or untracked files") {
		t.Errorf("the error must name the worktree and git's error: %v", err)
	}
	if !wt.Exists() {
		t.Error("the dirty worktree must be left in place")
	}
}

// checkpointed is alice/widget checked out as the PR loop does, with a run's
// leftover work saved as a recovery checkpoint and the worktree removed, as the
// run's cleanup leaves it (#246). It returns the checkpoint and origin's tip.
func checkpointed(t *testing.T, repo *gittest.Repo, wt gitpkg.Worktree) (checkpoint, tip string) {
	t.Helper()
	tip = repo.Publish("alice/widget")
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("first CheckoutRemote: %v", err)
	}
	repo.Dirty(wt)
	if err := wt.Checkpoint("PR-12", "pr-fix"); err != nil {
		t.Fatalf("Checkpoint: %v", err)
	}
	checkpoint, _ = wt.HeadSHA()
	if err := wt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	return checkpoint, tip
}

// #246: a checkpoint on origin's tip is where the next checkout starts, and the
// checkout says so, naming the checkpoint and the tip under it.
func TestCheckoutRemoteResumesACheckpointOnOriginsTip(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Checkout().WorktreeOn("pr-12", "alice/widget")
	checkpoint, tip := checkpointed(t, repo, wt)

	resumed, err := wt.CheckoutRemote()

	if err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	if resumed != (gitpkg.Resumed{Checkpoint: checkpoint, Base: tip}) {
		t.Errorf("resumed = %+v, want checkpoint %s on %s", resumed, checkpoint, tip)
	}
	if head, _ := wt.HeadSHA(); head != checkpoint || !wt.Clean() {
		t.Errorf("the worktree is at %s (clean %v), want the checkpoint %s", head, wt.Clean(), checkpoint)
	}
}

// A checkpoint whose head origin has since moved past was cut from a head that is
// gone: the checkout starts from origin's new tip, as before #246.
func TestCheckoutRemoteDropsACheckpointOriginMovedPast(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Checkout().WorktreeOn("pr-12", "alice/widget")
	_, tip := checkpointed(t, repo, wt)
	other := repo.Checkout().WorktreeOn("other", "alice/widget-2")
	repo.Git(repo.Dir, "worktree", "add", "-q", "-b", "alice/widget-2", other.Path(), tip)
	repo.Commit(other, "pushed.txt", "a human's push\n", "a push since")
	moved := repo.Git(other.Path(), "rev-parse", "HEAD")
	repo.Git(other.Path(), "push", "-q", "origin", "HEAD:alice/widget")

	resumed, err := wt.CheckoutRemote()

	if err != nil || resumed != (gitpkg.Resumed{}) {
		t.Fatalf("CheckoutRemote = %+v, %v; want origin's tip, not the stale checkpoint", resumed, err)
	}
	if head, _ := wt.HeadSHA(); head != moved {
		t.Errorf("the worktree is at %s, want origin's new tip %s", head, moved)
	}
}

// Only a checkpoint is resumed: a run's own unpushed commit — a fix the gates
// refused — is reset to origin's tip, as before #246.
func TestCheckoutRemoteResetsAnUnpushedCommitThatIsNoCheckpoint(t *testing.T) {
	repo := gittest.New(t)
	tip := repo.Publish("alice/widget")
	wt := repo.Checkout().WorktreeOn("pr-12", "alice/widget")
	if _, err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("first CheckoutRemote: %v", err)
	}
	repo.Commit(wt, "fix.txt", "a refused fix\n", "Fix the build")
	if err := wt.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}

	resumed, err := wt.CheckoutRemote()

	if err != nil || resumed != (gitpkg.Resumed{}) {
		t.Fatalf("CheckoutRemote = %+v, %v; want origin's tip", resumed, err)
	}
	if head, _ := wt.HeadSHA(); head != tip {
		t.Errorf("the worktree is at %s, want origin's tip %s", head, tip)
	}
}

// A checkpoint origin already has — the ticket loop pushes a branch whose tip is
// one — is no unpushed work: the checkout is at origin's tip, and resumes nothing.
func TestCheckoutRemoteDoesNotResumeAPushedCheckpoint(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Checkout().WorktreeOn("pr-12", "alice/widget")
	checkpoint, _ := checkpointed(t, repo, wt)
	repo.Git(repo.Dir, "push", "-q", "origin", "alice/widget")

	resumed, err := wt.CheckoutRemote()

	if err != nil || resumed != (gitpkg.Resumed{}) {
		t.Fatalf("CheckoutRemote = %+v, %v; want a plain checkout at origin's tip", resumed, err)
	}
	if head, _ := wt.HeadSHA(); head != checkpoint {
		t.Errorf("the worktree is at %s, want origin's tip %s", head, checkpoint)
	}
}
