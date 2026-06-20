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
// Like cmd/implementation it stays a thin wrapper over internal/: container
// launch + transcript tee (internal/session), the dropbox ground truth + filing
// (internal/filing), and worktree teardown (internal/git).
package main

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/filing"
	gitpkg "github.com/beherd/agent-harness/internal/git"
	"github.com/beherd/agent-harness/internal/linear"
	"github.com/beherd/agent-harness/internal/prompt"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/session"
	"github.com/beherd/agent-harness/internal/verify"
)

var ticketRE = regexp.MustCompile(`^[A-Z]+-\d+$`)

// sessionName prefixes this tool's transcript + findings dir under the ticket's
// log dir (DESIGN.md "Logging": logs/BEH-NNN/<session>-<run-id>.jsonl).
const sessionName = "retrospective"

type cliArgs struct {
	identifier string
	dryRun     bool
	verbose    bool
}

func parseArgs(argv []string) (cliArgs, error) {
	var a cliArgs
	for _, arg := range argv {
		switch {
		case arg == "--dry-run":
			a.dryRun = true
		case arg == "--verbose":
			a.verbose = true
		case !strings.HasPrefix(arg, "-") && a.identifier == "":
			a.identifier = strings.ToUpper(arg)
		}
	}
	if !ticketRE.MatchString(a.identifier) {
		return a, fmt.Errorf("usage: retrospective <TICKET-ID> [--dry-run] [--verbose]  (got: %q)", a.identifier)
	}
	return a, nil
}

func main() {
	code, err := run()
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	os.Exit(code)
}

func run() (int, error) {
	args, err := parseArgs(os.Args[1:])
	if err != nil {
		return 1, err
	}

	config.LoadDotEnv(".env")
	cfg, err := config.Load(os.Getenv)
	if err != nil {
		return 1, err
	}

	runID := runlog.MakeRunID(time.Now())
	// Logs are keyed by ticket id, not run id: this lets retrospective find the
	// implementation + review transcripts it studies by globbing logs/BEH-NNN/,
	// and lands its own transcript in the same dir (DESIGN.md "Logging").
	log, err := runlog.New(filepath.Join(cfg.HerdPath, "agent-harness", "logs"), args.identifier)
	if err != nil {
		return 1, err
	}
	slug := strings.ToLower(args.identifier)

	dry := ""
	if args.dryRun {
		dry = " (dry-run)"
	}
	log.Event(fmt.Sprintf("run %s — retrospective %s%s", runID, args.identifier, dry))

	client := linear.NewClient(linear.NewTransport(cfg.LinearAPIKey))

	// The ticket is fetched for its team id (findings are filed back into it) and
	// for narration. Retrospective runs *last* and never claims the ticket — the
	// implementation tool already moved it to In Progress.
	t, err := client.FetchTicket(args.identifier)
	if err != nil {
		return 1, err
	}
	log.Event(fmt.Sprintf("fetched %s — %s", t.Identifier, t.Title))

	p := prompt.BuildRetrospective(t, slug)
	findingsDir := log.FindingsDir(sessionName)
	if err := os.MkdirAll(findingsDir, 0o755); err != nil {
		return 1, err
	}
	// Clear any stale dropbox from a prior retrospective run of this ticket before
	// the session writes. The dir is ticket+session keyed (reused across runs), so
	// without this a previous run's out.json would both re-file as duplicates and
	// make the "out.json present" ground truth pass even if this run never wrote.
	if err := filing.ClearDropbox(findingsDir); err != nil {
		return 1, err
	}

	// runID is second-resolution; include the pid so two runs started in the same
	// second still get distinct container names (and distinct `docker kill` targets).
	containerName := fmt.Sprintf("herd-harness-%s-%d-%s", runID, os.Getpid(), sessionName)
	dockerArgs := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     findingsDir,
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          p,
		Model:           cfg.Model,
		ContainerName:   containerName,
	})

	if args.dryRun {
		log.Event("dry-run — not launching the container")
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- docker command ---\ndocker %s\n",
			p, strings.Join(dockerArgs, " "),
		)
		return 0, nil
	}

	// Fail fast if Docker can't run the container before launching the session.
	if err := sandbox.Preflight(cfg.Image, filepath.Join(cfg.HerdPath, "agent-harness"), sandbox.ProbeRunner, sandbox.BuildImage); err != nil {
		return 1, err
	}

	transcriptFile := runlog.TranscriptName(sessionName, runID)
	log.Event(fmt.Sprintf("launching sandbox (cap %d min)", int(cfg.TddTimeout.Minutes())))
	outcome := session.Run(dockerArgs, session.Options{
		ContainerName:  containerName,
		TranscriptFile: transcriptFile,
		Timeout:        cfg.TddTimeout,
		Verbose:        args.verbose,
		Log:            log,
	})
	log.Event(fmt.Sprintf(
		"session exited (code %d) — transcript at logs/%s/%s", outcome.ExitCode, args.identifier, transcriptFile,
	))

	// Ground truth, never self-report: the retrospective ran iff it wrote the
	// findings dropbox. An empty `[]` is still present → success; an absent file
	// means the step never ran (DESIGN.md "Success is ground-truth").
	result := verify.Retrospective(filing.DropboxExists(findingsDir))
	if result.OK {
		log.Event("retrospective ✓ " + result.Reason)
	} else {
		log.Event("retrospective ✗ " + result.Reason)
	}

	// File whatever the session dropped: one Linear issue per finding, `[]` files
	// nothing. Safe to call even on failure — an absent dropbox files nothing.
	filing.File(findingsDir, t.TeamID, args.identifier, client, client, log)

	if !result.OK {
		// Keep the worktree as a recoverable breadcrumb (DESIGN.md failure matrix).
		return 1, nil
	}

	// Clean ticket: retrospective filed AND the branch reached origin (review's
	// host-side push gate). Only then is the worktree pure disk cost — the PR
	// captures everything — so tear it down host-side (the real-path mount makes
	// its .git pointer resolve from the main checkout). If the branch was never
	// pushed, keep the worktree so unpushed work is never lost.
	if gitpkg.BranchPushed(cfg.HerdPath, slug) {
		if err := gitpkg.RemoveWorktree(cfg.HerdPath, slug); err != nil {
			log.Event("worktree kept — removal failed: " + err.Error())
		} else {
			log.Event("worktree removed — ticket clean (branch pushed + retrospective filed)")
		}
	} else {
		log.Event("worktree kept — branch not pushed to origin yet")
	}

	return 0, nil
}
