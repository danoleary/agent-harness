package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// seedRepo creates a temp git repo with one initial commit and returns its path.
func seedRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")
	return repo
}

func TestHeadSHAReadsCurrentHead(t *testing.T) {
	repo := seedRepo(t)

	first, err := realWorktree(repo, repo, "beh-561").HeadSHA()
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	if first == "" {
		t.Fatal("HeadSHA returned empty sha for a repo with a commit")
	}

	if err := os.WriteFile(filepath.Join(repo, "g"), []byte("y"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "second")

	second, err := realWorktree(repo, repo, "beh-561").HeadSHA()
	if err != nil {
		t.Fatalf("HeadSHA after commit: %v", err)
	}
	if second == first {
		t.Fatalf("HeadSHA did not move after a new commit (%s)", second)
	}
}

func TestHeadSHAErrorsOutsideRepo(t *testing.T) {
	if _, err := realWorktree(t.TempDir(), t.TempDir(), "beh-561").HeadSHA(); err == nil {
		t.Fatal("HeadSHA should error in a non-git directory")
	}
}

// The escape hatch: an auto-fix session that reproduced the gates locally and
// found no code defect leaves the worktree clean with HEAD unmoved. Rather than
// forcing the agent to fabricate a speculative diff, the harness adds an empty
// commit so the re-push gives CI a fresh HEAD to re-run against (BEH-561).
func TestEnsureCIRerunCommitAddsEmptyCommitWhenHeadUnmoved(t *testing.T) {
	repo := seedRepo(t)
	headBefore, err := realWorktree(repo, repo, "beh-561").HeadSHA()
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	before := commitCount(t, repo)

	if err := realWorktree(repo, repo, "beh-561").EnsureCIRerunCommit(headBefore); err != nil {
		t.Fatalf("EnsureCIRerunCommit: %v", err)
	}

	if got := commitCount(t, repo); got != before+1 {
		t.Fatalf("expected exactly one new (empty) commit, count %d → %d", before, got)
	}
	if !realWorktree(repo, repo, "beh-561").Clean() {
		t.Fatal("worktree should stay clean after an empty commit")
	}
	// It must be empty: the new commit's tree equals its parent's tree.
	headTree := revParse(t, repo, "HEAD^{tree}")
	parentTree := revParse(t, repo, "HEAD~1^{tree}")
	if headTree != parentTree {
		t.Fatalf("re-trigger commit was not empty: tree %s != parent tree %s", headTree, parentTree)
	}
}

// BEH-579: the empty CI-rerun commit is made host-side against a repo whose
// config may carry the placeholder `Test <test@example.com>` identity. It ships
// to the PR, so it must stamp the harness bot identity (author AND committer)
// rather than leak the placeholder into history.
func TestEnsureCIRerunCommitStampsHarnessIdentity(t *testing.T) {
	repo := seedRepo(t) // seeded with the Test placeholder config
	headBefore, err := realWorktree(repo, repo, "beh-561").HeadSHA()
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}

	if err := realWorktree(repo, repo, "beh-561").EnsureCIRerunCommit(headBefore); err != nil {
		t.Fatalf("EnsureCIRerunCommit: %v", err)
	}

	want := HarnessAuthorName + "|" + HarnessAuthorEmail + "|" + HarnessAuthorName + "|" + HarnessAuthorEmail
	if got := commitIdentity(t, repo); got != want {
		t.Fatalf("CI-rerun identity = %q, want %q (the placeholder Test identity must not leak)", got, want)
	}
}

// When the agent committed a real fix (HEAD moved since the session started), the
// harness must NOT pile an empty commit on top — the fix is what should re-trigger
// CI. EnsureCIRerunCommit is a no-op in that case.
func TestEnsureCIRerunCommitIsNoOpWhenHeadMoved(t *testing.T) {
	repo := seedRepo(t)
	headBefore, err := realWorktree(repo, repo, "beh-561").HeadSHA()
	if err != nil {
		t.Fatalf("HeadSHA: %v", err)
	}
	// Simulate the agent committing a real fix.
	if err := os.WriteFile(filepath.Join(repo, "fix"), []byte("real"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "real fix")
	after := commitCount(t, repo)

	if err := realWorktree(repo, repo, "beh-561").EnsureCIRerunCommit(headBefore); err != nil {
		t.Fatalf("EnsureCIRerunCommit: %v", err)
	}

	if got := commitCount(t, repo); got != after {
		t.Fatalf("expected no extra commit on top of a real fix, count %d → %d", after, got)
	}
}

// A pre-session HeadSHA read failure leaves headBefore empty. That must degrade
// to "treat any HEAD as a real commit" — i.e. NEVER manufacture a spurious empty
// commit on top of an unknown baseline — rather than falsely concluding HEAD was
// unmoved (BEH-561).
func TestEnsureCIRerunCommitNoOpWhenHeadBeforeUnknown(t *testing.T) {
	repo := seedRepo(t)
	before := commitCount(t, repo)

	if err := realWorktree(repo, repo, "beh-561").EnsureCIRerunCommit(""); err != nil {
		t.Fatalf("EnsureCIRerunCommit: %v", err)
	}

	if got := commitCount(t, repo); got != before {
		t.Fatalf("expected no commit when headBefore is unknown, count %d → %d", before, got)
	}
}

func revParse(t *testing.T, repo, rev string) string {
	t.Helper()
	out, err := exec.Command("git", "-C", repo, "rev-parse", rev).Output()
	if err != nil {
		t.Fatalf("rev-parse %s: %v", rev, err)
	}
	return strings.TrimSpace(string(out))
}

// #86: BaseSHA is the commit the feature branch was cut from (or last rebased
// onto) — its merge-base with origin/main — not local `main`, which the harness
// never moves. Here local main stays at init while origin/main gains a sibling
// ticket's merge, the branch is cut from that, and origin/main then advances again
// mid-run: the base is still the sibling's merge.
func TestBaseSHAIsTheForkPointFromOriginMainNotLocalMain(t *testing.T) {
	repo := seedRepo(t)
	commit := func(name string) string {
		t.Helper()
		if err := os.WriteFile(filepath.Join(repo, name), []byte(name), 0o644); err != nil {
			t.Fatalf("write: %v", err)
		}
		runGit(t, repo, "add", "-A")
		runGit(t, repo, "commit", "-q", "-m", name)
		sha, err := realWorktree(repo, repo, "gh-86").HeadSHA()
		if err != nil {
			t.Fatalf("HeadSHA: %v", err)
		}
		return sha
	}
	runGit(t, repo, "checkout", "-q", "-b", "upstream")
	sibling := commit("sibling")
	runGit(t, repo, "checkout", "-q", "-b", branchName("gh-86"))
	commit("feature")
	runGit(t, repo, "checkout", "-q", "upstream")
	commit("later-sibling")
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", "upstream")
	runGit(t, repo, "checkout", "-q", "main")

	got, err := realWorktree(repo, repo, "gh-86").BaseSHA()
	if err != nil {
		t.Fatalf("BaseSHA: %v", err)
	}
	if got != sibling {
		t.Fatalf("BaseSHA = %s, want the fork point %s", got, sibling)
	}
}

func TestBaseSHAErrorsWithoutOriginMain(t *testing.T) {
	if _, err := realWorktree(seedRepo(t), t.TempDir(), "gh-86").BaseSHA(); err == nil {
		t.Fatal("BaseSHA should error when the branch and origin/main do not resolve")
	}
}
