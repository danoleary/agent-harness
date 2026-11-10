package git_test

import (
	"reflect"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// #267, replayed: the PR added ADR-0040, and main then merged an unrelated
// ADR-0040. The conflict-resolution session rebased onto main and renumbered the
// branch's ADR to the next free number, 0041, keeping its slug. RenumberedADRs
// reads that off the branch's own work before and after, so the harness can fix
// the PR's description; main's ADR, which the branch never added, is not a
// renumber.
func TestRenumberedADRsSeesTheBranchsADRMovedToTheNextFreeNumber(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-7")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	write(t, wt.Path(), "docs/adr/0040-a-pr-records-its-decision.md", "# ADR-0040: a PR records its decision\n")
	write(t, wt.Path(), "README.md", "see ADR-0040\n")
	repo.Git(wt.Path(), "add", "-A")
	repo.Git(wt.Path(), "commit", "-q", "-m", "Record the decision (ADR-0040)")
	head, _ := wt.HeadSHA()

	repo.Merge("docs/adr/0040-main-decided-first.md", "# ADR-0040: main decided first\n", "a sibling's ADR")
	repo.Git(repo.Dir, "fetch", "-q", "origin")

	// The resolution session.
	repo.Git(wt.Path(), "reset", "-q", "--hard", "origin/main")
	write(t, wt.Path(), "docs/adr/0041-a-pr-records-its-decision.md", "# ADR-0041: a PR records its decision\n")
	write(t, wt.Path(), "README.md", "see ADR-0041\n")
	repo.Git(wt.Path(), "add", "-A")
	repo.Git(wt.Path(), "commit", "-q", "-m", "Record the decision (ADR-0041)")

	got, err := wt.RenumberedADRs(head)
	if err != nil {
		t.Fatalf("RenumberedADRs: %v", err)
	}
	want := []gitpkg.Renumber{{
		From: "0040", To: "0041",
		FromFile: "0040-a-pr-records-its-decision.md", ToFile: "0041-a-pr-records-its-decision.md",
	}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RenumberedADRs = %+v, want %+v", got, want)
	}
}

// A resolution that kept the branch's ADR number renumbered nothing, and a branch
// that adds no ADR has nothing to renumber.
func TestRenumberedADRsIsEmptyWhenTheNumberStands(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-8")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	write(t, wt.Path(), "docs/adr/0040-a-pr-records-its-decision.md", "# ADR-0040\n")
	write(t, wt.Path(), "code.go", "package x\n")
	repo.Git(wt.Path(), "add", "-A")
	repo.Git(wt.Path(), "commit", "-q", "-m", "Record the decision")
	head, _ := wt.HeadSHA()

	repo.Merge("other.txt", "unrelated\n", "main moved on")
	repo.Git(repo.Dir, "fetch", "-q", "origin")
	repo.Resolve(wt)

	got, err := wt.RenumberedADRs(head)
	if err != nil {
		t.Fatalf("RenumberedADRs: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("RenumberedADRs = %+v, want none", got)
	}
}
