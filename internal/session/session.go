// Package session runs one sandboxed claude session: it launches the docker
// container, tees the stream-json transcript to disk, narrates progress to the
// console, and enforces a session cap on active time (monotonic, so host sleep is
// excluded — BEH-608). It is the shared container-launch +
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
	"syscall"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/stream"
)

// Logger is the narration + transcript sink a session writes through
// (*runlog.Logger satisfies it). Structured mirrors tool-use / session-result
// events into the global loop.jsonl the viewer tails (ADR-0005).
type Logger interface {
	Event(msg string)
	Structured(loopstream.Record)
	TeeLine(file, raw string)
}

// Options parameterises one Run.
type Options struct {
	// ContainerName is the docker --name, so the harness can `docker kill` it on timeout.
	ContainerName string
	// TranscriptFile is the filename (under the ticket's log dir) the stream-json
	// transcript is teed to.
	TranscriptFile string
	// Timeout is the hard cap on active (monotonic) session time; on expiry the
	// container is killed. It is measured against the monotonic clock, which freezes
	// during host sleep, so time the host spent asleep does NOT count toward it
	// (BEH-608) — a session that merely slept past its cap is not killed for it.
	Timeout time.Duration
	// IdleTimeout is the heartbeat window: if no bytes arrive on the stream for this
	// much active (monotonic) time, the stream is treated as dead (e.g. an API
	// connection silently severed) and the container is killed even though the hard
	// cap may have time left. Host sleep does not count toward the window, so a dead
	// stream is reaped within this window of wake, not mid-sleep. Non-positive
	// disables the heartbeat.
	IdleTimeout time.Duration
	// Verbose echoes the raw agent stream to the console instead of the concise narration.
	Verbose bool
	// Log is the narration + transcript sink.
	Log Logger
}

// watchdogPollInterval is how often the watchdog re-evaluates the cap and idle
// window. The ticker is monotonic and freezes during host sleep; after a host wake
// it resumes, and the next tick (within this interval) re-evaluates the monotonic
// elapsed — reaping a still-dead stream within the idle window of wake. This
// polling is the no-indefinite-hang guarantee (a one-shot timer would not fire
// again after sleep — BEH-386).
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
// (time.Now().Round(0)). The watchdog enforces its limits on the monotonic clock
// (so host sleep is excluded — see watchReason), and uses realNow only to measure
// total wall-elapsed for the sleep diagnostic: wall minus monotonic elapsed is the
// time the host spent asleep.
func realNow() time.Time { return time.Now().Round(0) }

// heartbeat is a concurrency-safe holder for the monotonic-bearing instant of the
// last observed stream activity (so the watchdog's idle window, measured via
// time.Since, excludes host sleep), written by the stdout reader and read by the
// watchdog.
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

// watchReason reports the limit the session has crossed, with a human reason for
// the kill log, or "" if neither limit is breached yet. Both limits are measured
// against MONOTONIC elapsed durations the caller passes in, which freeze while the
// host sleeps — so time the host spent asleep is excluded from both (BEH-608):
//
//   - activeElapsed is monotonic time since start; bounded by hardCap. hardCap <= 0 disables it.
//   - idleElapsed   is monotonic time since the last stream activity (the
//     heartbeat: a dead stream stops producing output); bounded by idle. idle <= 0 disables it.
//
// Excluding sleep means a session that was streaming right up until the host slept
// is NOT killed for the sleep: on wake the watchdog's poll ticker resumes and the
// monotonic clock picks up where it froze. The no-indefinite-hang guarantee
// BEH-386 secured still holds — it comes from the *polling* ticker re-evaluating
// after wake (the original bug was a one-shot time.AfterFunc that never fired
// again), not from charging slept time. A genuinely dead stream is still reaped,
// within `idle` of wake rather than mid-sleep. This reverses the earlier
// wall-clock cap (BEH-386/538): charging sleep reaped active, near-complete
// sessions whose host merely slept, burning the whole cap re-deriving the same
// work across re-launches.
//
// When both are breached, the limit whose deadline came *first* (on the active-
// time axis) is reported, not the cap unconditionally — naming the cap for a
// session that went idle long before it hides that the stream was the true cause
// (the BEH-538 truthfulness property). lastActivityAt is the active-axis instant
// of the last beat (start = 0): activeElapsed - idleElapsed.
func watchReason(activeElapsed, idleElapsed, hardCap, idle time.Duration) string {
	capCrossed := hardCap > 0 && activeElapsed >= hardCap
	idleCrossed := idle > 0 && idleElapsed >= idle

	capReason := fmt.Sprintf("session cap %s hit", hardCap)
	idleReason := fmt.Sprintf("no stream activity for %s (stream appears dead)", idle)

	lastActivityAt := activeElapsed - idleElapsed

	switch {
	case capCrossed && idleCrossed:
		// Both past — name the one whose deadline elapsed earlier (the true cause).
		if lastActivityAt+idle < hardCap {
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

// sleepNote names any host sleep that occurred during a session that was then
// killed, as context for the kill log. The cap and idle window are measured on the
// monotonic clock, so slept time is excluded and never the cause of a kill
// (BEH-608) — but the gap between wall-elapsed and monotonic (active) elapsed at
// kill time is exactly the time slept, so surfacing it confirms the sleep was
// discounted: a 30 min active cap reached after 66 min of wall time reads as
// "host also slept ~36m — not counted", not a broken timer. Returns "" when the
// gap is negligible (the host was awake the whole time).
func sleepNote(wallElapsed, monoElapsed time.Duration) string {
	slept := wallElapsed - monoElapsed
	if slept < sleepNoteThreshold {
		return ""
	}
	return fmt.Sprintf(" (host also slept ~%s — not counted toward the cap)", slept.Round(time.Minute))
}

// Run launches the sandboxed session described by dockerArgs (everything after
// `docker`), teeing the transcript and narrating progress. It returns the
// container's exit code (1 on any launch failure) plus whether a usage-policy
// refusal was seen. On a docker-cannot-start exit (125) it surfaces docker's own
// reason on the console — otherwise the operator sees a bare exit code (BEH-316).
func Run(dockerArgs []string, opts Options) Outcome {
	cmd := exec.Command("docker", dockerArgs...) // allow-unbounded-exec: main docker run, bounded by the timer-based kill below
	// Launch docker in its own process group so a Ctrl-C delivered to the harness's
	// foreground process group (e.g. the long-running cmd/loop daemon) is NOT
	// forwarded to this child and on to the container. The loop's signal handler
	// instead flips a stop flag and lets the in-flight session finish; the running
	// container is only killed on the explicit double-Ctrl-C hard abort. Without
	// Setpgid the terminal would SIGINT the whole group, severing the session
	// mid-ticket and defeating the graceful between-ticket stop.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
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

	// Monotonic watchdog: enforce the hard cap and the idle heartbeat against the
	// monotonic clock (which freezes during host sleep, so slept time is excluded —
	// BEH-608) and kill the container out from under a session that overruns its
	// active time or whose stream has gone dead while awake. The heartbeat is tapped
	// off the stdout reader — every read that yields bytes is a sign of life — and
	// carries a monotonic reading so the idle window also excludes sleep. The wall
	// clock (start) is kept only to quantify any concurrent sleep for the kill log.
	start := realNow()      // wall clock; sleep diagnostic only
	monoStart := time.Now() // keeps its monotonic reading; freezes during host sleep
	hb := &heartbeat{}
	hb.beat(time.Now()) // monotonic-bearing, so time.Since(hb.at()) excludes sleep
	tappedStdout := heartbeatReader{r: stdout, beat: func() { hb.beat(time.Now()) }}

	watchdogDone := make(chan struct{})
	go func() {
		ticker := time.NewTicker(watchdogPollInterval)
		defer ticker.Stop()
		for {
			select {
			case <-watchdogDone:
				return
			case <-ticker.C:
				activeElapsed := time.Since(monoStart) // monotonic: excludes host sleep
				idleElapsed := time.Since(hb.at())     // monotonic: excludes host sleep
				if reason := watchReason(activeElapsed, idleElapsed, opts.Timeout, opts.IdleTimeout); reason != "" {
					note := sleepNote(realNow().Sub(start), activeElapsed)
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
		if msg, kind, ok := stream.NarrateRecord(line); ok {
			// Mirror to the global stream with the structured kind (tool-use vs
			// session-result) so the viewer counts activity / sees the session end
			// without re-parsing the prose (ADR-0005). The ticket/stage are left to the
			// viewer's carried-forward state — the session doesn't know them, and the
			// preceding stage-start record already set them.
			log.Structured(loopstream.Record{Kind: kind, Message: msg})
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
