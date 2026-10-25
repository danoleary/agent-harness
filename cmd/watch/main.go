// Command watch is the read-only loop viewer (ADR-0005): it tails the global
// structured event stream (.agent-harness/logs/loop.jsonl) the daemon and the
// single-shot pipeline write. On an interactive TTY it renders a live, redraw-on-a-
// ticker dashboard — a stateful ASCII mascot, a current-ticket panel, a stage
// indicator (n of 3), the current step plus a tool-call counter, daemon health, and
// a scrollback tail. --no-animation keeps the dashboard but freezes the mascot to a
// single static frame (reduced motion); the rest of the view still updates live.
// When stdout is not a TTY, or NO_COLOR is set, it falls back to plain scrollback
// lines (one per event, no mascot), so piping or redirecting stays clean and no ANSI
// escapes leak into a pipe.
//
// It NEVER controls the loop — `touch`ing a STOP sentinel remains the only control
// path, and quitting the viewer (Ctrl-C) does not touch the daemon. It holds no
// credentials: it only reads a log file (and, for health, the daemon's pidfile via
// a signal-0 liveness probe that cannot affect the process), so it does not load
// the harness config.
//
// Path resolution, in order: a positional argument; else $PROJECT_PATH's
// .agent-harness/logs/loop.jsonl; else logs/loop.jsonl relative to the cwd (the
// harness dir, where `make watch` runs). `--loop <name>` follows a named loop
// instead (its logs/loops/<name>/loop.jsonl, loop-<name>.pid and STOP-<name>,
// #106), `--instance <id>` one named loop instance (its
// logs/instances/<id>/loop.jsonl, loop-<id>.pid and STOP-<id>), and `--all`
// every instance at once in the plain view, each line labeled with its instance
// (#89). The pidfile and the sentinels are always the ones the followed daemon
// uses, the plain STOP among them; a stream named by path is matched to its
// daemon by where it sits under logs/. When the file is absent it surfaces a
// clear "loop not running" / daemon-down state and keeps polling, so starting the
// daemon later just works without restarting the viewer.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/viewer"
)

// pollInterval is how often the viewer re-reads the stream for new lines (and, in
// dashboard mode, redraws). It is a plain poll (no inotify/fsevents — stdlib only);
// sub-second so the live view feels responsive without busy-spinning.
const pollInterval = 500 * time.Millisecond

func main() {
	argv, noAnimation, noBell := parseArgs(os.Args[1:])
	tgt, err := parseTarget(argv)
	if err != nil {
		fmt.Fprintf(os.Stderr, "watch: %v\n", err)
		os.Exit(2)
	}
	if tgt.all {
		// Several streams interleave, so --all is always the plain view, each line
		// labeled with its instance.
		runAll(runtimeDir())
		return
	}
	f := followFor(tgt, runtimeDir())
	if tgt.path != "" {
		f = followStream(tgt.path)
	}

	if useDashboard(isTTY(os.Stdout), os.Getenv) {
		runDashboard(f.stream, f.pid, f.stops, animateFromFlag(noAnimation), bellEnabled(noBell, os.Getenv))
		return
	}
	runPlain(f.stream, f.pid)
}

// target is which daemon the viewer follows: the default ticket loop (or a
// stream named by path), a named loop (#106), one named instance (#89), or all
// the instances.
type target struct {
	path     string
	loop     string
	instance string
	all      bool
}

// parseTarget reads --loop <name>, --instance <id> (each also as --flag=value)
// and --all from the arguments parseArgs left, the first other argument being
// the stream path.
func parseTarget(argv []string) (target, error) {
	var t target
	for i := 0; i < len(argv); i++ {
		switch a := argv[i]; {
		case a == "--loop":
			if i+1 >= len(argv) || argv[i+1] == "" {
				return t, errors.New("--loop needs a loop name, e.g. `--loop pr-doctor`")
			}
			i++
			t.loop = argv[i]
		case strings.HasPrefix(a, "--loop="):
			t.loop = strings.TrimPrefix(a, "--loop=")
		case a == "--instance":
			if i+1 >= len(argv) || argv[i+1] == "" {
				return t, errors.New("--instance needs an id, e.g. `--instance b`")
			}
			i++
			t.instance = argv[i]
		case strings.HasPrefix(a, "--instance="):
			t.instance = strings.TrimPrefix(a, "--instance=")
		case a == "--all":
			t.all = true
		case t.path == "":
			t.path = a
		}
	}
	if t.all && t.instance != "" {
		return t, errors.New("--all already follows every instance; drop --instance, or drop --all to follow one")
	}
	if t.all && t.loop != "" {
		return t, errors.New("--all follows the ticket loop's instances; drop --loop, or drop --all to follow that loop")
	}
	// The name and the id name files, so they are held to the daemon's rule.
	if t.loop != "" && !config.IsLoopName(t.loop) {
		return t, fmt.Errorf("loop %q must be lowercase letters, digits and dashes", t.loop)
	}
	if t.instance != "" && !config.IsLoopName(t.instance) {
		return t, fmt.Errorf("instance %q must be lowercase letters, digits and dashes", t.instance)
	}
	return t, nil
}

// defaultInstanceLabel names the default instance's lines under --all; no named
// instance may take it (config.Project.LoopInstance).
const defaultInstanceLabel = config.DefaultInstanceLabel

// follow is one daemon's files: the stream the viewer tails, the pidfile it
// probes for liveness, and every sentinel whose presence means that daemon is
// winding down — its own first, then the ones it shares with other daemons.
type follow struct {
	label  string
	stream string
	pid    string
	stops  []string
}

// runtimeDir is where the loop daemons keep their runtime files: the Consumer's
// .agent-harness/ under $PROJECT_PATH, else the cwd (the harness dir, where
// `make watch` runs and loop-start.sh falls back to).
func runtimeDir() string {
	if checkout := os.Getenv("PROJECT_PATH"); checkout != "" {
		return config.ProjectDir(checkout)
	}
	return "."
}

// followFor names the files of the daemon t picks under the runtime dir, by the
// names the daemon and loop-start.sh use (config.Loop's LogsDir, PidFile and
// sentinels): the default ticket loop's logs/loop.jsonl and loop.pid, a named
// loop's logs/loops/<name>/loop.jsonl and loop-<name>.pid, or an instance's
// logs/instances/<id>/loop.jsonl and loop-<id>.pid. Each is winding down on its
// instance's STOP-<id>, its loop's STOP-<name>, or the plain STOP. The viewer
// reads no config, so the plain STOP is the default STOP_FILE.
func followFor(t target, runtime string) follow {
	l := config.Loop{Name: t.loop, Instance: t.instance}
	if l.Name == "" {
		l.Name = config.DefaultLoopName
	}
	// The config names are relative to the checkout; runtime is its .agent-harness/.
	under := func(rel string) string {
		return filepath.Join(runtime, strings.TrimPrefix(filepath.ToSlash(rel), config.ProjectDirName+"/"))
	}
	f := follow{
		label:  defaultInstanceLabel,
		stream: loopstream.PathUnder(under(l.LogsDir())),
		pid:    under(l.PidFile()),
	}
	if own := l.InstanceStopFile(); own != "" {
		f.label = l.Instance
		f.stops = append(f.stops, under(own))
	} else if l.Name != config.DefaultLoopName {
		f.label = l.Name
	}
	f.stops = append(f.stops, under(l.StopFile()), filepath.Join(runtime, "STOP"))
	return f
}

// followStream follows a stream named by path with the files of the daemon that
// writes it (#106), read off where the stream sits: logs/loops/<name>/ is loop
// <name>'s, logs/instances/<id>/ is instance <id>'s, and any other stream is the
// default ticket loop's, its runtime dir the one above its logs/.
func followStream(path string) follow {
	dir := filepath.Dir(path)
	parent := filepath.Dir(dir)
	runtime := parent
	t := target{}
	if logs := filepath.Dir(parent); filepath.Base(logs) == "logs" {
		switch filepath.Base(parent) {
		case "loops":
			t.loop, runtime = filepath.Base(dir), filepath.Dir(logs)
		case "instances":
			t.instance, runtime = filepath.Base(dir), filepath.Dir(logs)
		}
	}
	f := followFor(t, runtime)
	f.stream = path
	return f
}

// allInstances is the default instance and every named instance with a logs dir
// under the runtime dir, in name order.
func allInstances(runtime string) []follow {
	out := []follow{followFor(target{}, runtime)}
	dirs, _ := filepath.Glob(filepath.Join(runtime, "logs", "instances", "*"))
	sort.Strings(dirs)
	for _, dir := range dirs {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			out = append(out, followFor(target{instance: filepath.Base(dir)}, runtime))
		}
	}
	return out
}

// runAll follows every instance at once in the plain view (#89): each event line
// labeled with its instance, a stopped notice when an instance's daemon goes
// away, and instances that start later picked up on the next poll.
func runAll(runtime string) {
	fmt.Printf("watching every loop instance under %s — Ctrl-C to quit (this does not stop any loop)\n", runtime)
	tailers := map[string]*viewer.Tailer{}
	alive := map[string]bool{}
	for {
		for _, f := range allInstances(runtime) {
			tl, seen := tailers[f.label]
			if !seen {
				tl = viewer.NewLabeledTailer(f.stream, f.label)
				tailers[f.label] = tl
				alive[f.label] = daemonAlive(f.pid)
			}
			if _, err := tl.Poll(os.Stdout); err != nil {
				fmt.Fprintf(os.Stderr, "watch: [%s] %v\n", f.label, err)
			}
			now := daemonAlive(f.pid)
			if notice, ok := plainStoppedNotice(alive[f.label], now); ok {
				fmt.Printf("[%s] %s\n", f.label, notice)
			}
			alive[f.label] = now
		}
		time.Sleep(pollInterval)
	}
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
func runDashboard(path, pidPath string, stopPaths []string, animate, bell bool) {
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
			StopRequested: anyStopRequested(stopPaths),
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

// stopRequested reports whether the STOP sentinel is present — a pure existence
// probe (os.Stat), never a write: the viewer reads the operator's stop request to
// display it, but the sentinel remains the operator's to create and the daemon's to
// clear. A present sentinel (even the empty file `touch` creates) is a stop request;
// any stat error (absent, unreadable) reads as not-requested rather than a crash.
func stopRequested(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// anyStopRequested reports whether any of the sentinels is present.
func anyStopRequested(paths []string) bool {
	for _, p := range paths {
		if stopRequested(p) {
			return true
		}
	}
	return false
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
