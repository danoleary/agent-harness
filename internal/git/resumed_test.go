package git

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// seedBranchRepo builds a temp repo with one commit on main, an origin/main
// tracking ref pinned to it, and a feat/<slug> branch carrying aheadSubjects as
// extra commits — the resumed-worktree shape ResumedBranchAdvisory reads. Pinning
// origin/main to the base is what makes `origin/main..feat/<slug>` show exactly
// the ahead commits, the BEH-554 "un-merged fix on the same branch" signal.
func seedBranchRepo(t *testing.T, slug string, aheadSubjects ...string) string {
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
	runGit(t, repo, "update-ref", "refs/remotes/origin/main", "HEAD")
	runGit(t, repo, "checkout", "-q", "-b", "feat/"+slug)
	for i, subj := range aheadSubjects {
		if err := os.WriteFile(filepath.Join(repo, fmt.Sprintf("ahead-%d", i)), []byte("y"), 0o644); err != nil {
			t.Fatalf("seed ahead commit: %v", err)
		}
		runGit(t, repo, "add", "-A")
		runGit(t, repo, "commit", "-q", "-m", subj)
	}
	return repo
}

// The whole point of BEH-554: a resumed worktree whose feat/<slug> branch already
// carries an un-merged fix commit for the very ticket being dispatched. The
// advisory must fire, naming the ticket and the branch so the session is steered
// to verify-and-handoff rather than re-implement work that's already done.
func TestResumedBranchAdvisoryWarnsWhenBranchAheadReferencesTicket(t *testing.T) {
	repo := seedBranchRepo(t, "beh-318", "fix(chat): give the GIF picker error paths (BEH-318)")

	msg := Open(repo, "feat").Worktree("beh-318").ResumedBranchAdvisory("BEH-318")
	if msg == "" {
		t.Fatal("expected an advisory for a branch already carrying an un-merged fix for the ticket")
	}
	if !strings.Contains(msg, "BEH-318") {
		t.Fatalf("advisory should name the ticket, got: %s", msg)
	}
	if !strings.Contains(msg, "feat/beh-318") {
		t.Fatalf("advisory should name the branch, got: %s", msg)
	}
}

// A fresh dispatch whose feat branch never diverged from main (the branch exists
// but sits exactly at origin/main) is real, un-started work — the guard must stay
// silent so it dispatches without noise.
func TestResumedBranchAdvisorySilentWhenBranchHasNoCommitsAhead(t *testing.T) {
	repo := seedBranchRepo(t, "beh-318") // branch created, zero commits ahead

	if msg := Open(repo, "feat").Worktree("beh-318").ResumedBranchAdvisory("BEH-318"); msg != "" {
		t.Fatalf("a branch with no commits ahead must not warn, got: %s", msg)
	}
}

// The common case: a genuinely fresh ticket whose feat/<slug> branch does not
// exist yet. The range read errors, and the guard must fail quiet rather than
// imply anything about un-started work.
func TestResumedBranchAdvisorySilentWhenBranchAbsent(t *testing.T) {
	repo := t.TempDir()
	runGit(t, repo, "init", "-q", "-b", "main")
	runGit(t, repo, "config", "user.email", "test@example.com")
	runGit(t, repo, "config", "user.name", "Test")
	if err := os.WriteFile(filepath.Join(repo, "f"), []byte("x"), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, repo, "add", "-A")
	runGit(t, repo, "commit", "-q", "-m", "init")

	if msg := Open(repo, "feat").Worktree("beh-318").ResumedBranchAdvisory("BEH-318"); msg != "" {
		t.Fatalf("an absent branch must fail quiet (no advisory), got: %s", msg)
	}
}

// Precision: a branch can carry un-merged commits that belong to *other* work
// (e.g. a slug collision, or commits cherry-picked in). The advisory is keyed on
// the dispatched ticket's key, so commits that don't reference it must not fire —
// matched on word boundaries so BEH-318 never matches BEH-3180.
func TestResumedBranchAdvisorySilentWhenAheadCommitsReferenceOtherTicket(t *testing.T) {
	repo := seedBranchRepo(t, "beh-318", "chore: unrelated work (BEH-999)", "fixup BEH-3180 typo")

	if msg := Open(repo, "feat").Worktree("beh-318").ResumedBranchAdvisory("BEH-318"); msg != "" {
		t.Fatalf("ahead commits not referencing the ticket must not warn, got: %s", msg)
	}
}
