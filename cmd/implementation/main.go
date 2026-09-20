// Command implementation is the first of the three harness tools: fetch + claim
// one hand-passed ticket, run only the /tdd session in a Docker sandbox, verify
// the worktree + handoff commit by ground truth, and file any dropped findings.
// No push, no PR (review owns those), no loop. See docs/DESIGN.md "Build order".
//
// The body lives in internal/stages.Implementation so cmd/pipeline can call the
// same stage; this entrypoint stays a thin wrapper that parses args, wires up
// config + the runlog, and maps the stage Result to an exit code.
package main

import (
	"errors"
	"fmt"
	"os"

	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/version"
)

func main() {
	args, err := stages.ParseArgs("implementation", os.Args[1:], false)
	if errors.Is(err, stages.ErrHelp) {
		// Asked-for usage is a success: print it on stdout and exit 0, so `--help`
		// works before any credential is set and can be piped.
		fmt.Println(stages.Usage("implementation", false))
		os.Exit(0)
	}
	if errors.Is(err, stages.ErrVersion) {
		fmt.Println(version.Version)
		os.Exit(0)
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	cfg, log, runID, err := stages.Setup(args.Identifier)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	res := stages.Implementation(hostio.New(cfg, log, runID, args.Verbose), cfg, log, args)
	if res.Err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", res.Err)
		os.Exit(1)
	}
	if !res.OK {
		os.Exit(1)
	}
}
