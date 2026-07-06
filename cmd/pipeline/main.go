// Command pipeline runs the single-ticket chain — implementation → review →
// retrospective — over one ticket, then exits. It is the layer between the three
// standalone tools and the (future) autonomous loop: it adds no stop control or
// circuit breaker, only the in-order orchestration with the failure/skip semantics
// in DESIGN.md "The pipeline".
//
// The ticket is either hand-passed (`pipeline BEH-NNN`) or auto-selected
// (`pipeline --next`, DESIGN.md "Single-shot auto-select"): --next resolves the
// top-of-queue eligible ticket and claims it during selection (ADR-0003), threading
// PreClaimed into the implementation stage so the hand-passed path stays untouched.
//
// In-process, not subprocesses: it calls the same internal/stages bodies the
// cmd/<tool> wrappers do, sharing one config load and one runlog, so it gets real
// per-stage results (not opaque exit codes) and stays a single process for clean
// Ctrl-C / timeout handling. This entrypoint stays a thin wrapper.
package main

import (
	"fmt"
	"os"

	gitpkg "github.com/beherd/agent-harness/internal/git"
	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/pipeline"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/stages"
	"github.com/beherd/agent-harness/internal/trackers"
)

func main() {
	args, err := stages.ParseArgs("pipeline", os.Args[1:], true)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// --next auto-selects (and, unless --dry-run, claims) the ticket, then falls
	// through to the normal pipeline over it. Selection needs config + a Linear
	// client up front, and narrates to the console because the ticket-keyed runlog
	// can't exist until a ticket is chosen (an empty queue chooses none).
	if args.Next {
		cfg, err := stages.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		client, err := trackers.New(cfg.Tracker, trackers.Secrets{LinearKey: cfg.LinearAPIKey, GitHubToken: cfg.GitHubToken, JiraBaseURL: cfg.JiraBaseURL, JiraEmail: cfg.JiraEmail, JiraToken: cfg.JiraAPIToken})
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		// Selection narrates the ticket-selected event into the same global loop.jsonl
		// the per-ticket logger feeds, so a single-shot `pipeline --next` is viewable by
		// cmd/watch too (ADR-0005). A single-shot run does not truncate — only the daemon does.
		console := runlog.NewConsole(loopstream.NewStream(loopstream.PathUnder(stages.LogsRoot(cfg))))
		sel := pipeline.ResolveNext(client, args.DryRun, console)
		if !sel.Proceed {
			// Dry-run resolved a real ticket: print its plan before exiting.
			if args.DryRun && sel.Identifier != "" {
				fmt.Print(pipeline.Plan(cfg, sel.Identifier))
			}
			os.Exit(sel.ExitCode)
		}
		args.Identifier = sel.Identifier
		args.PreClaimed = sel.PreClaimed
	} else if args.DryRun {
		// Hand-passed --dry-run is pipeline-level: print the plan and exit without
		// claiming the ticket, launching a container, or touching Linear (DESIGN.md).
		// It needs only config, not a runlog — nothing is logged because nothing runs.
		cfg, err := stages.LoadConfig()
		if err != nil {
			fmt.Fprintf(os.Stderr, "%v\n", err)
			os.Exit(1)
		}
		fmt.Print(pipeline.Plan(cfg, args.Identifier))
		return
	}

	// One config load + one runlog + one run id, shared across all three stages.
	cfg, log, runID, err := stages.Setup(args.Identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// Bind each stage to the shared cfg/log/runID/args (--verbose and PreClaimed
	// forward through args). The pipeline decides ordering; the stages do the work.
	outcome := pipeline.Run(pipeline.Deps{
		FetchMain:      func() error { return gitpkg.FetchMain(cfg.HerdPath) },
		Implementation: func() stages.Result { return stages.Implementation(cfg, log, runID, args) },
		Review:         func() stages.Result { return stages.Review(cfg, log, runID, args) },
		Retrospective:  func() stages.Result { return stages.Retrospective(cfg, log, runID, args) },
		Log:            log,
	})
	os.Exit(outcome.ExitCode)
}
