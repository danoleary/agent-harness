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

// Two renumbers that chain, 0040 → 0041 while 0041 → 0042, move each reference
// once rather than carrying ADR-0040 on to 0042.
func TestRenumberRefsMovesEachReferenceOnce(t *testing.T) {
	moved := []gitpkg.Renumber{
		{From: "0040", To: "0041", FromFile: "0040-a.md", ToFile: "0041-a.md"},
		{From: "0041", To: "0042", FromFile: "0041-b.md", ToFile: "0042-b.md"},
	}
	got := gitpkg.RenumberRefs("ADR-0040 (0040-a.md) and ADR-0041 (0041-b.md), not ADR-00401 or ADR-0012", moved)
	if want := "ADR-0041 (0041-a.md) and ADR-0042 (0042-b.md), not ADR-00401 or ADR-0012"; got != want {
		t.Errorf("RenumberRefs = %q, want %q", got, want)
	}
}

// A branch that adds two ADRs, 0041 and 0042, onto a main that then took 0041
// moves both up one, in order, and its index stays ascending: 0042 cannot keep
// its number once 0041 lands on it.
func TestRenumberCollidingADRsMovesEveryBranchADRAboveMainsHighest(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-9")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	write(t, wt.Path(), "docs/adr/0041-first.md", "# ADR-0041: first\n")
	write(t, wt.Path(), "docs/adr/0042-second.md", "# ADR-0042: second, after ADR-0041\n")
	repo.Git(wt.Path(), "add", "-A")
	repo.Git(wt.Path(), "commit", "-q", "-m", "Two decisions")
	repo.Merge("docs/adr/0041-main-decided-first.md", "# ADR-0041: main decided first\n", "a sibling's ADR")
	repo.Git(repo.Dir, "fetch", "-q", "origin")
	if r := wt.Rebase(); r != gitpkg.RebaseClean {
		t.Fatalf("Rebase = %v", r)
	}

	got, err := wt.RenumberCollidingADRs()
	if err != nil {
		t.Fatalf("RenumberCollidingADRs: %v", err)
	}
	want := []gitpkg.Renumber{
		{From: "0041", To: "0042", FromFile: "0041-first.md", ToFile: "0042-first.md"},
		{From: "0042", To: "0043", FromFile: "0042-second.md", ToFile: "0043-second.md"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("RenumberCollidingADRs = %+v, want %+v", got, want)
	}
	if b := repo.Git(wt.Path(), "show", "HEAD:docs/adr/0043-second.md"); b != "# ADR-0043: second, after ADR-0042" {
		t.Errorf("0043-second.md = %q, want both of its numbers moved once", b)
	}
	if !wt.IsRebasedOnto(gitpkg.BaseRef) {
		t.Error("the rebuilt branch must still sit on origin/main")
	}
}

// A branch with a merge in its history is not flattened: the renumber refuses
// and leaves the branch on its tip.
func TestRenumberCollidingADRsRefusesAMergeHistory(t *testing.T) {
	repo := gittest.New(t)
	wt := repo.Worktree("gh-10")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	write(t, wt.Path(), "docs/adr/0041-first.md", "# ADR-0041: first\n")
	repo.Git(wt.Path(), "add", "-A")
	repo.Git(wt.Path(), "commit", "-q", "-m", "A decision")
	repo.Merge("docs/adr/0041-main-decided-first.md", "# ADR-0041: main decided first\n", "a sibling's ADR")
	repo.Git(repo.Dir, "fetch", "-q", "origin")
	repo.Resolve(wt)
	tip, _ := wt.HeadSHA()

	if got, err := wt.RenumberCollidingADRs(); err == nil {
		t.Errorf("RenumberCollidingADRs = %+v, want a refusal", got)
	}
	if now, _ := wt.HeadSHA(); now != tip {
		t.Errorf("HEAD = %s, want the branch left on %s", now, tip)
	}
}
