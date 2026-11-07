package docs

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// #235, replayed: #211 appended ADR-0041's line to the ADR index while #197's
// ADR-0040 line had just landed on main at the same spot. The numbers did not
// clash, but Landing's replay stopped on a content conflict in the index and cost a
// resolution session and a second Gate run to keep both lines. With the repo's
// .gitattributes the same replay goes through, keeping both lines in order.
//
// GitHub's merge ignores .gitattributes, so two PRs that took the same number
// still conflict there (ADR-0034); only a local replay unions the index.
func TestTwoADRsAppendedAtOnceReplayCleanlyOntoTheIndex(t *testing.T) {
	root := repoRoot(t)
	attrs, err := os.ReadFile(filepath.Join(root, ".gitattributes"))
	if err != nil {
		t.Fatalf("the repo has no .gitattributes: %v", err)
	}
	const (
		index = "docs/adr/README.md"
		adr40 = "- [ADR-0040](0040-open-security-alerts-are-fixed-by-a-loop-that-opens-its-own-pr.md): Open security alerts are fixed by a loop that opens its own PR\n"
		adr41 = "- [ADR-0041](0041-the-host-fetches-the-sentry-issues-a-ticket-cites-into-the-prompt.md): The host fetches the Sentry issues a ticket cites into the prompt\n"
	)
	base := indexBefore(t, filepath.Join(root, index), "- [ADR-0040]")

	repo := gittest.New(t)
	repo.Merge(".gitattributes", string(attrs), "mark the index merge=union")
	repo.Merge(index, base, "the index #197 and #211 were cut from")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}
	wt := repo.Worktree("gh-211")
	if err := wt.Create(); err != nil {
		t.Fatal(err)
	}
	repo.Commit(wt, index, base+adr41, "#211: add ADR-0041")
	repo.Merge(index, base+adr40, "#197: add ADR-0040")
	if err := repo.Checkout().FetchMain(); err != nil {
		t.Fatal(err)
	}

	if got := wt.RebaseDetail(); got.Result != gitpkg.RebaseClean {
		t.Fatalf("replay = %v (conflicted %v), want clean:\n%s", got.Result, got.Conflicted, got.Output)
	}
	b, err := os.ReadFile(filepath.Join(wt.Path(), index))
	if err != nil {
		t.Fatal(err)
	}
	if want := base + adr40 + adr41; string(b) != want {
		t.Errorf("replayed index =\n%s\nwant both lines, in number order:\n%s", b, want)
	}
}

// indexBefore is the index at path cut off before the line starting with entry:
// the index as it stood before that ADR and every later one were added.
func indexBefore(t *testing.T, path, entry string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	head, _, ok := strings.Cut(string(b), "\n"+entry)
	if !ok {
		t.Fatalf("%s has no %q line", path, entry)
	}
	return head + "\n"
}
