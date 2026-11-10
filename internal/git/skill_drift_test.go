package git_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// The host checkout's `.claude/` tree drifts from the base when main gains a skill
// change the checkout has not pulled — the #245 incident, where the checkout sat
// on a side branch whose /tdd skill predated the base commit's — or when the
// operator edits a skill by hand. A ticket's worktree under `.claude/worktrees/`
// is not part of that tree: it is untracked.
func TestClaudeDriftNamesTheSkillsTheCheckoutHasNotPulled(t *testing.T) {
	repo := gittest.New(t)
	repo.Handoff("gh-1")
	repo.Merge(".claude/skills/tdd/SKILL.md", "commit before the full gate\n", "reorder /tdd's Finish")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	got, err := repo.Checkout().ClaudeDrift(gitpkg.BaseRef)
	if err != nil {
		t.Fatalf("ClaudeDrift: %v", err)
	}
	if want := []string{".claude/skills/tdd/SKILL.md"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ClaudeDrift = %v, want %v", got, want)
	}

	repo.Git(repo.Dir, "pull", "-q", "--ff-only", "origin", "main")
	if got, err := repo.Checkout().ClaudeDrift(gitpkg.BaseRef); err != nil || len(got) != 0 {
		t.Errorf("after pulling, ClaudeDrift = %v, %v; want none", got, err)
	}

	if err := os.WriteFile(filepath.Join(repo.Dir, ".claude", "skills", "tdd", "SKILL.md"), []byte("a local edit\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got, _ := repo.Checkout().ClaudeDrift(gitpkg.BaseRef); len(got) != 1 {
		t.Errorf("with an uncommitted skill edit, ClaudeDrift = %v, want the edited skill", got)
	}
}

// A base the checkout cannot resolve is an error, not a clean tree.
func TestClaudeDriftAgainstAnUnknownBaseIsAnError(t *testing.T) {
	if _, err := gittest.New(t).Checkout().ClaudeDrift("origin/no-such-branch"); err == nil {
		t.Error("ClaudeDrift against an unknown ref = nil error")
	}
}
