// Command loop is the autonomous daemon: it drives the single-ticket pipeline
// (internal/pipeline) over the ready-for-agent queue in a long-running for{},
// selecting and claiming the next ticket, running implementation → review →
// retrospective over it, then repeating — idling and re-polling on an empty queue
// rather than exiting (DESIGN.md "The loop", ADR-0004). It takes no required
// arguments: the queue is the input.
//
// The loop sequencing lives in internal/loop (unit-tested without Docker, Linear,
// or real signals); this entrypoint stays a thin wrapper that wires the real
// host-side I/O the loop injects: the STOP-sentinel filesystem ops, the SIGINT
// signal handler, Linear selection (via pipeline.ResolveNext), and the per-ticket
// pipeline run (via stages.Setup + pipeline.Run).
//
// Stop control (DESIGN.md "Stop control"):
//   - SIGINT (Ctrl-C) flips a flag and logs "will stop after current ticket"; the
//     running session is left alone, and the loop winds down at the next
//     between-ticket checkpoint.
//   - the agent-harness/STOP sentinel does the same — `touch` it from anywhere to
//     wind an AFK run down gracefully. It is cleared at startup so a stale file
//     from a prior run can't stop a fresh daemon.
//   - a second SIGINT is a hard abort: it kills any running harness container and
//     exits now, leaving the worktree behind (harmless; review-worktree can pick
//     it up later).
package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/beherd/agent-harness/internal/config"
	gitpkg "github.com/beherd/agent-harness/internal/git"
	"github.com/beherd/agent-harness/internal/linear"
	"github.com/beherd/agent-harness/internal/loop"
	"github.com/beherd/agent-harness/internal/pipeline"
	"github.com/beherd/agent-harness/internal/proc"
	"github.com/beherd/agent-harness/internal/stages"
)

// Idle re-poll cadence: how long the daemon waits before re-polling an empty
// queue, and the granularity that wait is broken into so a stop landing mid-idle
// is honoured within a few seconds, not a whole poll interval later. Both are
// env-overridable for tests/ops without touching code.
const (
	defaultPollInterval = 30 * time.Second
	defaultTickInterval = 2 * time.Second
)

// defaultMaxConsecutiveFailures is the circuit-breaker threshold: after this many
// consecutive tickets fail to reach a pushed PR, the daemon trips and winds down
// (DESIGN.md §Circuit breaker). Hardcoded here; the LOOP_MAX_CONSECUTIVE_FAILURES
// env override arrives with the loop's config knobs (BEH-577).
const defaultMaxConsecutiveFailures = 3

// defaultCapBackoff is how long the daemon sleeps after an external Anthropic
// spending-cap abort before re-polling, long enough to let the cap window reset so
// the loop auto-resumes (DESIGN.md §Spending-cap abort backoff). Hardcoded here;
// the LOOP_CAP_BACKOFF_MS env override arrives with the loop's config knobs
// (BEH-577). It is broken into defaultTickInterval chunks so a STOP landing
// mid-backoff is honoured within seconds, not ~45 minutes later.
const defaultCapBackoff = 45 * time.Minute

// killDockerTimeout bounds the hard-abort docker calls so a wedged daemon can't
// hang the exit path (BEH-388) — the second Ctrl-C must always terminate promptly.
const killDockerTimeout = 10 * time.Second

// consoleNarrator narrates loop-level events (startup, stop, between-ticket
// transitions) before any ticket-keyed runlog exists — the loop runs across many
// tickets, so its own narration can't live under one ticket's dir. It matches the
// runlog's concise timestamped console format (DESIGN.md "Logging").
type consoleNarrator struct{}

func (consoleNarrator) Event(message string) {
	fmt.Printf("%s  %s\n", time.Now().UTC().Format(time.RFC3339), message)
}

func main() {
	log := consoleNarrator{}

	// Config is loaded once for the whole daemon: HERD_PATH locates the STOP
	// sentinel and the primary checkout to fast-forward; LINEAR_API_KEY backs
	// selection. A bad config must fail loud before the loop starts.
	cfg, err := stages.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	stopFile := filepath.Join(cfg.HerdPath, "agent-harness", "STOP")
	client := linear.NewClient(linear.NewTransport(cfg.LinearAPIKey))

	// sigStop is flipped by the first SIGINT; the loop folds it together with the
	// STOP sentinel into one StopRequested predicate. An atomic.Bool is the seam
	// between the async signal goroutine and the synchronous loop check.
	var sigStop atomic.Bool
	installSignalHandler(&sigStop, log)

	code := loop.Run(loop.Deps{
		ClearStopFile: func() error { return removeIfPresent(stopFile) },
		FetchMain:     func() error { return gitpkg.FetchMain(cfg.HerdPath) },
		StopRequested: func() bool { return sigStop.Load() || fileExists(stopFile) },
		ResolveNext: func() (string, bool) {
			// A real run, never a dry-run: the loop claims-on-select (ADR-0003) so a
			// concurrent selection can't grab the same ticket. ExitCode 1 (a Linear
			// selection/claim failure) is folded into "no ticket" — the loop idles and
			// re-polls rather than crashing the whole daemon on one bad poll.
			sel := pipeline.ResolveNext(client, false, log)
			if !sel.Proceed {
				return "", false
			}
			return sel.Identifier, true
		},
		RunPipeline:            func(id string) loop.TicketOutcome { return runPipeline(cfg, id) },
		ReleaseTicket:          func(id string) error { return client.ReleaseToTodo(id) },
		Sleep:                  time.Sleep,
		PollInterval:           defaultPollInterval,
		TickInterval:           defaultTickInterval,
		CapBackoff:             defaultCapBackoff,
		MaxConsecutiveFailures: defaultMaxConsecutiveFailures,
		Log:                    log,
	})
	os.Exit(code)
}

// runPipeline runs the full implementation → review → retrospective chain over one
// already-claimed ticket, mirroring cmd/pipeline's wiring: its own ticket-keyed
// runlog and run id (the loop runs many tickets, so each gets its own log dir). The
// stage args carry PreClaimed=true because the loop's ResolveNext claimed the
// ticket on selection, so the implementation stage skips its own claim and releases
// on a preflight failure (ADR-0003).
func runPipeline(cfg config.Config, identifier string) loop.TicketOutcome {
	_, log, runID, err := stages.Setup(identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		// A setup failure produced no PR; surface it as a plain no-PR outcome so the
		// breaker counts it like any other failure to ship.
		return loop.TicketOutcome{}
	}
	args := stages.Args{Identifier: identifier, PreClaimed: true}
	out := pipeline.Run(pipeline.Deps{
		FetchMain:      func() error { return gitpkg.FetchMain(cfg.HerdPath) },
		Implementation: func() stages.Result { return stages.Implementation(cfg, log, runID, args) },
		Review:         func() stages.Result { return stages.Review(cfg, log, runID, args) },
		Retrospective:  func() stages.Result { return stages.Retrospective(cfg, log, runID, args) },
		Log:            log,
	})
	// Translate the pipeline's typed Outcome into the breaker's signals — the loop
	// keys on "did it ship?", not the exit code.
	return loop.TicketOutcome{
		ReachedPushedPR:  out.ReachedPushedPR,
		SpendingCapAbort: out.SpendingCapAbort,
	}
}

// installSignalHandler wires the two-stage SIGINT contract (DESIGN.md "Stop
// control"): the first Ctrl-C flips the stop flag and narrates the graceful
// wind-down; a second is a hard abort that kills any running harness container and
// exits now. Subsequent signals after the first are handled by the same goroutine.
func installSignalHandler(sigStop *atomic.Bool, log loop.Narrator) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		sigStop.Store(true)
		log.Event("loop — stop requested (Ctrl-C); will stop after current ticket")
		<-ch
		log.Event("loop — second interrupt; hard abort, killing running container")
		killHarnessContainers()
		os.Exit(130) // 128 + SIGINT(2): conventional "terminated by Ctrl-C".
	}()
}

// killHarnessContainers best-effort kills any container whose name carries the
// harness prefix, so a hard abort tears down the in-flight session rather than
// orphaning it. The active session container is detached in its own process group
// (so the parent's SIGINT didn't reach it); naming the kill by prefix is how the
// host reaches across that boundary. Failures are ignored — the process is exiting.
func killHarnessContainers() {
	out, _, err := proc.Output(killDockerTimeout, "docker", "ps", "-q", "--filter", "name=herd-harness-")
	if err != nil || len(out) == 0 {
		return
	}
	for _, id := range splitLines(string(out)) {
		if id != "" {
			_ = proc.Run(killDockerTimeout, "docker", "kill", id)
		}
	}
}

// removeIfPresent deletes path, treating an already-absent file as success — the
// startup STOP-clear must not fail merely because there was nothing to clear.
func removeIfPresent(path string) error {
	err := os.Remove(path)
	if err != nil && os.IsNotExist(err) {
		return nil
	}
	return err
}

// fileExists reports whether path exists (a present STOP sentinel = stop
// requested). A stat error other than not-exist is treated as "present" so an
// unreadable sentinel errs toward stopping rather than ignoring an operator's
// touch.
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil || !os.IsNotExist(err)
}

// splitLines splits docker's newline-separated id output into trimmed lines.
func splitLines(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == '\n' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	if start < len(s) {
		out = append(out, s[start:])
	}
	return out
}
