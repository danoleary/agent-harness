package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

func TestUseDashboardOnlyOnInteractiveTTY(t *testing.T) {
	noEnv := func(string) string { return "" }
	withNoColor := func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	}
	for _, tc := range []struct {
		name   string
		isTTY  bool
		getenv func(string) string
		want   bool
	}{
		{"interactive tty", true, noEnv, true},
		{"not a tty (piped)", false, noEnv, false},
		{"NO_COLOR set", true, withNoColor, false},
	} {
		if got := useDashboard(tc.isTTY, tc.getenv); got != tc.want {
			t.Fatalf("%s: useDashboard=%v, want %v", tc.name, got, tc.want)
		}
	}
}

// --no-animation is a reduced-motion toggle on the dashboard (a static pig), NOT a
// reason to drop to the plain fallback: on a TTY the dashboard is still used, and
// the flag only flips the animate decision. Only a non-TTY / NO_COLOR drops to
// plain (AC4). animateFromFlag is the thin derivation the command threads to Render.
func TestNoAnimationKeepsDashboardButStopsAnimating(t *testing.T) {
	noEnv := func(string) string { return "" }
	if !useDashboard(true, noEnv) {
		t.Fatalf("a TTY must use the dashboard even with --no-animation")
	}
	if animateFromFlag(true) {
		t.Fatalf("--no-animation (noAnimation=true) must disable animation")
	}
	if !animateFromFlag(false) {
		t.Fatalf("without --no-animation, animation is on")
	}
}

func TestDaemonAliveReadsPidLiveness(t *testing.T) {
	dir := t.TempDir()

	// Absent pidfile → not alive.
	if daemonAlive(filepath.Join(dir, "nope.pid")) {
		t.Fatalf("absent pidfile must report not alive")
	}

	// A pidfile naming this very process → alive (we are obviously running).
	alivePath := filepath.Join(dir, "alive.pid")
	if err := os.WriteFile(alivePath, []byte(strconv.Itoa(os.Getpid())+"\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !daemonAlive(alivePath) {
		t.Fatalf("pidfile naming the running test process must report alive")
	}

	// A pidfile naming an almost-certainly-dead pid → not alive (not a crash).
	deadPath := filepath.Join(dir, "dead.pid")
	if err := os.WriteFile(deadPath, []byte("2147483646\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if daemonAlive(deadPath) {
		t.Fatalf("pidfile naming a dead pid must report not alive")
	}

	// Garbage pidfile → not alive, not a panic.
	junkPath := filepath.Join(dir, "junk.pid")
	if err := os.WriteFile(junkPath, []byte("not-a-pid\n"), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if daemonAlive(junkPath) {
		t.Fatalf("garbage pidfile must report not alive")
	}
}

func TestResolvePidPathIsSiblingOfLogsDir(t *testing.T) {
	// loop.pid lives at the harness root, beside the logs/ dir the stream is under.
	got := resolvePidPath(filepath.Join("some", "harness", "logs", "loop.jsonl"))
	want := filepath.Join("some", "harness", "loop.pid")
	if got != want {
		t.Fatalf("resolvePidPath = %q, want %q", got, want)
	}
}

// The STOP sentinel lives at the harness root (agent-harness/STOP) — beside loop.pid
// and the logs/ dir the stream is under — so the viewer resolves it from the stream
// path exactly as it resolves the pidfile.
func TestResolveStopPathIsSiblingOfLogsDir(t *testing.T) {
	got := resolveStopPath(filepath.Join("some", "harness", "logs", "loop.jsonl"))
	want := filepath.Join("some", "harness", "STOP")
	if got != want {
		t.Fatalf("resolveStopPath = %q, want %q", got, want)
	}
}

// stopRequested is a pure existence probe (a read, never control): a present
// sentinel — even the empty file `touch` creates — means stop was requested; an
// absent one means it was not.
func TestStopRequestedReadsSentinelPresence(t *testing.T) {
	dir := t.TempDir()

	absent := filepath.Join(dir, "STOP")
	if stopRequested(absent) {
		t.Fatalf("an absent STOP sentinel must report not requested")
	}

	if err := os.WriteFile(absent, nil, 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	if !stopRequested(absent) {
		t.Fatalf("a present (empty) STOP sentinel must report requested")
	}
}
