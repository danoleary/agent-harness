package git_test

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
)

// A reused worktree with nothing ahead of origin/main is moved onto the main that
// has advanced since it was cut, so the session reads and gates current code (#334).
func TestFastForwardMovesAnUntouchedWorktreeOntoAdvancedMain(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-334")
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	oldBase, _ := wt.HeadSHA()
	repo.Merge("a.txt", "one\n", "a sibling lands")
	repo.Merge("b.txt", "two\n", "another sibling lands")
	newMain := repo.OnOrigin("main")

	from, to, err := wt.FastForward()
	if err != nil {
		t.Fatalf("FastForward: %v", err)
	}
	if from != oldBase || to != newMain {
		t.Errorf("FastForward = (%s, %s), want (%s, %s)", from, to, oldBase, newMain)
	}
	if head, _ := wt.HeadSHA(); head != newMain {
		t.Errorf("HEAD = %s, want current origin/main %s", head, newMain)
	}
	if base, _ := wt.BaseSHA(); base != newMain {
		t.Errorf("BaseSHA = %s, want %s: the base gate must speak for current main", base, newMain)
	}
}

// Nothing to do on a worktree already at origin/main.
func TestFastForwardIsANoOpOnACurrentWorktree(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-334")
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}

	from, to, err := wt.FastForward()
	if err != nil || from != "" || to != "" {
		t.Errorf("FastForward = (%q, %q, %v), want a no-op", from, to, err)
	}
}

// A branch with commits ahead, or a tree with uncommitted work, holds a session's
// work: FastForward leaves both where they are.
func TestFastForwardLeavesWorkAlone(t *testing.T) {
	for name, setup := range map[string]func(*gittest.Repo) string{
		"commits ahead": func(repo *gittest.Repo) string {
			repo.Handoff("gh-334")
			return ""
		},
		"uncommitted work": func(repo *gittest.Repo) string {
			wt := repo.Worktree("gh-334")
			if err := wt.Create(); err != nil {
				t.Fatalf("Create: %v", err)
			}
			repo.Dirty(wt)
			return "uncommitted.txt"
		},
	} {
		t.Run(name, func(t *testing.T) {
			repo := gittest.New(t)
			kept := setup(repo)
			wt := repo.Worktree("gh-334")
			before, _ := wt.HeadSHA()
			repo.Merge("a.txt", "one\n", "a sibling lands")

			from, to, err := wt.FastForward()
			if err != nil || from != "" || to != "" {
				t.Errorf("FastForward = (%q, %q, %v), want it to leave the work alone", from, to, err)
			}
			if head, _ := wt.HeadSHA(); head != before {
				t.Errorf("HEAD moved from %s to %s", before, head)
			}
			if kept != "" && !strings.Contains(repo.Git(wt.Path(), "status", "--porcelain"), kept) {
				t.Errorf("uncommitted %s was lost", kept)
			}
		})
	}
}
