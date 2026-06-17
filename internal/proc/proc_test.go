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
