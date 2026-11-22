package git

import (
	"slices"
	"testing"
)

// The subjects of the commits a branch gained after one of its own commits,
// oldest first: what a resumed run reads to tell whether anything landed on the
// branch since its verified handoff (#367).
func TestSubjectsSinceNamesTheCommitsAfterACommitOnTheBranch(t *testing.T) {
	repo := seedBranchRepo(t, "gh-367", "the handoff (#367)", "checkpoint(harness): recover", "more work")
	wt := Open(repo, "feat").Worktree("gh-367")
	handoff := revParse(t, repo, "feat/gh-367~2")

	got, ok := wt.SubjectsSince(handoff)

	if want := []string{"checkpoint(harness): recover", "more work"}; !ok || !slices.Equal(got, want) {
		t.Errorf("SubjectsSince = %q, %v; want %q", got, ok, want)
	}
	tip := revParse(t, repo, "feat/gh-367")
	if got, ok := wt.SubjectsSince(tip); !ok || len(got) != 0 {
		t.Errorf("SubjectsSince(tip) = %q, %v; want none, ok", got, ok)
	}
}

// A commit the branch no longer carries — rewritten by a rebase, or never on
// it — has no "since": the read is not ok rather than an empty list.
func TestSubjectsSinceACommitOffTheBranchIsNotOK(t *testing.T) {
	repo := seedBranchRepo(t, "gh-367", "the handoff (#367)")
	wt := Open(repo, "feat").Worktree("gh-367")
	runGit(t, repo, "checkout", "-q", "-b", "elsewhere", "main")
	runGit(t, repo, "commit", "-q", "--allow-empty", "-m", "off the branch")
	other := revParse(t, repo, "HEAD")

	for _, rev := range []string{other, "0123456789abcdef0123456789abcdef01234567", "", "--all"} {
		if got, ok := wt.SubjectsSince(rev); ok {
			t.Errorf("SubjectsSince(%q) = %q, ok; want not ok", rev, got)
		}
	}
}
