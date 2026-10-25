package git_test

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/danoleary/agent-harness/internal/gittest"
)

// A claimed ticket's work in flight is what its worktree has changed since it
// left main — committed or not — and not what main has gained since (#97).
func TestChangedFilesIsTheWorktreesOwnWork(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Handoff("gh-1") // commits gh-1.txt
	repo.Dirty(wt)             // uncommitted.txt, untracked
	if err := os.WriteFile(filepath.Join(wt.Path(), "README.md"), []byte("edited\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.Merge("sibling.txt", "a sibling\n", "a sibling merged")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	got, err := wt.ChangedFiles()
	if err != nil {
		t.Fatalf("ChangedFiles: %v", err)
	}
	if want := []string{"README.md", "gh-1.txt"}; !reflect.DeepEqual(got, want) {
		t.Errorf("ChangedFiles = %v, want %v", got, want)
	}
}

// A worktree that does not exist has changed nothing.
func TestChangedFilesOfAMissingWorktreeIsEmpty(t *testing.T) {
	got, err := gittest.New(t).Worktree("gh-2").ChangedFiles()
	if err != nil || len(got) != 0 {
		t.Errorf("ChangedFiles = %v, %v; want none", got, err)
	}
}
