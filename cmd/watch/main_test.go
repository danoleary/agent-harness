package main

import (
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
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

// parseArgs splits flags from the optional positional path. --no-animation and
// --no-bell are independent toggles that may appear before or after the path.
func TestParseArgsRecognisesNoBell(t *testing.T) {
	pos, noAnim, noBell := parseArgs([]string{"--no-bell", "logs/loop.jsonl"})
	if noBell != true || noAnim != false {
		t.Fatalf("--no-bell alone: got noBell=%v noAnim=%v", noBell, noAnim)
	}
	if len(pos) != 1 || pos[0] != "logs/loop.jsonl" {
		t.Fatalf("positional path must survive flag parsing, got %v", pos)
	}

	_, noAnim2, noBell2 := parseArgs([]string{"--no-animation", "--no-bell"})
	if !noAnim2 || !noBell2 {
		t.Fatalf("both flags must parse together, got noAnim=%v noBell=%v", noAnim2, noBell2)
	}

	_, _, noBell3 := parseArgs([]string{"logs/loop.jsonl"})
	if noBell3 {
		t.Fatalf("no flag means the bell stays enabled")
	}
}

// The transition bell is on by default, suppressed by --no-bell or NO_COLOR.
func TestBellEnabledDefaultsOnAndIsSuppressible(t *testing.T) {
	noEnv := func(string) string { return "" }
	withNoColor := func(k string) string {
		if k == "NO_COLOR" {
			return "1"
		}
		return ""
	}
	if !bellEnabled(false, noEnv) {
		t.Fatalf("the bell must default on")
	}
	if bellEnabled(true, noEnv) {
		t.Fatalf("--no-bell must suppress the bell")
	}
	if bellEnabled(false, withNoColor) {
		t.Fatalf("NO_COLOR must suppress the bell")
	}
}

// WATCH_STALL_AFTER overrides the stall threshold when it parses to a positive
// duration; otherwise ok=false leaves the dashboard's built-in default untouched.
func TestStallThresholdFromEnv(t *testing.T) {
	env := func(v string) func(string) string {
		return func(k string) string {
			if k == "WATCH_STALL_AFTER" {
				return v
			}
			return ""
		}
	}
	if d, ok := stallThresholdFrom(env("5m")); !ok || d != 5*time.Minute {
		t.Fatalf("a valid duration must be parsed, got d=%v ok=%v", d, ok)
	}
	for _, bad := range []string{"", "nonsense", "0s", "-3m"} {
		if d, ok := stallThresholdFrom(env(bad)); ok || d != 0 {
			t.Fatalf("%q must be rejected (leave the default), got d=%v ok=%v", bad, d, ok)
		}
	}
}

// The plain (non-TTY) fallback surfaces a stopped notice once, on the transition
// from alive to dead — not every tick, and not when the daemon was already dead /
// stays alive. This gives a piped/redirected consumer the "it stopped" signal the
// dashboard shows as a banner (ADR-0006); an unclean death otherwise prints nothing.
func TestPlainStoppedNoticeOnlyOnAliveToDeadTransition(t *testing.T) {
	if _, ok := plainStoppedNotice(true, true); ok {
		t.Fatalf("a still-alive daemon must not print a stopped notice")
	}
	if _, ok := plainStoppedNotice(false, false); ok {
		t.Fatalf("an already-dead daemon must not re-print the notice every tick")
	}
	msg, ok := plainStoppedNotice(true, false)
	if !ok || msg == "" {
		t.Fatalf("the alive→dead transition must print a non-empty stopped notice, got %q ok=%v", msg, ok)
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

// The STOP sentinel lives at the harness root (.agent-harness/STOP) — beside loop.pid
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
