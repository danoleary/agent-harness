// Command retrospective is the third and terminal harness tool: it runs the
// /retrospective skill over a ticket's prior session transcripts (implementation
// + review), then files whatever harness-improvement findings the session
// dropped to Linear. Its ground truth is the *presence* of `/findings/out.json`:
// an empty `[]` is success ("ran, found nothing"); an absent file means the step
// never ran and is a failure (worktree kept as a breadcrumb). On a fully clean
// ticket — the branch was pushed (review's host-side gate) and the retrospective
// filed — it tears the worktree down host-side. See docs/DESIGN.md "Build order"
// + "Harness-improvement findings".
//
// The body lives in internal/stages.Retrospective so cmd/pipeline can call the
// same stage; this entrypoint stays a thin wrapper that parses args, wires up
// config + the runlog, and maps the stage Result to an exit code.
package main

import (
	"fmt"
	"os"

	"github.com/beherd/agent-harness/internal/stages"
)

func main() {
	args, err := stages.ParseArgs("retrospective", os.Args[1:])
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	cfg, log, runID, err := stages.Setup(args.Identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	res := stages.Retrospective(cfg, log, runID, args)
	if res.Err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", res.Err)
		os.Exit(1)
	}
	if !res.OK {
		os.Exit(1)
	}
}
