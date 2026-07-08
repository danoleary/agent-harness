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
	"github.com/beherd/agent-harness/internal/pr"
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
	client, err := trackers.New(cfg.Tracker, trackers.Secrets{LinearKey: cfg.LinearAPIKey, GitHubToken: cfg.GitHubToken, JiraBaseURL: cfg.JiraBaseURL, JiraEmail: cfg.JiraEmail, JiraToken: cfg.JiraAPIToken})
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
		RunPipeline: func(id string) loop.TicketOutcome { return runPipeline(cfg, id) },
		// RecoverCommittedFix finishes a no-PR run whose fix is already committed on the
		// branch but was never pushed/PR'd — the recovered-checkpoint / verify-only state
		// that stranded BEH-649 (BEH-713). Host-side: rebase + push + open PR via git/gh,
		// never inside the sandbox.
		RecoverCommittedFix: func(id string) (loop.TicketOutcome, bool) {
			return recoverCommittedFix(cfg, client, log, id)
		},
		ReleaseTicket: func(id string) error { return client.ReleaseToTodo(id) },
		// CloseTicket consumes a recommend-close verdict by moving the superseded ticket
		// to the terminal Canceled state (BEH-682), so it exits the selection + reaper
		// pools for good rather than re-looping to the same "nothing to ship" conclusion.
		CloseTicket:   func(id string) error { return client.MoveToCanceled(id) },
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
		CapResetTime:     out.SpendingCapResetTime,
		RecommendClose:   out.RecommendClose,
	}
}

// recoverGhTimeout bounds the `gh pr view`/`gh pr create` calls the committed-fix
// recovery makes, so a stalled network or a blocking gh auth prompt can't hang the
// between-ticket recovery (same hang class the review stage bounds with prCreateTimeout).
const recoverGhTimeout = 2 * time.Minute

// recoverCommittedFix finishes a no-PR run whose fix is already committed on the branch
// but was never pushed/PR'd — the recovered-checkpoint / verify-only state that stranded
// BEH-649, bouncing it Todo↔In-Progress ~9× (BEH-713). It returns attempted=true only
// when there IS such a state to complete — a clean worktree carrying a committed,
// non-empty diff against origin/main with no open PR — so the loop keeps releasing a
// genuine OOM/crash/empty-diff no-PR run (attempted=false) exactly as before. On a
// recoverable branch it rebases onto the latest main (so the PR opens on a current base),
// pushes force-with-lease (the remote may hold an older checkpoint tip — the BEH-649
// case, where a plain push is rejected non-fast-forward), and opens the PR, so a real fix
// the review stage never shipped reaches a PR for CI + human review instead of looping
// forever. Opening a PR is safe recovery, not a merge: CI and human review still gate the
// merge. Any git/gh failure returns ReachedPushedPR=false (attempted=true), so the loop
// falls through to the normal release and a later run re-grabs and retries — a completion
// attempt is never worse than no attempt.
func recoverCommittedFix(cfg config.Config, client tracker.Tracker, log loop.Narrator, identifier string) (loop.TicketOutcome, bool) {
	slug := strings.ToLower(identifier)
	worktreePath := gitpkg.WorktreePath(cfg.HerdPath, slug)

	// A committed fix means a clean worktree: uncommitted edits are an in-progress or
	// crashed run, not a finished-but-unpushed one, so leave those to the normal path.
	if _, err := os.Stat(worktreePath); err != nil {
		return loop.TicketOutcome{}, false
	}
	if !gitpkg.WorktreeClean(worktreePath) {
		return loop.TicketOutcome{}, false
	}
	// Refresh main so the empty-diff check and the PR base are current.
	if err := gitpkg.FetchMain(cfg.HerdPath); err != nil {
		log.Event("loop … warning: could not fetch origin/main during committed-fix recovery: " + err.Error())
	}
	// Nothing committed ahead of main (already merged, or an empty branch) → not a
	// recoverable committed fix; let the normal no-PR release handle it.
	if gitpkg.BranchDiffEmpty(worktreePath) {
		return loop.TicketOutcome{}, false
	}
	// A branch that already has an OPEN PR isn't stranded (its outcome would already
	// carry ReachedPushedPR — belt-and-suspenders), so there is nothing to complete.
	if openPRExists(cfg.HerdPath, gitpkg.BranchName(cfg.BranchPrefix, slug)) {
		return loop.TicketOutcome{}, false
	}

	log.Event("loop — " + identifier + " has a committed, unpushed fix with no PR; completing it host-side (rebase + push + PR) (BEH-713)")

	// Rebase onto the latest main so the PR opens on a current base. A genuine content
	// conflict can't be auto-completed — attempted, but not shipped, so the loop releases
	// it for a later run / human to resolve (never a worse state than before).
	if gitpkg.RebaseOntoMain(worktreePath) == gitpkg.RebaseConflict {
		gitpkg.AbortRebase(worktreePath)
		log.Event("loop … " + identifier + " committed-fix recovery: rebase onto main conflicted — cannot auto-complete")
		return loop.TicketOutcome{ReachedPushedPR: false}, true
	}
	// The rebase can collapse the branch to zero net change (a sibling PR landed the
	// same fix) — nothing to open a PR for.
	if gitpkg.BranchDiffEmpty(worktreePath) {
		log.Event("loop … " + identifier + " committed-fix recovery: branch became empty after rebase — nothing to ship")
		return loop.TicketOutcome{ReachedPushedPR: false}, true
	}
	// force-with-lease: the remote may hold an older checkpoint tip (the BEH-649 case),
	// so a plain push would be rejected as non-fast-forward.
	if err := gitpkg.PushForceWithLease(cfg.HerdPath, cfg.BranchPrefix, slug); err != nil {
		log.Event("loop … " + identifier + " committed-fix recovery: push failed: " + err.Error())
		return loop.TicketOutcome{ReachedPushedPR: false}, true
	}
	t, err := client.FetchTicket(identifier)
	if err != nil {
		log.Event("loop … " + identifier + " committed-fix recovery: could not fetch ticket for the PR body: " + err.Error())
		return loop.TicketOutcome{ReachedPushedPR: false}, true
	}
	subjects := gitpkg.CommitSubjects(cfg.HerdPath, cfg.BranchPrefix, slug)
	if err := openPR(cfg.HerdPath, cfg.BranchPrefix, slug, pr.BuildTitle(t), pr.BuildBody(t, subjects)); err != nil {
		log.Event("loop … " + identifier + " committed-fix recovery: gh pr create failed: " + err.Error())
		return loop.TicketOutcome{ReachedPushedPR: false}, true
	}
	return loop.TicketOutcome{ReachedPushedPR: true}, true
}

// openPRExists reports whether an OPEN PR already exists for the branch, via
// `gh pr view <branch> --json state`. Any error (no PR for the branch, gh failure) is
// treated as "no open PR": the recovery then proceeds to open one, and a duplicate
// `gh pr create` would fail harmlessly (surfaced as a completion failure → release).
func openPRExists(herdPath, branch string) bool {
	out, err := proc.CombinedOutputInDir(recoverGhTimeout, herdPath, "gh", "pr", "view", branch, "--json", "state", "-q", ".state")
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(out)) == "OPEN"
}

// openPR opens the pull request for the recovered branch, mirroring the review stage's
// host-side `gh pr create` (ADR-0002: the harness owns push + PR). GH_TOKEN stays
// host-only — gh reads it from the harness env — and gh infers the origin repo from the
// herd checkout.
func openPR(herdPath, branchPrefix, slug, title, body string) error {
	out, err := proc.CombinedOutputInDir(
		recoverGhTimeout, herdPath,
		"gh", "pr", "create",
		"--head", gitpkg.BranchName(branchPrefix, slug),
		"--base", "main",
		"--title", title,
		"--body", body,
	)
	if err != nil {
		return fmt.Errorf("%w: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
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
