package git

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The PR loop works a branch someone else named (ADR-0019): a Worktree on it keeps
// the slug-keyed path but carries the branch it was given, not <prefix>/<slug>.
func TestWorktreeOnCarriesTheNamedBranch(t *testing.T) {
	wt := fakeCheckout(runners{}).WorktreeOn("pr-12", "alice/widget")
	if wt.Branch() != "alice/widget" {
		t.Errorf("Branch = %q, want the named branch alice/widget", wt.Branch())
	}
	if want := fakeWorktree("pr-12", runners{}).Path(); wt.Path() != want {
		t.Errorf("Path = %q, want the slug-keyed worktree path %q", wt.Path(), want)
	}
}

// CheckoutRemote puts the worktree on the branch's tip as origin has it: it fetches
// the branch into its remote-tracking ref, then adds the worktree with -B so a stale
// local branch of the same name is reset to that tip rather than reused.
func TestCheckoutRemoteFetchesTheBranchAndAddsTheWorktreeAtItsTip(t *testing.T) {
	run, calls := recordingRunner(func([]string) error { return nil })
	wt := fakeCheckout(runners{run: run}).WorktreeOn("pr-12", "alice/widget")

	if err := wt.CheckoutRemote(); err != nil {
		t.Fatalf("CheckoutRemote: %v", err)
	}
	want := [][]string{
		{"git", "-C", testRepo, "fetch", "-q", "origin", "+refs/heads/alice/widget:refs/remotes/origin/alice/widget"},
		{"git", "-C", testRepo, "worktree", "add", "-B", "alice/widget", wt.Path(), "refs/remotes/origin/alice/widget"},
	}
	if !reflect.DeepEqual(*calls, want) {
		t.Errorf("calls = %v\nwant    %v", *calls, want)
	}
}

// A branch origin no longer has (the PR was closed and its branch deleted) leaves
// nothing to check out: the fetch failure is returned and no worktree is added.
func TestCheckoutRemoteStopsWhenTheFetchFails(t *testing.T) {
	run, calls := recordingRunner(func(args []string) error {
		if hasArg(args, "fetch") {
			return errors.New("fatal: couldn't find remote ref refs/heads/alice/widget")
		}
		return nil
	})
	// The fetch is retried against transient blips under a wall-clock budget; a
	// clock that leaps past it keeps the test to one attempt.
	clock := time.Unix(0, 0)
	now := func() time.Time { clock = clock.Add(time.Hour); return clock }
	wt := fakeCheckout(runners{run: run, now: now}).WorktreeOn("pr-12", "alice/widget")

	if err := wt.CheckoutRemote(); err == nil {
		t.Fatal("a failed fetch must be returned")
	}
	for _, c := range *calls {
		if strings.Contains(strings.Join(c, " "), "worktree add") {
			t.Errorf("no worktree may be added after a failed fetch; calls = %v", *calls)
		}
	}
}
