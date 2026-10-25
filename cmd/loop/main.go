// Command loop is the autonomous daemon: it drives the single-ticket pipeline
// (internal/pipeline) over the ready-for-agent queue in a long-running for{},
// selecting and claiming the next ticket, running implementation → review →
// retrospective over it, then repeating — idling and re-polling on an empty queue
// rather than exiting (DESIGN.md "The loop", ADR-0004). It takes no required
// arguments: the queue is the input.
//
// The sequencing lives in internal/loop (unit-tested without Docker, a tracker or
// real signals) and the host-side I/O it drives in internal/loophost (the
// production loop.Host, and the ticket loop's loop.Source and loop.Pipeline —
// ADR-0019). This entrypoint is the composition root: it loads config, opens the
// narration stream, builds the host, the ticket Source and the ticket Pipeline,
// wires the signal handler, and hands the daemon its knobs.
//
// Stop control (DESIGN.md "Stop control"):
//   - SIGINT (Ctrl-C) flips a flag and logs "will stop after current ticket"; the
//     running session is left alone, and the loop winds down at the next
//     between-ticket checkpoint.
//   - the .agent-harness/STOP sentinel does the same — `touch` it from anywhere to
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
	"syscall"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/loop"
	"github.com/danoleary/agent-harness/internal/loophost"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/prloop"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/trackers"
	"github.com/danoleary/agent-harness/internal/version"
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

// usage is the daemon's own grammar. The loop takes no ticket — the queue is the
// input — so it shares nothing with stages.Usage beyond the shape.
const usage = `usage: loop [--prs [--dry-run]] [--help] [--version]

Works the tracker's ready queue unattended, one ticket at a time: implementation,
review and retrospective per ticket, then a PR.

  --prs       work the open PRs instead: fix the ones that conflict with main or
              have red CI (temporary; becomes ` + "`agent-harness loop <name>`" + `)
  --dry-run   with --prs: list the PRs it would work and the plan for each, and
              change nothing

Configuration is read from the environment, filled in from the first of:
  $HARNESS_ENV_FILE
  ./.env
  $XDG_CONFIG_HOME/agent-harness/.env   (else ~/.config/agent-harness/.env)
Never keep that file inside the project checkout: every sandbox bind-mounts it.

Stop it gracefully with ` + "`touch $PROJECT_PATH/.agent-harness/STOP`" + ` (` + "`STOP-prs`" + ` for --prs)
or one SIGINT. See CONSUMER.md for the .agent-harness/ directory a project must commit.`

// flags is the daemon's parsed command line.
type flags struct {
	// prs runs the PR loop instead of the ticket loop (ADR-0019).
	prs bool
	// dryRun prints the PR loop's plan and exits without changing anything.
	dryRun bool
}

// parseFlags reads the daemon's two run flags; anything else is ignored, as it
// always was (--help and --version are answered before this, without a
// credential). --dry-run only means something for the PR loop, and is refused
// without it: ignoring it would start a real ticket loop when a dry run was asked
// for. The ticket loop's dry-run is `pipeline --next --dry-run`.
func parseFlags(argv []string) (flags, error) {
	var f flags
	for _, arg := range argv {
		switch arg {
		case "--prs":
			f.prs = true
		case "--dry-run":
			f.dryRun = true
		}
	}
	if f.dryRun && !f.prs {
		return f, fmt.Errorf("loop: --dry-run needs --prs (for one ticket, use `agent-harness pipeline --next --dry-run`)")
	}
	return f, nil
}

func main() {
	// Usage before config: an operator who has just unpacked a release archive must
	// be able to ask what this takes without holding a credential yet. Handled here
	// because the daemon otherwise parses no arguments at all, so `loop --help` fell
	// through to "missing Claude credential" — which teaches nothing.
	for _, arg := range os.Args[1:] {
		if arg == "--help" || arg == "-h" {
			fmt.Println(usage)
			os.Exit(0)
		}
	}
	for _, arg := range os.Args[1:] {
		if arg == "--version" {
			fmt.Println(version.Version)
			os.Exit(0)
		}
	}

	f, err := parseFlags(os.Args[1:])
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}

	// Config is loaded once for the whole daemon: PROJECT_PATH locates the STOP
	// sentinel and the primary checkout to fast-forward, and the tracker secrets back
	// selection. A bad config must fail loud before the loop starts.
	cfg, err := stages.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	if f.prs {
		os.Exit(runPRLoop(cfg, f.dryRun))
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

	client, err := trackers.New(cfg.Tracker, trackers.Secrets{LinearKey: cfg.LinearAPIKey, GitHubToken: cfg.GitHubToken, JiraBaseURL: cfg.JiraBaseURL, JiraEmail: cfg.JiraEmail, JiraToken: cfg.JiraAPIToken})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	source := loophost.NewTicketSource(cfg, log, client)
	host := loophost.NewDaemon(cfg, source.CheckAccess)
	installSignalHandler(host, log)

	os.Exit(loop.Run(loop.Deps{
		Source:   source,
		Pipeline: loophost.NewTicketPipeline(log),
		Host:     host,
		Limits:   limitsFrom(cfg),
		Clock:    loop.SystemClock{},
		Log:      log,
	}))
}

// runPRLoop is the composition root of the PR loop (ADR-0019): the github_prs
// Source and the PR-fixer Pipeline, run by the same daemon as the ticket loop but
// with their own STOP sentinel and their own loop.jsonl, so the two can run side
// by side. The tracker is still read: a branch whose ticket the ticket loop holds
// is skipped. --dry-run prints the Source's plan and changes nothing.
func runPRLoop(cfg config.Config, dryRun bool) int {
	client, err := trackers.New(cfg.Tracker, trackers.Secrets{LinearKey: cfg.LinearAPIKey, GitHubToken: cfg.GitHubToken, JiraBaseURL: cfg.JiraBaseURL, JiraEmail: cfg.JiraEmail, JiraToken: cfg.JiraAPIToken})
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	stream := loopstream.NewStream(loopstream.PathUnder(prloop.LogsRoot(cfg)))
	log := runlog.NewConsole(stream)
	tickets := loophost.NewTicketSource(cfg, log, client)
	pulls := github.NewPulls(github.NewGhTransport(cfg.ProjectPath), "{owner}/{repo}")
	source := prloop.NewSource(pulls, prloop.OptionsFrom(cfg), tickets.HeldBranches, log)

	if dryRun {
		plan, err := source.Plan()
		if err != nil {
			fmt.Fprintf(os.Stderr, "loop --prs --dry-run: %v\n", err)
			return 1
		}
		fmt.Print(prloop.RenderPlan(plan))
		return 0
	}

	// logs/prs/ is new to an existing checkout, and the stream does not create its
	// own directory.
	if err := os.MkdirAll(prloop.LogsRoot(cfg), 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create %s: %v\n", prloop.LogsRoot(cfg), err)
	}
	if err := stream.Truncate(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not truncate the PR loop's loop.jsonl at startup: %v\n", err)
	}
	daemonCfg := cfg
	daemonCfg.StopFile = cfg.PRLoopStopFile
	host := loophost.NewDaemon(daemonCfg, source.CheckAccess)
	installSignalHandler(host, log)
	return loop.Run(loop.Deps{
		Source:   source,
		Pipeline: prloop.Pipeline{},
		Host:     host,
		Limits:   limitsFrom(cfg),
		Clock:    loop.SystemClock{},
		Log:      log,
	})
}

// limitsFrom maps the Consumer's config onto the daemon's knobs. Every optional
// between-ticket chore is switched on or off by a number here — a zero
// LoopDiskReclaimThreshold disables reclaim, a non-positive LoopClaimTTL disables
// the stale-claim reaper — so "does this daemon reap?" is answered by the
// environment an operator set, not by which funcs this file happened to wire.
func limitsFrom(cfg config.Config) loop.Limits {
	return loop.Limits{
		PollInterval:           cfg.LoopPollInterval,
		TickInterval:           tickInterval,
		CapBackoff:             cfg.LoopCapBackoff,
		CapBackoffHeartbeat:    capBackoffHeartbeat,
		ClaimTTL:               cfg.LoopClaimTTL,
		DiskReclaimThreshold:   cfg.LoopDiskReclaimThreshold,
		MaxConsecutiveFailures: cfg.LoopMaxConsecutiveFailures,
		MaxTickets:             cfg.LoopMaxTickets,
		MaxRuntime:             cfg.LoopMaxRuntime,
	}
}

// installSignalHandler wires the two-stage SIGINT contract (DESIGN.md "Stop
// control"): the first Ctrl-C raises the host's stop signal and narrates the
// graceful wind-down; a second is a hard abort that kills any running harness
// container and exits now. Subsequent signals after the first are handled by the
// same goroutine.
func installSignalHandler(host *loophost.Daemon, log loop.Narrator) {
	ch := make(chan os.Signal, 2)
	signal.Notify(ch, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-ch
		host.RequestStop()
		log.Event("loop — stop requested (Ctrl-C); will stop after current ticket")
		<-ch
		log.Event("loop — second interrupt; hard abort, killing running container")
		host.KillContainers()
		os.Exit(130) // 128 + SIGINT(2): conventional "terminated by Ctrl-C".
	}()
}
