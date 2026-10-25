package main

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/danoleary/agent-harness/internal/scaffold"
)

// initMain is `agent-harness init`, run in the checkout the operator is in.
func initMain(args []string) int {
	return runInit(args, checkoutRoot(), os.Stdin, os.Stdout, os.Stderr)
}

// initArgs is init's flag grammar, shared by its own usage and the dispatcher's
// help row so the two cannot drift apart (#119).
const initArgs = "[--yes] [--force] [--toolchain T] [--repo owner/name]"

// runInit scaffolds dir's .agent-harness/ (internal/scaffold, ADR-0021).
func runInit(args []string, dir string, in io.Reader, out, errOut io.Writer) int {
	fs := flag.NewFlagSet("init", flag.ContinueOnError)
	fs.SetOutput(errOut)
	yes := fs.Bool("yes", false, "accept every detected default without asking")
	fs.BoolVar(yes, "y", false, "shorthand for --yes")
	force := fs.Bool("force", false, "overwrite files that already exist (never the .env)")
	toolchain := fs.String("toolchain", "", "the project toolchain: go, pnpm, dotnet or python (default: detected)")
	repo := fs.String("repo", "", "the GitHub owner/name whose issues are the tickets (default: the origin remote)")
	fs.Usage = func() {
		fmt.Fprint(fs.Output(), "usage: agent-harness init "+initArgs+"\n\n"+
			"Scaffold this checkout's .agent-harness/ directory: config.toml, a Dockerfile FROM\n"+
			"the pinned sandbox base, the three prompt bodies, and a gitignored .env.\n\n")
		fs.PrintDefaults()
	}
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(out)
			fs.Usage()
			return 0
		}
		return 2
	}
	if fs.NArg() > 0 {
		fmt.Fprintf(errOut, "agent-harness init: unexpected argument %q\n", fs.Arg(0))
		return 2
	}

	fmt.Fprintf(out, "Scaffolding %s/.agent-harness/\n", dir)
	err := scaffold.Run(scaffold.Options{
		Dir: dir, Yes: *yes, Force: *force, Toolchain: *toolchain, Repo: *repo,
		OriginURL: originURL, In: in, Out: out,
	})
	if err != nil {
		fmt.Fprintf(errOut, "agent-harness init: %v\n", err)
		return 1
	}
	return 0
}

// checkoutRoot is the top of the git checkout the operator is in, so `init` run
// from a subdirectory still lands where the harness reads it; outside git it is
// the working directory.
func checkoutRoot() string {
	if top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output(); err == nil {
		return strings.TrimSpace(string(top))
	}
	wd, err := os.Getwd()
	if err != nil {
		return "."
	}
	return wd
}

func originURL(dir string) (string, error) {
	url, err := exec.Command("git", "-C", dir, "remote", "get-url", "origin").Output()
	return strings.TrimSpace(string(url)), err
}
