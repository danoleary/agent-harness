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

// A stream named by path is followed with the files of the daemon that writes
// it (#106): the default ticket loop's logs/loop.jsonl sits beside loop.pid and
// STOP, a named loop's logs/loops/<name>/loop.jsonl beside loop-<name>.pid and
// STOP-<name>, and an instance's logs/instances/<id>/loop.jsonl beside
// loop-<id>.pid and STOP-<id> — each under the same .agent-harness/.
func TestAStreamPathIsFollowedWithItsDaemonsFiles(t *testing.T) {
	runtime := filepath.Join("/src/app", ".agent-harness")
	for _, tc := range []struct {
		stream string
		pid    string
		stops  []string
	}{
		{
			stream: filepath.Join(runtime, "logs", "loop.jsonl"),
			pid:    filepath.Join(runtime, "loop.pid"),
			stops:  []string{filepath.Join(runtime, "STOP-tickets"), filepath.Join(runtime, "STOP")},
		},
		{
			stream: filepath.Join(runtime, "logs", "loops", "pr-doctor", "loop.jsonl"),
			pid:    filepath.Join(runtime, "loop-pr-doctor.pid"),
			stops:  []string{filepath.Join(runtime, "STOP-pr-doctor"), filepath.Join(runtime, "STOP")},
		},
		{
			stream: filepath.Join(runtime, "logs", "instances", "b", "loop.jsonl"),
			pid:    filepath.Join(runtime, "loop-b.pid"),
			stops:  []string{filepath.Join(runtime, "STOP-b"), filepath.Join(runtime, "STOP-tickets"), filepath.Join(runtime, "STOP")},
		},
	} {
		f := followStream(tc.stream)
		if f.stream != tc.stream || f.pid != tc.pid || !equal(f.stops, tc.stops) {
			t.Errorf("followStream(%q) = %+v; want pid %q, stops %v", tc.stream, f, tc.pid, tc.stops)
		}
	}
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
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

// --instance <id> follows one named loop instance, --all follows every one; a
// bare path is still the stream to tail (#89).
func TestParseTargetReadsTheInstanceFlags(t *testing.T) {
	cases := []struct {
		argv    []string
		want    target
		wantErr bool
	}{
		{argv: nil, want: target{}},
		{argv: []string{"logs/loop.jsonl"}, want: target{path: "logs/loop.jsonl"}},
		{argv: []string{"--instance", "b"}, want: target{instance: "b"}},
		{argv: []string{"--instance=b"}, want: target{instance: "b"}},
		{argv: []string{"--all"}, want: target{all: true}},
		{argv: []string{"--loop", "pr-doctor"}, want: target{loop: "pr-doctor"}},
		{argv: []string{"--loop=pr-doctor"}, want: target{loop: "pr-doctor"}},
		{argv: []string{"--loop", "triage", "--instance", "b"}, want: target{loop: "triage", instance: "b"}},
		{argv: []string{"--loop"}, wantErr: true},
		{argv: []string{"--loop", "pr-doctor", "--all"}, wantErr: true},
		{argv: []string{"--loop", "../x"}, wantErr: true},
		{argv: []string{"--instance"}, wantErr: true},
		{argv: []string{"--instance", "b", "--all"}, wantErr: true},
	}
	for _, c := range cases {
		got, err := parseTarget(c.argv)
		if (err != nil) != c.wantErr || (!c.wantErr && got != c.want) {
			t.Errorf("parseTarget(%v) = %+v, %v; want %+v (error %v)", c.argv, got, err, c.want, c.wantErr)
		}
	}
}

// A named instance's stream, pidfile and own sentinel are the ones
// `agent-harness start --instance <id>` and the daemon write.
func TestFollowingAnInstanceReadsItsOwnFiles(t *testing.T) {
	runtime := filepath.Join("/src/app", ".agent-harness")
	f := followFor(target{instance: "b"}, runtime)
	if f.stream != filepath.Join(runtime, "logs", "instances", "b", "loop.jsonl") {
		t.Errorf("stream = %q", f.stream)
	}
	if f.pid != filepath.Join(runtime, "loop-b.pid") {
		t.Errorf("pid = %q", f.pid)
	}
	if !equal(f.stops, []string{filepath.Join(runtime, "STOP-b"), filepath.Join(runtime, "STOP-tickets"), filepath.Join(runtime, "STOP")}) {
		t.Errorf("stops = %q", f.stops)
	}

	plain := followFor(target{}, runtime)
	if plain.stream != filepath.Join(runtime, "logs", "loop.jsonl") || plain.pid != filepath.Join(runtime, "loop.pid") || plain.stops[len(plain.stops)-1] != filepath.Join(runtime, "STOP") {
		t.Errorf("the default instance moved: %+v", plain)
	}
}

// --loop <name> follows a named loop (#106): its stream under logs/loops/<name>/,
// the loop-<name>.pid `agent-harness start <name>` writes, and both sentinels
// that wind it down — its own STOP-<name> and the plain STOP — rather than files
// one dir up from its stream that nothing writes.
func TestFollowingANamedLoopReadsItsOwnFiles(t *testing.T) {
	runtime := filepath.Join("/src/app", ".agent-harness")
	f := followFor(target{loop: "pr-doctor"}, runtime)
	if f.stream != filepath.Join(runtime, "logs", "loops", "pr-doctor", "loop.jsonl") {
		t.Errorf("stream = %q", f.stream)
	}
	if f.pid != filepath.Join(runtime, "loop-pr-doctor.pid") {
		t.Errorf("pid = %q", f.pid)
	}
	if !equal(f.stops, []string{filepath.Join(runtime, "STOP-pr-doctor"), filepath.Join(runtime, "STOP")}) {
		t.Errorf("stops = %q", f.stops)
	}

	tickets := followFor(target{loop: "tickets"}, runtime)
	if tickets.stream != filepath.Join(runtime, "logs", "loop.jsonl") || tickets.pid != filepath.Join(runtime, "loop.pid") {
		t.Errorf("--loop tickets must follow the default ticket loop, got %+v", tickets)
	}

	inst := followFor(target{loop: "triage", instance: "b"}, runtime)
	if inst.pid != filepath.Join(runtime, "loop-b.pid") || !equal(inst.stops, []string{filepath.Join(runtime, "STOP-b"), filepath.Join(runtime, "STOP-triage"), filepath.Join(runtime, "STOP")}) {
		t.Errorf("instance b of triage = %+v", inst)
	}
}

// A viewer following a named loop shows the stop request whichever of its
// sentinels the operator touched, and not one meant for another loop.
func TestANamedLoopIsWindingDownOnItsOwnOrThePlainStop(t *testing.T) {
	runtime := t.TempDir()
	f := followFor(target{loop: "pr-doctor"}, runtime)
	touch := func(name string) {
		t.Helper()
		if err := os.WriteFile(filepath.Join(runtime, name), nil, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	touch("STOP-tickets")
	if anyStopRequested(f.stops) {
		t.Fatalf("STOP-tickets must not wind pr-doctor down")
	}
	touch("STOP-pr-doctor")
	if !anyStopRequested(f.stops) {
		t.Fatalf("STOP-pr-doctor must show pr-doctor winding down")
	}
	if err := os.Remove(filepath.Join(runtime, "STOP-pr-doctor")); err != nil {
		t.Fatal(err)
	}
	touch("STOP")
	if !anyStopRequested(f.stops) {
		t.Fatalf("the plain STOP must show pr-doctor winding down")
	}
}

// --all finds the default instance and every named one that has a stream dir,
// each labeled with its instance.
func TestAllInstancesFindsEveryStream(t *testing.T) {
	runtime := t.TempDir()
	for _, id := range []string{"b", "c"} {
		if err := os.MkdirAll(filepath.Join(runtime, "logs", "instances", id), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	got := allInstances(runtime)

	var labels []string
	for _, f := range got {
		labels = append(labels, f.label)
	}
	if len(got) != 3 || labels[0] != defaultInstanceLabel || labels[1] != "b" || labels[2] != "c" {
		t.Fatalf("instances = %v, want [%s b c]", labels, defaultInstanceLabel)
	}
	if got[1].stream != filepath.Join(runtime, "logs", "instances", "b", "loop.jsonl") || got[1].pid != filepath.Join(runtime, "loop-b.pid") {
		t.Errorf("instance b = %+v", got[1])
	}
}
