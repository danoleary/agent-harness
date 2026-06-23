// Command pipeline runs the single-ticket chain — implementation → review →
// retrospective — over one hand-passed ticket, then exits. It is the layer
// between the three standalone tools and the (future) autonomous loop: it adds no
// ticket selection, stop control, or circuit breaker, only the in-order
// orchestration with the failure/skip semantics in DESIGN.md "The pipeline".
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
	"github.com/beherd/agent-harness/internal/pipeline"
	"github.com/beherd/agent-harness/internal/stages"
)

func main() {
	args, err := stages.ParseArgs("pipeline", os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}

	// --dry-run is pipeline-level: print the plan and exit without claiming the
	// ticket, launching a container, or touching Linear (DESIGN.md). It needs only
	// config, not a runlog — nothing is logged because nothing runs.
	if args.DryRun {
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

	// Bind each stage to the shared cfg/log/runID/args (--verbose forwards through
	// args). The pipeline decides ordering; the stages do the work.
	code := pipeline.Run(pipeline.Deps{
		FetchMain:      func() error { return gitpkg.FetchMain(cfg.HerdPath) },
		Implementation: func() stages.Result { return stages.Implementation(cfg, log, runID, args) },
		Review:         func() stages.Result { return stages.Review(cfg, log, runID, args) },
		Retrospective:  func() stages.Result { return stages.Retrospective(cfg, log, runID, args) },
		Log:            log,
	})
	os.Exit(code)
}
