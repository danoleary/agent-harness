package proc

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestCombinedOutputAbortsBlockingCommandAtDeadline(t *testing.T) {
	start := time.Now()
	_, err := CombinedOutput(50*time.Millisecond, "sleep", "5")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected a timeout error, got nil")
	}
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected error to wrap ErrTimeout, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected prompt abort near the 50ms deadline, took %s", elapsed)
	}
}

func TestCombinedOutputInDirRunsInGivenDirectory(t *testing.T) {
	dir := t.TempDir()
	out, err := CombinedOutputInDir(5*time.Second, dir, "pwd")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// macOS /tmp is a symlink to /private/tmp, so compare resolved paths.
	got, err := filepath.EvalSymlinks(strings.TrimSpace(string(out)))
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

func TestCombinedOutputReturnsOutputWhenCommandFinishesInTime(t *testing.T) {
	out, err := CombinedOutput(5*time.Second, "echo", "hello")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got := string(out); got != "hello\n" {
		t.Fatalf("expected %q, got %q", "hello\n", got)
	}
}

func TestNonTimeoutFailureIsNotReportedAsTimeout(t *testing.T) {
	// `false` exits non-zero promptly; that is an ordinary failure, not a hung
	// dependency — Preflight relies on this to keep its "daemon down" message
	// distinct from a deadline kill.
	err := Run(5*time.Second, "false")
	if err == nil {
		t.Fatal("expected a non-zero exit error, got nil")
	}
	if errors.Is(err, ErrTimeout) {
		t.Fatalf("a non-zero exit must not be wrapped as ErrTimeout, got %v", err)
	}
}

func TestRunSurfacesStderrLastLineOnNonZeroExit(t *testing.T) {
	// git writes the actual cause to stderr then exits 128; the operator must see
	// the `fatal: …` line, not a bare `exit status 128` (BEH-404).
	err := Run(5*time.Second, "sh", "-c", "echo 'fatal: could not read Username' >&2; exit 128")
	if err == nil {
		t.Fatal("expected a non-zero exit error, got nil")
	}
	if !strings.Contains(err.Error(), "fatal: could not read Username") {
		t.Fatalf("expected error to surface git's stderr line, got %q", err.Error())
	}
}

func TestRunKeepsTimeoutIntactDespiteStderr(t *testing.T) {
	// A command that prints to stderr then hangs is killed at the deadline; the
	// stderr-surfacing must not clobber the ErrTimeout signal or its message —
	// callers distinguish a hung remote from an ordinary 128 (BEH-404).
	err := Run(50*time.Millisecond, "sh", "-c", "echo 'noise on stderr' >&2; sleep 5")
	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected error to wrap ErrTimeout, got %v", err)
	}
	if strings.Contains(err.Error(), "noise on stderr") {
		t.Fatalf("a deadline kill must not append stderr, got %q", err.Error())
	}
}

func TestRunSurfacesFatalRidingOnCarriageReturnProgress(t *testing.T) {
	// git overwrites its progress meter with bare \r (no \n), then writes the
	// fatal on the same physical line. The surfaced cause must be the clean fatal,
	// not the progress noise with an embedded carriage return (BEH-404).
	err := Run(5*time.Second, "sh", "-c", "printf 'Receiving objects: 100%%\\rfatal: early EOF\\n' >&2; exit 128")
	if err == nil {
		t.Fatal("expected a non-zero exit error, got nil")
	}
	if got := err.Error(); !strings.Contains(got, "fatal: early EOF") || strings.Contains(got, "Receiving") || strings.Contains(got, "\r") {
		t.Fatalf("expected the clean fatal line with no progress noise or \\r, got %q", got)
	}
}

func TestRunStaysQuietOnSuccessDespiteStderr(t *testing.T) {
	// stderr chatter on a zero exit (e.g. git's progress lines) is not a failure;
	// the happy path returns nil so the caller logs nothing (BEH-404).
	if err := Run(5*time.Second, "sh", "-c", "echo 'progress: 50%' >&2; exit 0"); err != nil {
		t.Fatalf("expected success to stay quiet, got %v", err)
	}
}

func TestRunAbortsBlockingCommandAtDeadline(t *testing.T) {
	start := time.Now()
	err := Run(50*time.Millisecond, "sleep", "5")
	elapsed := time.Since(start)

	if !errors.Is(err, ErrTimeout) {
		t.Fatalf("expected error to wrap ErrTimeout, got %v", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("expected prompt abort near the 50ms deadline, took %s", elapsed)
	}
}
