package git

import (
	"os"
	"path/filepath"
	"testing"
)

// pushFile commits one file at path in dir and pushes main.
func pushFile(t *testing.T, dir, path, subject string) {
	t.Helper()
	full := filepath.Join(dir, path)
	if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(full, []byte(subject), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, dir, "add", "-A")
	runGit(t, dir, "commit", "-q", "-m", subject)
	runGit(t, dir, "push", "-q", "origin", "main")
}

// MainCommitsSince is how the loop tells that the harness binary it runs lacks
// fixes main already has (#461): it counts only main's commits under the paths
// asked for, none from a commit main already contains or is behind, and a commit
// git cannot resolve is a check that did not run.
func TestMainCommitsSinceCountsMainsCommitsUnderThePaths(t *testing.T) {
	author, host := newSharedRemote(t)
	c := Open(host, testPrefix)
	build := headSHA(t, author)
	pushFile(t, author, "internal/pipeline/nowork.go", "fix: the dispatch fix")
	pushFile(t, author, "docs/adr/README.md", "docs: an ADR")
	pushFile(t, author, "cmd/loop/main.go", "fix: a loop fix")
	if err := c.FetchMain(); err != nil {
		t.Fatalf("FetchMain: %v", err)
	}

	if n, err := c.MainCommitsSince(build, "internal", "cmd"); n != 2 || err != nil {
		t.Errorf("MainCommitsSince(build) = %d, %v; want 2 harness commits, the docs one not counted", n, err)
	}
	if n, err := c.MainCommitsSince(headSHA(t, author), "internal", "cmd"); n != 0 || err != nil {
		t.Errorf("MainCommitsSince(main's tip) = %d, %v; want 0", n, err)
	}

	// A build ahead of main: a local commit main does not have yet.
	pushFile(t, author, "internal/loop/loop.go", "fix: not yet fetched")
	if n, err := c.MainCommitsSince(headSHA(t, author), "internal", "cmd"); err == nil {
		t.Errorf("MainCommitsSince(a commit the checkout never fetched) = %d, nil; want an error", n)
	}
	if err := c.FetchMain(); err != nil {
		t.Fatalf("FetchMain: %v", err)
	}
	runGit(t, author, "commit", "-q", "--allow-empty", "-m", "local, ahead of main")
	runGit(t, host, "fetch", "-q", author, "main")
	if n, err := c.MainCommitsSince(headSHA(t, author), "internal", "cmd"); n != 0 || err != nil {
		t.Errorf("MainCommitsSince(a commit ahead of main) = %d, %v; want 0", n, err)
	}

	if _, err := c.MainCommitsSince("--all"); err == nil {
		t.Errorf("MainCommitsSince(an option) = nil error; want it refused")
	}
}
