// Package session runs one sandboxed claude session: it launches the docker
// container, tees the stream-json transcript to disk, narrates progress to the
// console, and enforces a wall-clock cap. It is the shared container-launch +
// transcript-tee plumbing the three cmd/ tools (implementation, review,
// retrospective) build on (DESIGN.md "Build order").
package session

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/stream"
)

// Logger is the narration + transcript sink a session writes through
// (*runlog.Logger satisfies it).
type Logger interface {
	Event(msg string)
	TeeLine(file, raw string)
}

// Options parameterises one Run.
type Options struct {
	// ContainerName is the docker --name, so the harness can `docker kill` it on timeout.
	ContainerName string
	// TranscriptFile is the filename (under the ticket's log dir) the stream-json
	// transcript is teed to.
	TranscriptFile string
	// Timeout is the hard wall-clock cap; on expiry the container is killed. It is
	// measured against the wall clock (not a monotonic timer), so time the host
	// spent asleep counts toward it.
	Timeout time.Duration
	// IdleTimeout is the heartbeat window: if no bytes arrive on the stream for this
	// long, the stream is treated as dead (e.g. an API connection silently severed
	// while the host slept) and the container is killed even though the hard cap may
	// have time left. Non-positive disables the heartbeat.
	IdleTimeout time.Duration
	// Verbose echoes the raw agent stream to the console instead of the concise narration.
	Verbose bool
	// Log is the narration + transcript sink.
	Log Logger
}

// watchdogPollInterval is how often the watchdog re-evaluates the wall-clock cap
// and idle window. After a host wake the ticker resumes and the next tick (within
// this interval) observes the full elapsed wall time and reaps a dead container.
const watchdogPollInterval = 15 * time.Second

// stderrTailLines is how many trailing non-empty stderr lines to keep so a launch
// failure can be explained on the console (docker prints the real cause then a
// generic "See '… --help'." trailer, so one line isn't enough).
const stderrTailLines = 10

// Outcome is the result of a finished session: the container's exit code, plus
// whether the stream ended on a terminal usage-policy refusal (BEH-389) or a
// spending-cap abort (BEH-494) — pre-work aborts the caller may choose to retry
// rather than treat as a real failure.
type Outcome struct {
	// ExitCode is the container's exit code (1 on any launch failure).
	ExitCode int
	// UsagePolicyRefusal is true iff the stream carried the terminal usage-policy
	// refusal result event. The diff survives on disk (ADR-0002 real-path mount),
	// so the caller can retry the session rather than discard the run.
	UsagePolicyRefusal bool
	// SpendingCapAbort is true iff the stream carried a spending-cap abort (BEH-494):
	// the terminal is_error cap result, or the `model:"<synthetic>"` "Spending cap
	// reached" turn the API can ship at session start (BEH-568). Either way the
	// session was killed by a billing/usage cap before doing any work — a distinct
	// retry-after-reset class, not a real failure.
	SpendingCapAbort bool
	// ReviewVerdictEmitted is true iff the stream carried the /review-worktree
	// verdict — the "## Review:" report header (BEH-525). The caller uses it to tell
	// a completed qualitative review from one cut short before the report (e.g. an
	// OOM mid-gate), so a green host-side gate re-run isn't mistaken for a full
	// review. Only meaningful for review sessions.
	ReviewVerdictEmitted bool
	// ReviewBlocked is true iff the /review-worktree verdict declared a blocked
	// disposition (BEH-580) — an unresolved Blocker/Important finding the autonomous
	// reviewer could not resolve. The caller fails the push closed on it so the
	// finding isn't shipped to a PR unaddressed. Only meaningful for review sessions.
	ReviewBlocked bool
	// DockerReason is docker's own error line on a launch failure (exit 125 —
	// sandbox.ExitCannotStart), extracted from the stderr tail. It lets the caller
	// tell a transient launch failure (overlay2/read-only-fs, BEH-542) from a
	// genuine one (daemon down, image missing) so only the former is retried. Empty
	// when the container started.
	DockerReason string
}

// Retryable reports whether this outcome is an environmental, transient failure
// worth a bare retry — the 137 OOM-kill (sandbox.ExitOOMKill, BEH-524) or a
// transient exit-125 launch failure (overlay2/read-only-fs of BEH-542, or the
// container dying mid-run with `unexpected EOF` of BEH-550). Both are the
// host momentarily wedging, not a code/config fault; the same `docker run`
// succeeds once it recovers. A genuine 125 (daemon down, image missing, bad flag)
// and any real non-zero code the process itself returned stay terminal.
func (o Outcome) Retryable() bool {
	if o.ExitCode == sandbox.ExitOOMKill {
		return true
	}
	return o.ExitCode == sandbox.ExitCannotStart && sandbox.IsRetryableStartFailure(o.DockerReason)
}

// streamFlags are the notable stream signals pumpStdout detects while scanning the
// stdout transcript, surfaced onto Outcome: the retryable pre-work aborts plus
// whether the review session reached its verdict.
type streamFlags struct {
	usagePolicyRefusal   bool
	spendingCapAbort     bool
	reviewVerdictEmitted bool
	reviewBlocked        bool
}

// realNow returns the current wall-clock time with the monotonic reading stripped
// (time.Now().Round(0)). The watchdog must compare wall-clock instants so a host
// sleep — during which the monotonic clock freezes — still counts toward the cap
// and the idle window.
func realNow() time.Time { return time.Now().Round(0) }

// heartbeat is a concurrency-safe holder for the wall-clock instant of the last
// observed stream activity, written by the stdout reader and read by the watchdog.
type heartbeat struct {
	mu   sync.Mutex
	last time.Time
}

func (h *heartbeat) beat(t time.Time) {
	h.mu.Lock()
	h.last = t
	h.mu.Unlock()
}

func (h *heartbeat) at() time.Time {
	h.mu.Lock()
	defer h.mu.Unlock()
	return h.last
}

// heartbeatReader wraps the docker stdout reader so every read that yields bytes
// records a heartbeat. A read returning no bytes (EOF / empty) does not — a dead
// stream only ever blocks or returns 0/EOF, so it produces no heartbeats and the
// idle watchdog can reap it.
type heartbeatReader struct {
	r    io.Reader
	beat func()
}

func (h heartbeatReader) Read(p []byte) (int, error) {
	n, err := h.r.Read(p)
	if n > 0 {
		h.beat()
	}
	return n, err
}

// watchReason reports the wall-clock limit the session has crossed, with a human
// reason for the kill log, or "" if neither limit is breached yet. Both limits are
// measured against wall-clock times the caller passes in (never a monotonic
// duration), so time the host spent asleep counts: Go's monotonic timers freeze
// during macOS sleep, so a frozen time.AfterFunc let a container that lost its API
// stream mid-sleep hang indefinitely (BEH-386-class).
//
//   - hardCap bounds total elapsed since start (the hard wall-clock cap). hardCap <= 0 disables it.
//   - idle    bounds time since the last stream activity (the heartbeat: a dead
//     stream stops producing output). idle <= 0 disables it.
//
// When both are breached, the limit whose deadline came *first* is reported, not
// the cap unconditionally. The watchdog only re-evaluates on its monotonic ticker,
// which freezes while the host sleeps, so a session that went idle early and was
// then slept past its cap is first observed well after both deadlines — blaming
// the cap there hides that the stream was dead long before (the BEH-538 mislabel:
// "wall-clock cap 30m hit" at 66 min for a session idle since minute 5). Reporting
// the earlier deadline attributes the kill to the stall that actually ended it.
func watchReason(now, start, lastActivity time.Time, hardCap, idle time.Duration) string {
	capCrossed := hardCap > 0 && now.Sub(start) >= hardCap
	idleCrossed := idle > 0 && now.Sub(lastActivity) >= idle

	capReason := fmt.Sprintf("wall-clock cap %s hit", hardCap)
	idleReason := fmt.Sprintf("no stream activity for %s (stream appears dead)", idle)

	switch {
	case capCrossed && idleCrossed:
		// Both past — name the one whose deadline elapsed earlier (the true cause).
		if lastActivity.Add(idle).Before(start.Add(hardCap)) {
			return idleReason
		}
		return capReason
	case capCrossed:
		return capReason
	case idleCrossed:
		return idleReason
	default:
		return ""
	}
}

// sleepNoteThreshold is the smallest wall-vs-monotonic gap worth reporting. Without
// a host sleep the two elapsed measurements track within scheduling jitter (well
// under a second), so a gap this large can only be the host having slept.
const sleepNoteThreshold = 1 * time.Minute

// sleepNote explains a late-landing wall-clock kill as host sleep, for the kill
// log. The watchdog measures the cap against the wall clock, but its poll ticker
// runs on the monotonic clock, which freezes while the host sleeps — so the
// container can only be reaped on the first tick after wake, long after the wall
// deadline. The gap between wall-elapsed and monotonic-elapsed at kill time is
// exactly the slept time, so surfacing it turns a 30 min cap observed at 66 min
// from an apparent timer bug into "host slept ~36m" (BEH-538). Returns "" when the
// gap is negligible (the host was awake the whole time).
func sleepNote(wallElapsed, monoElapsed time.Duration) string {
	slept := wallElapsed - monoElapsed
	if slept < sleepNoteThreshold {
		return ""
	}
	return fmt.Sprintf(" (host slept ~%s — watchdog paused, reaped on wake)", slept.Round(time.Minute))
}

// Run launches the sandboxed session described by dockerArgs (everything after
// `docker`), teeing the transcript and narrating progress. It returns the
// container's exit code (1 on any launch failure) plus whether a usage-policy
// refusal was seen. On a docker-cannot-start exit (125) it surfaces docker's own
// reason on the console — otherwise the operator sees a bare exit code (BEH-316).
func Run(dockerArgs []string, opts Options) Outcome {
	cmd := exec.Command("docker", dockerArgs...) // allow-unbounded-exec: main docker run, bounded by the timer-based kill below
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		opts.Log.Event("session ✗ failed to pipe docker stdout: " + err.Error())
		return Outcome{ExitCode: 1}
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		opts.Log.Event("session ✗ failed to pipe docker stderr: " + err.Error())
		return Outcome{ExitCode: 1}
	}

	if err := cmd.Start(); err != nil {
		opts.Log.Event("session ✗ failed to launch docker: " + err.Error())
		return Outcome{ExitCode: 1}
	}

	// Wall-clock watchdog: enforce the hard cap and the idle heartbeat against the
	// real clock (so host-sleep time counts) and kill the container out from under a
	// session that overruns or whose stream has gone dead. The heartbeat is tapped
	// off the stdout reader — every read that yields bytes is a sign of life.
	start := realNow()
	monoStart := time.Now() // keeps its monotonic reading; freezes during host sleep
	hb := &heartbeat{}
	hb.beat(start)
	tappedStdout := heartbeatReader{r: stdout, beat: func() { hb.beat(realNow()) }}

	watchdogDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(watchdogPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
				if reason := watchReason(realNow(), start, hb.at(), opts.Timeout, opts.IdleTimeout); reason != "" {
					note := sleepNote(realNow().Sub(start), time.Since(monoStart))
					opts.Log.Event("session ✗ " + reason + note + " — killing " + opts.ContainerName)
					_ = exec.Command("docker", "kill", opts.ContainerName).Run() // allow-unbounded-exec: docker kill from the watchdog itself
					return
				}
			}
		}
	}()
	defer close(watchdogDone)

	var (
		wg    sync.WaitGroup
		tail  []string
		flags streamFlags
	)
	wg.Add(2)
	go func() {
		defer wg.Done()
		flags = pumpStdout(tappedStdout, opts.TranscriptFile, opts.Verbose, opts.Log, os.Stdout)
	}()
	go func() {
		defer wg.Done()
		tail = pumpStderr(stderr, opts.TranscriptFile, opts.Log, stderrTailLines)
	}()
	wg.Wait()

	exitCode := 0
	if err := cmd.Wait(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			exitCode = exitErr.ExitCode()
		} else {
			exitCode = 1
		}
	}

	// Exit 125 means docker couldn't start the container at all (daemon down,
	// image missing, bad flag). The reason is teed only to the transcript, so
	// echo the real cause to the console (BEH-316).
	dockerReason := ""
	if exitCode == sandbox.ExitCannotStart {
		dockerReason = sandbox.DockerErrorReason(strings.Join(tail, "\n"))
		hint := dockerReason
		if hint == "" {
			hint = "see transcript for docker's error"
		}
		opts.Log.Event("session ✗ docker could not start the container (exit 125): " + hint)
	}

	return Outcome{ExitCode: exitCode, UsagePolicyRefusal: flags.usagePolicyRefusal, SpendingCapAbort: flags.spendingCapAbort, ReviewVerdictEmitted: flags.reviewVerdictEmitted, ReviewBlocked: flags.reviewBlocked, DockerReason: dockerReason}
}

// pumpStdout scans claude's stream-json stdout: it tees every line raw to the
// transcript, then either echoes the raw line (verbose) or surfaces the concise
// narration for narratable lines. A malformed line is teed but skipped for
// narration so one bad chunk can't kill the run. It returns the retryable
// pre-work aborts the stream carried — the terminal usage-policy refusal
// (BEH-389) and the spending-cap abort (BEH-494) — checked on every line
// regardless of --verbose so a retryable abort is never missed.
func pumpStdout(r io.Reader, transcriptFile string, verbose bool, log Logger, echo io.Writer) streamFlags {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var flags streamFlags
	for sc.Scan() {
		line := sc.Text()
		log.TeeLine(transcriptFile, line)
		if stream.IsUsagePolicyRefusal(line) {
			flags.usagePolicyRefusal = true
		}
		if stream.IsSpendingCapAbort(line) {
			flags.spendingCapAbort = true
		}
		if stream.IsReviewVerdict(line) {
			flags.reviewVerdictEmitted = true
		}
		if stream.IsReviewBlocked(line) {
			flags.reviewBlocked = true
		}
		if verbose {
			fmt.Fprintln(echo, line)
			continue
		}
		if msg, ok := stream.Narrate(line); ok {
			log.Event(msg)
		}
	}
	return flags
}

// pumpStderr tees every stderr line (forensic only) and returns a bounded tail of
// the last non-empty lines, so a launch failure can be explained on the console.
func pumpStderr(r io.Reader, transcriptFile string, log Logger, tailLines int) []string {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 4*1024*1024)
	var tail []string
	for sc.Scan() {
		line := sc.Text()
		log.TeeLine(transcriptFile, line)
		if t := strings.TrimSpace(line); t != "" {
			tail = append(tail, t)
			if len(tail) > tailLines {
				tail = tail[len(tail)-tailLines:]
			}
		}
	}
	return tail
}
