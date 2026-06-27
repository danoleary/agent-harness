// Package stages holds the body of each harness tool's run() as a callable
// function returning a typed Result, so both the thin cmd/<tool> wrappers and
// cmd/pipeline can invoke it. The three tools (implementation, review,
// retrospective) stay behaviourally identical to their pre-extraction form; the
// pipeline composes them, sharing one config load and one runlog (DESIGN.md
// "The pipeline").
package stages

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/runlog"
)

var ticketRE = regexp.MustCompile(`^[A-Z]+-\d+$`)

// isDiskFull reports whether err is the host-disk-full ENOSPC — surfaced when a
// findings-dir mkdir fails because the disk filled (BEH-540: the BEH-336
// retrospective hard-errored with `mkdir … findings/…: no space left on device`,
// forcing a manual re-run). The disk being full is not the stage's fault and the
// in-sandbox agent can't fix it, so the caller degrades to a clear, actionable
// warning instead of an opaque hard error. errors.Is sees through os.PathError's
// wrapping; other errno values (e.g. EACCES) are not disk-full.
func isDiskFull(err error) bool {
	return errors.Is(err, syscall.ENOSPC)
}

// Args is the parsed CLI surface shared by all three tools and the pipeline:
// a ticket identifier plus the universal flags. Force overrides the
// already-merged-on-main dispatch guard (BEH-528) and is only consulted by the
// implementation stage.
type Args struct {
	Identifier string
	DryRun     bool
	Verbose    bool
	Force      bool
}

// ParseArgs parses argv (excluding the program name) into Args. tool names the
// caller for the usage error only — the flag/identifier grammar is identical for
// every tool, which is why it lives here rather than being copied per cmd.
func ParseArgs(tool string, argv []string) (Args, error) {
	var a Args
	for _, arg := range argv {
		switch {
		case arg == "--dry-run":
			a.DryRun = true
		case arg == "--verbose":
			a.Verbose = true
		case arg == "--force":
			a.Force = true
		case !strings.HasPrefix(arg, "-") && a.Identifier == "":
			a.Identifier = strings.ToUpper(arg)
		}
	}
	if !ticketRE.MatchString(a.Identifier) {
		return a, fmt.Errorf("usage: %s <TICKET-ID> [--dry-run] [--verbose] [--force]  (got: %q)", tool, a.Identifier)
	}
	return a, nil
}

// Result is a stage's typed outcome. It is richer than a bare exit code so the
// pipeline can sequence on OK (and the loop, later, can read structured results
// instead of opaque codes). OK == true implies Err == nil.
type Result struct {
	// OK is the ground-truth verdict: did this stage do its job?
	OK bool
	// Err is a hard setup/IO error (config load, Linear fetch, Docker preflight)
	// that the standalone wrapper prints to stderr before exiting 1. For the
	// pipeline it is simply folded into "not OK".
	Err error
	// Retryable marks a failed outcome that crashed environmentally with nothing
	// to salvage — no worktree, no commit — as opposed to running to completion
	// and producing no handoff diff. Only the former is worth a fresh attempt (a
	// later run, or a healthy host, may get further); the pipeline re-attempts the
	// stage once when it is set (BEH-543). Never set when OK is true.
	Retryable bool
}

// LoadConfig loads the harness config: .env first (best-effort), then the
// environment. Shared so the pipeline and each standalone tool load it the same
// way.
func LoadConfig() (config.Config, error) {
	config.LoadDotEnv(".env")
	return config.Load(os.Getenv)
}

// Setup performs the once-per-run wiring every tool needs: load config, mint a
// run id, and open the ticket-keyed runlog. The pipeline calls it once and
// passes the results to all three stages (DESIGN.md: one config load, one
// runlog); each standalone tool calls it for itself.
func Setup(identifier string) (config.Config, *runlog.Logger, string, error) {
	cfg, err := LoadConfig()
	if err != nil {
		return config.Config{}, nil, "", err
	}
	runID := runlog.MakeRunID(time.Now())
	log, err := runlog.New(filepath.Join(cfg.HerdPath, "agent-harness", "logs"), identifier)
	if err != nil {
		return cfg, nil, "", err
	}
	return cfg, log, runID, nil
}
