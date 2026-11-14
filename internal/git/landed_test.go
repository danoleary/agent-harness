package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// commitFileAt commits file in dir with subject, author and committer dated when,
// and pushes main to origin: a PR landing on the shared remote at a known time.
func commitFileAt(t *testing.T, dir, file, subject string, when time.Time) {
	t.Helper()
	path := filepath.Join(dir, file)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(subject), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	runGit(t, dir, "add", "-A")
	cmd := exec.Command("git", "-C", dir, "commit", "-q", "-m", subject)
	stamp := when.Format(time.RFC3339)
	cmd.Env = append(os.Environ(), "GIT_AUTHOR_DATE="+stamp, "GIT_COMMITTER_DATE="+stamp)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("commit: %v\n%s", err, out)
	}
	runGit(t, dir, "push", "-q", "origin", "main")
}

// #335: #317 was filed, then #318 landed the fix on main without naming #317, so
// the own-key scan saw nothing and #317 was dispatched six times. LandedSince lists
// what reached origin/main after the ticket was filed, with the files each commit
// changed, so the stage can match them against the files the ticket names.
func TestLandedSinceListsCommitsAfterTheTicketWasFiled(t *testing.T) {
	author, host := newSharedRemote(t)
	filed := time.Date(2026, 11, 14, 3, 40, 0, 0, time.UTC)

	commitFileAt(t, author, "internal/sandbox/entrypoint_trust_test.go", "Trust the session's workdir (#305) (#309)", filed.Add(-time.Hour))
	commitFileAt(t, author, "internal/sandbox/entrypoint_trust_test.go", "Resolve the temp workdir in the entrypoint trust tests, so they pass on macOS where /var is a symlink to /private/var (#318)", time.Date(2026, 11, 14, 3, 51, 48, 0, time.UTC))
	runGit(t, host, "fetch", "-q", "origin")

	got := Open(host, testPrefix).LandedSince(filed)
	if len(got) != 1 {
		t.Fatalf("want only the commit that landed after the ticket was filed, got %+v", got)
	}
	if !strings.HasSuffix(got[0].Subject, "(#318)") || len(got[0].SHA) < 7 {
		t.Errorf("want #318's short SHA and subject, got %+v", got[0])
	}
	if want := []string{"internal/sandbox/entrypoint_trust_test.go"}; !reflect.DeepEqual(got[0].Files, want) {
		t.Errorf("files = %v, want %v", got[0].Files, want)
	}
}

// A ticket whose tracker gave no filed time, and a checkout git cannot read, list
// nothing: the scan is advisory, so it fails open to silence rather than block.
func TestLandedSinceFailsOpen(t *testing.T) {
	_, host := newSharedRemote(t)
	if got := Open(host, testPrefix).LandedSince(time.Time{}); got != nil {
		t.Errorf("zero filed time: want nil, got %+v", got)
	}
	if got := Open(t.TempDir(), testPrefix).LandedSince(time.Now().Add(-time.Hour)); got != nil {
		t.Errorf("not a repo: want nil, got %+v", got)
	}
}
