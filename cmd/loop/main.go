// Command loop is the autonomous daemon: it runs one loop (ADR-0019) in a
// long-running for{} — polling the loop's Source for the next work item, running
// the loop's Steps over it, then repeating, idling and re-polling when there is no
// work rather than exiting (DESIGN.md "The loop", ADR-0004). With no name it runs
// the ticket loop over the ready-for-agent queue; `loop pr-doctor` runs the PR
// loop; a Consumer declares more as [[loops]] in config.toml.
//
// The sequencing lives in internal/loop (unit-tested without Docker, a tracker or
// real signals) and the host-side I/O it drives in internal/loophost and
// internal/prloop (the production loop.Host, and each loop's loop.Source and
// loop.Pipeline). This entrypoint is the composition root: it loads config, picks
// the named loop, opens its narration stream, builds the host, the Source and the
// Pipeline for the loop's Steps, wires the signal handler, and hands the daemon
// the loop's limits.
//
// Stop control (DESIGN.md "Stop control"):
//   - SIGINT (Ctrl-C) flips a flag and logs "will stop after current ticket"; the
//     running session is left alone, and the loop winds down at the next
//     between-item checkpoint.
//   - the loop's .agent-harness/STOP-<name> sentinel does the same — `touch` it
//     from anywhere to wind an AFK run down gracefully — and the plain
//     .agent-harness/STOP winds down every loop. Both are cleared at startup so a
//     stale file from a prior run can't stop a fresh daemon.
//   - a second SIGINT is a hard abort: it kills any running harness container and
//     exits now, leaving the worktree behind (harmless; review-worktree can pick
//     it up later).
package main

import (
	"fmt"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
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

// usage is the daemon's own grammar. The loop takes no ticket — its Source is the
// input — so it shares nothing with stages.Usage beyond the shape.
const usage = `usage: loop [<name>] [--instance <id>] [--dry-run] [--list] [--help] [--version]

Runs one loop unattended until stopped: a Source polled for work, and the Steps
run over each item it yields (ADR-0019). With no name it runs the ticket loop
("tickets"): implementation, review and retrospective per ready ticket, then a PR.
"pr-doctor" fixes the open PRs that conflict with main or have red CI. A project
declares its own loops, or re-parameterises these two, as [[loops]] in
.agent-harness/config.toml.

  <name>      the loop to run (default: tickets)
  --instance  run as instance <id>, one of several ticket-loop daemons against
              this project: its own STOP-<id>, pidfile and logs; claims, gate
              slots and the plain STOP are shared with every other instance
  --list      print every loop this project can run, and exit
  --dry-run   print the named loop's resolved Source, Steps and limits, and for a
              PR loop the PRs it would work and how; change nothing

Configuration is read from the environment, filled in from the first of:
  $HARNESS_ENV_FILE
  ./.env
  $XDG_CONFIG_HOME/agent-harness/.env   (else ~/.config/agent-harness/.env)
Never keep that file inside the project checkout: every sandbox bind-mounts it.

Stop a loop gracefully with ` + "`touch $PROJECT_PATH/.agent-harness/STOP-<name>`" + `,
one instance with ` + "`touch $PROJECT_PATH/.agent-harness/STOP-<id>`" + `, every loop
with ` + "`touch $PROJECT_PATH/.agent-harness/STOP`" + `, or with one SIGINT.
See CONSUMER.md for the .agent-harness/ directory a project must commit.`

// flags is the daemon's parsed command line.
type flags struct {
	// name is the loop to run; "" is the default ticket loop.
	name string
	// list prints the loops and exits.
	list bool
	// dryRun prints the loop's plan and exits without changing anything.
	dryRun bool
	// instance names one of several daemons running the loop (#89); "" is the
	// default instance.
	instance string
}

// parseFlags reads the loop name and the two run flags. Other flags are ignored,
// as they always were (--help and --version are answered before this, without a
// credential). --prs, the PR loop's flag before loops had names, is refused with
// its replacement rather than ignored: ignoring it would start the ticket loop.
func parseFlags(argv []string) (flags, error) {
	var f flags
	for i := 0; i < len(argv); i++ {
		arg := argv[i]
		switch {
		case arg == "--instance":
			if i+1 >= len(argv) || strings.HasPrefix(argv[i+1], "-") {
				return f, fmt.Errorf("loop: --instance needs an id, e.g. `--instance b`")
			}
			i++
			f.instance = argv[i]
		case strings.HasPrefix(arg, "--instance="):
			f.instance = strings.TrimPrefix(arg, "--instance=")
			if f.instance == "" {
				return f, fmt.Errorf("loop: --instance needs an id, e.g. `--instance b`")
			}
		case arg == "--prs":
			return f, fmt.Errorf("loop: --prs is gone; the PR loop is a named loop now: run `agent-harness loop %s`", config.PRDoctorLoopName)
		case arg == "--list":
			f.list = true
		case arg == "--dry-run":
			f.dryRun = true
		case strings.HasPrefix(arg, "-"):
		case f.name != "":
			return f, fmt.Errorf("loop: one loop at a time — got %q and %q (run each in its own daemon)", f.name, arg)
		default:
			f.name = arg
		}
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
	// sentinel and the primary checkout to fast-forward, the tracker secrets back
	// selection, and the project config declares the loops. A bad config must fail
	// loud before the loop starts.
	cfg, err := stages.LoadConfig()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	if f.list {
		fmt.Print(renderList(cfg.AllLoops()))
		os.Exit(0)
	}
	if _, ok := cfg.LoopNamed(f.name); !ok {
		fmt.Fprintf(os.Stderr, "loop: no loop named %q in this project.\n%sDeclare it as [[loops]] in %s/config.toml.\n", f.name, renderList(cfg.AllLoops()), config.ProjectDirName)
		os.Exit(2)
	}
	l, err := cfg.LoopInstance(f.name, f.instance)
	if err != nil {
		fmt.Fprintf(os.Stderr, "loop: %v\n", err)
		os.Exit(2)
	}
	if f.dryRun {
		fmt.Print(describeLoop(cfg, l, limitsFrom(cfg, l)))
	}

	switch l.Source.Kind {
	case config.SourceGitHubPRs:
		os.Exit(runPRLoop(cfg, l, f.dryRun))
	default:
		os.Exit(runTicketLoop(cfg, l, f.dryRun))
	}
}

// openStream opens the loop's own loop.jsonl — the daemon→viewer contract
// (ADR-0005) — truncated at clean startup so it is bounded to this one daemon run
// (like loop.log), and narrates loop-level events through a Console that mirrors
// structured events into it. The loop runs across many items, so its own
// narration can't live under one item's dir — but it feeds the SAME stream the
// per-item loggers do. A loop's logs dir may be new to an existing checkout, and
// the stream does not create its own directory.
func openStream(cfg config.Config, l config.Loop) *runlog.Console {
	root := filepath.Join(cfg.ProjectPath, l.LogsDir())
	if err := os.MkdirAll(root, 0o755); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not create %s: %v\n", root, err)
	}
	stream := loopstream.NewStream(loopstream.PathUnder(root))
	if err := stream.Truncate(); err != nil {
		fmt.Fprintf(os.Stderr, "warning: could not truncate %s at startup: %v\n", stream.Path(), err)
	}
	return runlog.NewConsole(stream)
}

// runTicketLoop is the composition root of a ticket loop: the tracker Source and
// the ticket Pipeline over the loop's Steps. Its dry-run is the resolved loop
// alone: claiming a ticket to plan it would change the tracker, and `agent-harness
// pipeline --next --dry-run` plans the next ticket without claiming it.
func runTicketLoop(cfg config.Config, l config.Loop, dryRun bool) int {
	if dryRun {
		fmt.Println("\nTo see the plan for the next ticket, run `agent-harness pipeline --next --dry-run`.")
		return 0
	}
	log := openStream(cfg, l)
	client, err := trackers.FromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	source := loophost.NewTicketSource(cfg, log, client)
	host := loophost.NewDaemon(cfg, l, source.CheckAccess)
	installSignalHandler(host, log)
	return loop.Run(loop.Deps{
		Source:   source,
		Pipeline: loophost.NewTicketPipeline(log, l),
		Host:     host,
		Limits:   limitsFrom(cfg, l),
		Clock:    loop.SystemClock{},
		Log:      log,
	})
}

// runPRLoop is the composition root of a PR loop (ADR-0019): the github_prs
// Source and the PR-fixer Pipeline over the loop's Steps, run by the same daemon
// as the ticket loop but with their own STOP sentinel and their own loop.jsonl, so
// the two can run side by side. The tracker is still read: a branch whose ticket
// the ticket loop holds is skipped. --dry-run prints the Source's plan and changes
// nothing.
func runPRLoop(cfg config.Config, l config.Loop, dryRun bool) int {
	client, err := trackers.FromConfig(cfg)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		return 1
	}
	pulls := github.NewPulls(github.NewGhTransport(cfg.ProjectPath), "{owner}/{repo}")
	if dryRun {
		quiet := runlog.NewConsole(loopstream.NewStream(os.DevNull))
		tickets := loophost.NewTicketSource(cfg, quiet, client)
		plan, err := prloop.NewSource(pulls, prloop.OptionsFrom(cfg, l), tickets.HeldBranches, quiet).Plan()
		if err != nil {
			fmt.Fprintf(os.Stderr, "loop %s --dry-run: %v\n", l.Name, err)
			return 1
		}
		fmt.Print("\n" + prloop.RenderPlan(plan))
		return 0
	}

	log := openStream(cfg, l)
	tickets := loophost.NewTicketSource(cfg, log, client)
	source := prloop.NewSource(pulls, prloop.OptionsFrom(cfg, l), tickets.HeldBranches, log)
	host := loophost.NewDaemon(cfg, l, source.CheckAccess)
	installSignalHandler(host, log)
	return loop.Run(loop.Deps{
		Source:   source,
		Pipeline: prloop.Pipeline{Loop: l},
		Host:     host,
		Limits:   limitsFrom(cfg, l),
		Clock:    loop.SystemClock{},
		Log:      log,
	})
}

// limitsFrom maps the Consumer's config onto the daemon's knobs: the LOOP_* env
// knobs, each overridden by the loop's own limit when it sets one. Every optional
// between-item chore is switched on or off by a number here — a zero
// LoopDiskReclaimThreshold disables reclaim, a non-positive ClaimTTL disables the
// stale-claim reaper — so "does this daemon reap?" is answered by the config an
// operator set, not by which funcs this file happened to wire.
func limitsFrom(cfg config.Config, l config.Loop) loop.Limits {
	duration := func(own, env time.Duration) time.Duration {
		if own > 0 {
			return own
		}
		return env
	}
	count := func(own, env int) int {
		if own > 0 {
			return own
		}
		return env
	}
	return loop.Limits{
		PollInterval:           duration(l.Limits.PollInterval, cfg.LoopPollInterval),
		TickInterval:           tickInterval,
		CapBackoff:             duration(l.Limits.CapBackoff, cfg.LoopCapBackoff),
		CapBackoffHeartbeat:    capBackoffHeartbeat,
		ClaimTTL:               duration(l.Limits.ClaimTTL, cfg.LoopClaimTTL),
		DiskReclaimThreshold:   cfg.LoopDiskReclaimThreshold,
		MaxConsecutiveFailures: count(l.Limits.MaxConsecutiveFailures, cfg.LoopMaxConsecutiveFailures),
		MaxTickets:             count(l.Limits.MaxItems, cfg.LoopMaxTickets),
		MaxRuntime:             duration(l.Limits.MaxRuntime, cfg.LoopMaxRuntime),
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
