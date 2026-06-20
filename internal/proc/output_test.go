package proc

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOutputSeparatesStdoutAndStderr(t *testing.T) {
	stdout, stderr, err := Output(5*time.Second, "sh", "-c", "echo out; echo err >&2")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if strings.TrimSpace(string(stdout)) != "out" {
		t.Fatalf("stdout = %q, want %q", stdout, "out")
	}
	if strings.TrimSpace(string(stderr)) != "err" {
		t.Fatalf("stderr = %q, want %q", stderr, "err")
	}
}

// `gh pr checks --json` exits non-zero when checks fail, but the JSON is on
// stdout — the caller must still get it. Output returns stdout AND the exit
// error so the caller can parse the JSON and ignore the benign non-zero exit.
func TestOutputReturnsStdoutAlongsideNonZeroExitError(t *testing.T) {
	stdout, _, err := Output(5*time.Second, "sh", "-c", `printf '[{"name":"lint"}]'; exit 1`)
	if err == nil {
		t.Fatal("expected a non-zero exit error")
	}
	if !strings.Contains(string(stdout), `"name":"lint"`) {
		t.Fatalf("stdout should carry the JSON despite the non-zero exit, got %q", stdout)
	}
}

func TestOutputWrapsTimeout(t *testing.T) {
	_, _, err := Output(50*time.Millisecond, "sleep", "5")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected ErrTimeout, got %v", err)
	}
}

func TestOutputInDirRunsInGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	stdout, _, err := OutputInDir(5*time.Second, dir, "pwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(stdout)))
	if err != nil {
		t.Fatalf("could not resolve pwd output: %v", err)
	}
	want, err := filepath.EvalSymlinks(dir)
	if err != nil {
		t.Fatalf("could not resolve temp dir: %v", err)
	}
	if got != want {
		t.Fatalf("expected command to run in %q, ran in %q", want, got)
	}
}
