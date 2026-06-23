// Command review is the second of the three harness tools: over the worktree the
// implementation slice left behind, run a *cold* /review-worktree session in the
// sandbox (fixes committed locally only), then — host-side, holding GH_TOKEN —
// independently re-run the quality gates in a throwaway container and, only if
// they pass, push the branch and open the PR. Ground truth is the harness's own
// gate run, never the agent's self-report (DESIGN.md "Build order", ADR-0002).
//
// The body lives in internal/stages.Review so cmd/pipeline can call the same
// stage; this entrypoint stays a thin wrapper that parses args, wires up config +
// the runlog, and maps the stage Result to an exit code.
package main

import (
	"fmt"
	"os"

	"github.com/beherd/agent-harness/internal/stages"
)

func main() {
	args, err := stages.ParseArgs("review", os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	cfg, log, runID, err := stages.Setup(args.Identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	res := stages.Review(cfg, log, runID, args)
	if res.Err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", res.Err)
		os.Exit(1)
	}
	if !res.OK {
		os.Exit(1)
	}
}
