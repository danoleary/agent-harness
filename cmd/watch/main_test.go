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
		name        string
		isTTY       bool
		noAnimation bool
		getenv      func(string) string
		want        bool
	}{
		{"interactive tty", true, false, noEnv, true},
		{"not a tty (piped)", false, false, noEnv, false},
		{"no-animation flag", true, true, noEnv, false},
		{"NO_COLOR set", true, false, withNoColor, false},
	} {
		if got := useDashboard(tc.isTTY, tc.noAnimation, tc.getenv); got != tc.want {
			t.Fatalf("%s: useDashboard=%v, want %v", tc.name, got, tc.want)
		}
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
