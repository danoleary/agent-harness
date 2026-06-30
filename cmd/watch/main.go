// Command watch is the read-only loop viewer (ADR-0005): it tails the global
// structured event stream (agent-harness/logs/loop.jsonl) the daemon and the
// single-shot pipeline write. On an interactive TTY it renders a live, redraw-on-a-
// ticker dashboard — a stateful ASCII mascot, a current-ticket panel, a stage
// indicator (n of 3), the current step plus a tool-call counter, daemon health, and
// a scrollback tail. --no-animation keeps the dashboard but freezes the mascot to a
// single static frame (reduced motion); the rest of the view still updates live.
// When stdout is not a TTY, or NO_COLOR is set, it falls back to plain scrollback
// lines (one per event, no mascot), so piping or redirecting stays clean and no ANSI
// escapes leak into a pipe.
//
// It NEVER controls the loop — `touch agent-harness/STOP` remains the only control
// path, and quitting the viewer (Ctrl-C) does not touch the daemon. It holds no
// credentials: it only reads a log file (and, for health, the daemon's pidfile via
// a signal-0 liveness probe that cannot affect the process), so it does not load
// the harness config.
//
// Path resolution, in order: a positional argument; else $HERD_PATH's
// agent-harness/logs/loop.jsonl; else logs/loop.jsonl relative to the cwd (the
// harness dir, where `make watch` runs). When the file is absent it surfaces a
// clear "loop not running" / daemon-down state and keeps polling, so starting the
// daemon later just works without restarting the viewer.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/viewer"
)

// pollInterval is how often the viewer re-reads the stream for new lines (and, in
// dashboard mode, redraws). It is a plain poll (no inotify/fsevents — stdlib only);
// sub-second so the live view feels responsive without busy-spinning.
const pollInterval = 500 * time.Millisecond

func main() {
	argv, noAnimation, noBell := parseArgs(os.Args[1:])
	path := resolvePath(argv)

	if useDashboard(isTTY(os.Stdout), os.Getenv) {
		runDashboard(path, resolvePidPath(path), resolveStopPath(path), animateFromFlag(noAnimation), bellEnabled(noBell, os.Getenv))
		return
	}
	runPlain(path, resolvePidPath(path))
}

// daemonStoppedNotice is the plain-text marker the non-TTY fallback prints when the
// daemon process goes away — the pipe/redirect analog of the dashboard's STOPPED
// banner (ADR-0006). It is deliberately reason-agnostic: a clean exit already prints
// its own "(stopped) loop — stopped: <reason>" event line just above, so this only
// has to mark that the process is now gone (the case an unclean death prints nothing).
const daemonStoppedNotice = "loop stopped — daemon process is no longer running (see logs/loop.log for why)"

// plainStoppedNotice reports the stopped notice to print this tick, and whether to
// print it: only on the transition from alive to dead, so it fires once rather than
// every tick and stays silent while the daemon runs or once it is already gone.
func plainStoppedNotice(wasAlive, isAlive bool) (string, bool) {
	if wasAlive && !isAlive {
		return daemonStoppedNotice, true
	}
	return "", false
}

// bellMark is the terminal bell (BEL): emitted once on the transition into a
// stopped/stalled state so a backgrounded `watch` still pokes an operator who walked
// away (ADR-0006). Stdlib-only, like the rest of the viewer.
const bellMark = "\a"

// runDashboard redraws the live full-screen dashboard each tick. It owns no control
// over the loop: it only reads the stream and probes the pidfile for liveness, so
// Ctrl-C (which kills this process) never touches the daemon. animate is false
// under --no-animation, which freezes the mascot to its single static frame while the
// rest of the dashboard keeps updating; tick advances the mascot's animation frame.
// When bell is set, a single BEL is emitted on each transition INTO a stopped/stalled
// state (not every tick) so an away-from-keyboard operator is alerted once.
func runDashboard(path, pidPath, stopPath string, animate, bell bool) {
	dt := viewer.NewDashboardTailer(path)
	if after, ok := stallThresholdFrom(os.Getenv); ok {
		dt.Dashboard().SetStallThreshold(after)
	}
	prevAlerting := false
	for tick := 0; ; tick++ {
		running, err := dt.Poll()
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		}
		status := viewer.DaemonStatus{
			Alive:         daemonAlive(pidPath),
			StopRequested: stopRequested(stopPath),
			StreamPresent: running,
		}
		now := time.Now()
		alerting := dt.Dashboard().Alerting(now, status)
		if bell && alerting && !prevAlerting {
			fmt.Fprint(os.Stdout, bellMark)
		}
		prevAlerting = alerting
		frame := viewer.Frame(dt.Dashboard().Render(now, status, tick, animate))
		fmt.Fprint(os.Stdout, frame)
		time.Sleep(pollInterval)
	}
}

// bellEnabled decides whether the transition bell rings: on by default, suppressed by
// --no-bell or NO_COLOR (a reduced-sensory environment that already drops the animated
// dashboard to the plain fallback — see ADR-0006). getenv is injected for test.
func bellEnabled(noBell bool, getenv func(string) string) bool {
	if noBell {
		return false
	}
	return getenv("NO_COLOR") == ""
}

// stallThresholdFrom reads an optional stall-threshold override from WATCH_STALL_AFTER
// (a Go duration string, e.g. "10m"). ok=false when unset or unparseable, leaving the
// dashboard's built-in default in place rather than forcing a value (ADR-0006).
func stallThresholdFrom(getenv func(string) string) (time.Duration, bool) {
	raw := getenv("WATCH_STALL_AFTER")
	if raw == "" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	if err != nil || d <= 0 {
		return 0, false
	}
	return d, true
}

// runPlain is the non-TTY fallback: one plain scrollback line per event, with a
// clear not-running notice on each transition to an absent stream, and a stopped
// notice on the transition to a dead daemon (ADR-0006) — the latter probed from the
// pidfile so an unclean death (no terminal record) still surfaces in a pipe. prevAlive
// starts true so a daemon already dead at attach prints the stopped notice once.
func runPlain(path, pidPath string) {
	fmt.Printf("watching %s — Ctrl-C to quit (this does not stop the loop)\n", path)
	tl := viewer.NewTailer(path)
	running := true
	prevAlive := true
	for {
		isRunning, err := tl.Poll(os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		}
		if !isRunning && running {
			fmt.Println(viewer.NotRunningMessage)
		}
		running = isRunning
		alive := daemonAlive(pidPath)
		if notice, ok := plainStoppedNotice(prevAlive, alive); ok {
			fmt.Println(notice)
		}
		prevAlive = alive
		time.Sleep(pollInterval)
	}
}

// parseArgs splits the command line into positional arguments and the two flags
// (--no-animation, --no-bell), so either flag can appear before or after the optional
// path argument. --no-bell suppresses the transition bell (ADR-0006); --no-animation
// freezes the mascot.
func parseArgs(argv []string) (positional []string, noAnimation, noBell bool) {
	for _, a := range argv {
		switch a {
		case "--no-animation":
			noAnimation = true
		case "--no-bell":
			noBell = true
		default:
			positional = append(positional, a)
		}
	}
	return positional, noAnimation, noBell
}

// useDashboard decides between the full dashboard and the plain fallback. The
// dashboard is used on an interactive TTY unless NO_COLOR is set; a non-TTY (a
// pipe/redirect) or NO_COLOR forces the plain path so escapes never reach a file or
// downstream process, and the plain path carries no mascot (ADR-0005 / AC4). Note
// --no-animation is NOT a fallback trigger: it keeps the dashboard but freezes the
// mascot to a static frame (animateFromFlag). getenv is injected for test.
func useDashboard(tty bool, getenv func(string) string) bool {
	if !tty {
		return false
	}
	return getenv("NO_COLOR") == ""
}

// animateFromFlag maps the --no-animation flag to the dashboard's animate decision:
// the flag is reduced-motion, so it renders a single static mascot frame rather than
// cycling, while the rest of the dashboard still updates live (AC2).
func animateFromFlag(noAnimation bool) bool { return !noAnimation }

// isTTY reports whether f is an interactive terminal (a character device), the
// signal that the animated dashboard has a screen to redraw on.
func isTTY(f *os.File) bool {
	fi, err := f.Stat()
	return err == nil && fi.Mode()&os.ModeCharDevice != 0
}

// resolvePath picks the stream file to tail (see the package doc for the order).
func resolvePath(argv []string) string {
	if len(argv) > 0 && argv[0] != "" {
		return argv[0]
	}
	if herd := os.Getenv("HERD_PATH"); herd != "" {
		return loopstream.PathUnder(filepath.Join(herd, "agent-harness", "logs"))
	}
	return loopstream.PathUnder("logs")
}

// resolvePidPath locates the daemon's pidfile (loop.pid, written by
// scripts/loop-start.sh) for the liveness probe. It lives at the harness root,
// beside the logs/ dir the stream is under — i.e. the grandparent of the stream
// file — so it tracks the resolved stream path without a second env lookup.
func resolvePidPath(streamPath string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(streamPath)), "loop.pid")
}

// resolveStopPath locates the STOP sentinel (agent-harness/STOP, the loop's only
// control path) for the stop-requested probe. Like the pidfile it lives at the
// harness root — the grandparent of the stream file — so the viewer can surface
// "winding down" without reading the harness config.
func resolveStopPath(streamPath string) string {
	return filepath.Join(filepath.Dir(filepath.Dir(streamPath)), "STOP")
}

// stopRequested reports whether the STOP sentinel is present — a pure existence
// probe (os.Stat), never a write: the viewer reads the operator's stop request to
// display it, but the sentinel remains the operator's to create and the daemon's to
// clear. A present sentinel (even the empty file `touch` creates) is a stop request;
// any stat error (absent, unreadable) reads as not-requested rather than a crash.
func stopRequested(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// daemonAlive reports whether the loop daemon named in pidPath is running, via a
// signal-0 probe (which checks for the process without affecting it). A missing,
// empty, or garbage pidfile, or a pid whose process is gone, reads as not-alive —
// rendered as a clear daemon-down state, never a crash. EPERM (the process exists
// but is owned by another user) still counts as alive.
func daemonAlive(pidPath string) bool {
	data, err := os.ReadFile(pidPath)
	if err != nil {
		return false
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	err = proc.Signal(syscall.Signal(0))
	return err == nil || errors.Is(err, syscall.EPERM)
}
