package git_test

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// write puts body at file in dir, creating the directories it needs.
func write(t *testing.T, dir, file, body string) {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// #114, replayed: review passed the branch, then the base advanced. One file
// conflicted on the pre-push replay, and the resolution session went past it. It
// ported a test the base had just added to the branch's changed API, renumbered the
// branch's design record (whose number the base had taken) and its cross-reference,
// and rewrote the commit message. Git flagged only the one conflicted file. Every
// other edit is what the reviewed range never contained, and RewrittenSince is how
// the harness sees it.
func TestRewrittenSinceSeesWhatTheResolutionWroteBeyondTheConflict(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-114")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	write(t, wt.Path(), "api.go", "func Run(ctx Context) error\n")
	write(t, wt.Path(), "CHANGELOG.md", "- the branch's entry\n")
	write(t, wt.Path(), "docs/adr/0002-branch-record.md", "# ADR-0002: the branch's record\n")
	write(t, wt.Path(), "README.md", "a consumer\nsee ADR-0002\n")
	repo.Git(wt.Path(), "add", "-A")
	repo.Git(wt.Path(), "commit", "-q", "-m", "Change Run to take a context")
	reviewedBase, _ := wt.BaseSHA()
	reviewedHead, _ := wt.HeadSHA()

	// The base advances while the branch is in review.
	repo.Merge("CHANGELOG.md", "- a sibling's entry\n", "a sibling's changelog entry")
	repo.Merge("api_test.go", "Run()\n", "a sibling's test of Run")
	repo.Merge("docs/adr/0002-sibling-record.md", "# ADR-0002: the sibling's record\n", "a sibling's design record")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	replay, conflicted := wt.RebaseConflicts()
	if replay != gitpkg.RebaseConflict {
		t.Fatalf("replay = %v, want a conflict", replay)
	}
	if want := []string{"CHANGELOG.md"}; !reflect.DeepEqual(conflicted, want) {
		t.Fatalf("conflicted = %v, want %v: the files git reported", conflicted, want)
	}

	// The resolution session: onto origin/main, the conflict resolved, and then
	// everything past it.
	repo.Git(wt.Path(), "reset", "-q", "--hard", "origin/main")
	write(t, wt.Path(), "api.go", "func Run(ctx Context) error\n")
	write(t, wt.Path(), "CHANGELOG.md", "- a sibling's entry\n- the branch's entry\n")
	write(t, wt.Path(), "docs/adr/0003-branch-record.md", "# ADR-0003: the branch's record\n")
	write(t, wt.Path(), "README.md", "a consumer\nsee ADR-0003\n")
	write(t, wt.Path(), "api_test.go", "Run(ctx)\n")
	repo.Git(wt.Path(), "add", "-A")
	repo.Git(wt.Path(), "commit", "-q", "-m", "Change Run to take a context, and port its test")

	got, err := wt.RewrittenSince(reviewedBase, reviewedHead)
	if err != nil {
		t.Fatalf("RewrittenSince: %v", err)
	}
	want := []string{"CHANGELOG.md", "README.md", "api_test.go", "docs/adr/0002-branch-record.md", "docs/adr/0003-branch-record.md"}
	if !reflect.DeepEqual(got.Files, want) {
		t.Errorf("Files = %v, want %v (api.go replayed unchanged and is not among them)", got.Files, want)
	}
	if !got.Messages {
		t.Error("Messages = false, want true: the commit message was rewritten")
	}
	if !strings.Contains(got.RangeDiff, "port its test") || !strings.Contains(got.RangeDiff, "api_test.go") {
		t.Errorf("RangeDiff does not show the rewritten range:\n%s", got.RangeDiff)
	}
}

// A clean replay rewrites none of the branch's work, even in a file the base also
// edited: a change near the branch's moves its context and line numbers, not its
// lines.
func TestRewrittenSinceIsEmptyAfterACleanReplay(t *testing.T) {
	repo := gittest.New(t)
	repo.Merge("multi.txt", "1\n2\n3\n4\n5\n6\n7\n", "a shared file")
	wt := repo.Worktree("gh-115")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	repo.Commit(wt, "multi.txt", "1\ntwo\n3\n4\n5\n6\n7\n", "Spell out two")
	reviewedBase, _ := wt.BaseSHA()
	reviewedHead, _ := wt.HeadSHA()

	repo.Merge("multi.txt", "1\n2\n3\n4\n5\nsix\n7\n", "Spell out six")
	repo.Git(repo.Dir, "fetch", "-q", "origin")
	if replay, conflicted := wt.RebaseConflicts(); replay != gitpkg.RebaseClean || len(conflicted) != 0 {
		t.Fatalf("replay = %v %v, want clean", replay, conflicted)
	}

	got, err := wt.RewrittenSince(reviewedBase, reviewedHead)
	if err != nil {
		t.Fatalf("RewrittenSince: %v", err)
	}
	if len(got.Files) != 0 || got.Messages {
		t.Errorf("RewrittenSince = %+v, want nothing rewritten", got)
	}
}
