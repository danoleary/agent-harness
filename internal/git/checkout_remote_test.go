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
	if err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("first CheckoutRemote: %v", err)
	}
	repo.Dirty(wt)

	err := wt.CheckoutRemote()

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
