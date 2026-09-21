package git

import (
	"os"
	"path/filepath"
	"testing"
)

// remoteBranchesReference: a `git ls-remote --heads` line whose branch name carries
// the ticket key matches; word boundaries hold so a shorter key is not a prefix of a
// longer one.
func TestRemoteBranchesReferenceMatchesBranchName(t *testing.T) {
	out := "abc123\trefs/heads/feat/beh-677-reap-stale-claims\n" +
		"def456\trefs/heads/feat/beh-451-news-tool\n"
	if !remoteBranchesReference(out, "BEH-677") {
		t.Fatalf("expected BEH-677 to match its feature branch:\n%s", out)
	}
	if remoteBranchesReference(out, "BEH-6") {
		t.Fatalf("BEH-6 must not prefix-match beh-677")
	}
	if remoteBranchesReference(out, "BEH-999") {
		t.Fatalf("BEH-999 has no branch and must not match")
	}
}

// TicketHasRemoteBranch reports true when a branch referencing the key was pushed to
// origin — the "there is work in flight" signal that keeps the reaper from releasing
// a claim that produced a branch (even without a PR yet).
func TestTicketHasRemoteBranchFindsPushedBranch(t *testing.T) {
	author, host := newSharedRemote(t)

	runGit(t, author, "checkout", "-q", "-b", "feat/beh-677-reap-stale-claims")
	commitAndPush(t, author, "wip (BEH-677)")
	runGit(t, author, "push", "-q", "origin", "feat/beh-677-reap-stale-claims")

	if !Open(host, testPrefix).TicketHasRemoteBranch("BEH-677") {
		t.Fatalf("BEH-677's branch was pushed to origin — should be found")
	}
	if Open(host, testPrefix).TicketHasRemoteBranch("BEH-999") {
		t.Fatalf("BEH-999 has no remote branch — must not be found")
	}
}

// A git failure (no origin remote) must fail toward NOT reaping: return true so a
// flaky ls-remote can never cause a live claim to be released. The safe direction is
// the opposite of TicketAlreadyOnMain's — here a false negative would reap real work.
func TestTicketHasRemoteBranchFailsSafeWhenGitErrors(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")

	// No origin remote — ls-remote fails.
	if !Open(repo, testPrefix).TicketHasRemoteBranch("BEH-677") {
		t.Fatalf("a git failure must fail safe (true), never let the reaper release a claim on doubt")
	}
}
