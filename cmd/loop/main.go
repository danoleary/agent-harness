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
	"strconv"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/beherd/agent-harness/internal/config"
	gitpkg "github.com/beherd/agent-harness/internal/git"
	"github.com/beherd/agent-harness/internal/loop"
	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/pipeline"
	"github.com/beherd/agent-harness/internal/proc"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/stages"
	"github.com/beherd/agent-harness/internal/tracker"
	"github.com/beherd/agent-harness/internal/trackers"
)

// tickInterval is the granularity the idle/backoff waits are broken into so a stop
// landing mid-wait is honoured within a few seconds, not a whole poll/backoff
// interval later. It is a fixed responsiveness floor, not an operator knob — the
// idle cadence (LoopPollInterval) and cap backoff (LoopCapBackoff) are the
// env-overridable durations (DESIGN.md §cmd/loop config knobs).
const tickInterval = 2 * time.Second

// capBackoffHeartbeat is how often the post-cap-abort backoff narrates a "still
// capped, re-poll ~HH:MMZ" heartbeat (BEH-605). One per minute keeps even the 45m
// default backoff visibly alive without flooding the log — coarse enough to be
// quiet, fine enough that a watcher never mistakes a long backoff for a dead daemon.
// Like tickInterval, it is a fixed responsiveness floor, not an operator knob.
const capBackoffHeartbeat = time.Minute

// killDockerTimeout bounds the hard-abort docker calls so a wedged daemon can't
// hang the exit path (BEH-388) — the second Ctrl-C must always terminate promptly.
const killDockerTimeout = 10 * time.Second

// pruneTimeout / storePruneTimeout bound the disk-reclaim shell-outs (ADR-0005) so a
// stalled `gh`/network or a wedged pnpm can't hang the between-ticket reclaim. The
// prune script makes one `gh pr view` per worktree, so it gets the more generous cap.
const (
	pruneTimeout      = 5 * time.Minute
	storePruneTimeout = 2 * time.Minute
)

func main() {
	// Config is loaded once for the whole daemon: HERD_PATH locates the STOP
	// sentinel and the primary checkout to fast-forward; LINEAR_API_KEY backs
	// selection. A bad config must fail loud before the loop starts.
	cfg, err := stages.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// The global loop.jsonl is the daemon→viewer contract (ADR-0005). Truncate it
	// at clean startup so it is bounded to this one daemon run (like loop.log), then
	// narrate loop-level events through a Console that mirrors structured events into
	// it. The loop runs across many tickets, so its own narration can't live under
	// one ticket's dir — but it feeds the SAME global stream the per-ticket loggers do.
	stream := loopstream.NewStream(loopstream.PathUnder(stages.LogsRoot(cfg)))
	if err := stream.Truncate(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not truncate loop.jsonl at startup: %v\n", err)
	}
	log := runlog.NewConsole(stream)

	// STOP_FILE locates the sentinel used by startup-clear and the stop check. A
	// relative override is resolved against HERD_PATH (the default "agent-harness/STOP"
	// gives the same path as before); an absolute override is used as-is.
	stopFile := cfg.StopFile
	if !filepath.IsAbs(stopFile) {
		stopFile = filepath.Join(cfg.HerdPath, stopFile)
	}
	client, err := trackers.New(cfg.Tracker.Kind, cfg.LinearAPIKey)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

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
		RunPipeline:   func(id string) loop.TicketOutcome { return runPipeline(cfg, id) },
		ReleaseTicket: func(id string) error { return client.ReleaseToTodo(id) },
		CommentTicket: func(id, body string) error { return client.AddComment(id, body) },
		// Stale-claim reaper (BEH-677): list the agent-claimed In Progress set from the
		// tracker, map it onto the loop's StaleClaim shape, and check for a pushed branch
		// host-side via git. All three run on the host, never inside the sandbox.
		ListInProgressClaims:   func() ([]loop.StaleClaim, error) { return listStaleClaims(client) },
		TicketHasRemoteBranch:  func(id string) bool { return gitpkg.TicketHasRemoteBranch(cfg.HerdPath, id) },
		ClaimTTL:               cfg.LoopClaimTTL,
		Sleep:                  time.Sleep,
		Now:                    time.Now,
		PollInterval:           cfg.LoopPollInterval,
		TickInterval:           tickInterval,
		CapBackoff:             cfg.LoopCapBackoff,
		CapBackoffHeartbeat:    capBackoffHeartbeat,
		MaxConsecutiveFailures: cfg.LoopMaxConsecutiveFailures,
		MaxTickets:             cfg.LoopMaxTickets,
		MaxRuntime:             cfg.LoopMaxRuntime,
		// Disk reclaim (ADR-0005). The worktrees live under HERD_PATH/.claude/worktrees,
		// so statfs HERD_PATH (always present, same volume) for the cheap gate; the prune
		// shells out to the existing squash-merge-aware script, and `pnpm store prune` is
		// the cheap secondary. All three run host-side, never inside the sandbox.
		DiskReclaimThreshold: cfg.LoopDiskReclaimThreshold,
		FreeDisk:             func() (uint64, error) { return sandbox.FreeDiskBytes(cfg.HerdPath) },
		PruneMergedWorktrees: func() (int, error) { return pruneMergedWorktrees(cfg.HerdPath) },
		StorePrune:           func() error { return storePrune(cfg.HerdPath) },
		Log:                  log,
	})
	os.Exit(code)
}

// listStaleClaims fetches the agent-claimed In Progress set from the tracker and maps
// it onto the loop's StaleClaim shape (BEH-677). The mapping is a straight projection —
// the loop stays decoupled from the tracker port, taking primitive claims the same way
// it takes primitive thunks for every other dependency.
func listStaleClaims(client tracker.Tracker) ([]loop.StaleClaim, error) {
	claims, err := client.ListInProgressClaims()
	if err != nil {
		return nil, err
	}
	out := make([]loop.StaleClaim, 0, len(claims))
	for _, c := range claims {
		out = append(out, loop.StaleClaim{
			Identifier:  c.Identifier,
			StartedAt:   c.StartedAt,
			HasLinkedPR: c.HasLinkedPR,
		})
	}
	return out, nil
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
		RecommendClose:   out.RecommendClose,
	}
}

// pruneMergedWorktrees shells out to the repo-root prune-merged-worktrees.sh --yes,
// the existing, tested, squash-merge-aware reclaimer (ADR-0005). It is run with the
// herd checkout as its cwd so the script's `git rev-parse --show-toplevel` resolves
// to that checkout, and bounded by pruneTimeout so a stalled `gh`/network can't hang
// the between-ticket reclaim. The merged/clean classification stays entirely in the
// script (single source of truth); here we only parse how many it removed for the
// loop's narration. A non-zero exit (e.g. gh unreachable) surfaces as an error the
// loop logs and swallows — reclaim is never a ticket outcome.
func pruneMergedWorktrees(herdPath string) (int, error) {
	script := filepath.Join(herdPath, "scripts", "prune-merged-worktrees.sh")
	out, err := proc.CombinedOutputInDir(pruneTimeout, herdPath, script, "--yes")
	if err != nil {
		return 0, fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return parsePrunedCount(string(out)), nil
}

// storePrune runs `pnpm store prune` — the cheap, non-destructive, network-free
// secondary reclaim (ADR-0005) — bounded so a wedged pnpm can't hang the loop. The
// global store is shared regardless of cwd; herdPath is used only to anchor the call.
func storePrune(herdPath string) error {
	out, err := proc.CombinedOutputInDir(storePruneTimeout, herdPath, "pnpm", "store", "prune")
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}

// parsePrunedCount reads the removed-worktree count from the prune script's
// authoritative summary line ("Pruned N worktree(s)."), degrading to 0 when the
// script removed nothing or printed no summary. Parsing the script's own count keeps
// a single source of truth for "what got removed".
func parsePrunedCount(output string) int {
	const prefix = "Pruned "
	for _, line := range splitLines(output) {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		fields := strings.Fields(strings.TrimPrefix(line, prefix))
		if len(fields) > 0 {
			if n, err := strconv.Atoi(fields[0]); err == nil {
				return n
			}
		}
	}
	return 0
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
