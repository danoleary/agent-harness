// Command watch is the read-only loop viewer (ADR-0005): it tails the global
// structured event stream (agent-harness/logs/loop.jsonl) the daemon and the
// single-shot pipeline write. On an interactive TTY it renders a live, redraw-on-a-
// ticker dashboard — a stateful ASCII pig, a current-ticket panel, a stage
// indicator (n of 3), the current step plus a tool-call counter, daemon health, and
// a scrollback tail. --no-animation keeps the dashboard but freezes the pig to a
// single static frame (reduced motion); the rest of the view still updates live.
// When stdout is not a TTY, or NO_COLOR is set, it falls back to plain scrollback
// lines (one per event, no pig), so piping or redirecting stays clean and no ANSI
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
	argv, noAnimation := parseArgs(os.Args[1:])
	path := resolvePath(argv)

	if useDashboard(isTTY(os.Stdout), os.Getenv) {
		runDashboard(path, resolvePidPath(path), animateFromFlag(noAnimation))
		return
	}
	runPlain(path)
}

// runDashboard redraws the live full-screen dashboard each tick. It owns no control
// over the loop: it only reads the stream and probes the pidfile for liveness, so
// Ctrl-C (which kills this process) never touches the daemon. animate is false
// under --no-animation, which freezes the pig to its single static frame while the
// rest of the dashboard keeps updating; tick advances the pig's animation frame.
func runDashboard(path, pidPath string, animate bool) {
	dt := viewer.NewDashboardTailer(path)
	for tick := 0; ; tick++ {
		if _, err := dt.Poll(); err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		}
		frame := viewer.Frame(dt.Dashboard().Render(time.Now(), daemonAlive(pidPath), tick, animate))
		fmt.Fprint(os.Stdout, frame)
		time.Sleep(pollInterval)
	}
}

// runPlain is the non-TTY fallback: one plain scrollback line per event, with a
// clear not-running notice on each transition to an absent stream.
func runPlain(path string) {
	fmt.Printf("watching %s — Ctrl-C to quit (this does not stop the loop)\n", path)
	tl := viewer.NewTailer(path)
	running := true
	for {
		isRunning, err := tl.Poll(os.Stdout)
		if err != nil {
			fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		}
		if !isRunning && running {
			fmt.Println(viewer.NotRunningMessage)
		}
		running = isRunning
		time.Sleep(pollInterval)
	}
}

// parseArgs splits the command line into positional arguments and the
// --no-animation flag (the only flag), so the flag can appear before or after the
// optional path argument.
func parseArgs(argv []string) (positional []string, noAnimation bool) {
	for _, a := range argv {
		if a == "--no-animation" {
			noAnimation = true
			continue
		}
		positional = append(positional, a)
	}
	return positional, noAnimation
}

// useDashboard decides between the full dashboard and the plain fallback. The
// dashboard is used on an interactive TTY unless NO_COLOR is set; a non-TTY (a
// pipe/redirect) or NO_COLOR forces the plain path so escapes never reach a file or
// downstream process, and the plain path carries no pig (ADR-0005 / AC4). Note
// --no-animation is NOT a fallback trigger: it keeps the dashboard but freezes the
// pig to a static frame (animateFromFlag). getenv is injected for test.
func useDashboard(tty bool, getenv func(string) string) bool {
	if !tty {
		return false
	}
	return getenv("NO_COLOR") == ""
}

// animateFromFlag maps the --no-animation flag to the dashboard's animate decision:
// the flag is reduced-motion, so it renders a single static pig frame rather than
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
