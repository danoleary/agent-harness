// Command implementation is the first of the three harness tools: fetch + claim
// one hand-passed ticket, run only the /tdd session in a Docker sandbox, verify
// the worktree + handoff commit by ground truth, and file any dropped findings.
// No push, no PR (review owns those), no loop. See docs/DESIGN.md "Build order".
//
// Its reusable plumbing lives in internal/ so review and retrospective share it:
// container launch + transcript tee (internal/session), ground-truth verification
// (internal/git + internal/verify), and findings filing (internal/filing). This
// entrypoint stays a thin wrapper that wires them together.
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
const sessionName = "implementation"

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
		return a, fmt.Errorf("usage: implementation <TICKET-ID> [--dry-run] [--verbose]  (got: %q)", a.identifier)
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
	// Logs are keyed by ticket id, not run id, so review/retrospective can find
	// this session's transcript later by globbing logs/BEH-NNN/ (DESIGN.md).
	log, err := runlog.New(filepath.Join(cfg.HerdPath, "agent-harness", "logs"), args.identifier)
	if err != nil {
		return 1, err
	}
	slug := strings.ToLower(args.identifier)

	dry := ""
	if args.dryRun {
		dry = " (dry-run)"
	}
	log.Event(fmt.Sprintf("run %s — implementation %s%s", runID, args.identifier, dry))

	client := linear.NewClient(linear.NewTransport(cfg.LinearAPIKey))

	t, err := client.FetchTicket(args.identifier)
	if err != nil {
		return 1, err
	}
	priority := t.Priority
	if priority == "" {
		priority = "No priority"
	}
	log.Event(fmt.Sprintf("fetched %s (%s) — %s", t.Identifier, priority, t.Title))

	p := prompt.BuildTdd(t, slug)
	findingsDir := log.FindingsDir(sessionName)
	if err := os.MkdirAll(findingsDir, 0o755); err != nil {
		return 1, err
	}
	// Clear any stale dropbox from a prior run of this ticket before the session
	// writes (the findings dir is reused across runs; a leftover would be re-filed).
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
		log.Event("dry-run — not claiming the ticket, not launching the container")
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- docker command ---\ndocker %s\n",
			p, strings.Join(dockerArgs, " "),
		)
		return 0, nil
	}

	// Fail fast if Docker can't run the container, so we never claim a ticket we
	// cannot actually work (the launch failure would otherwise leave it In
	// Progress with no worktree — BEH-316's exit-125 footgun).
	if err := sandbox.Preflight(cfg.Image, filepath.Join(cfg.HerdPath, "agent-harness"), sandbox.ProbeRunner, sandbox.BuildImage); err != nil {
		return 1, err
	}

	if err := client.MoveToInProgress(args.identifier); err != nil {
		return 1, err
	}
	log.Event(fmt.Sprintf("claimed %s → In Progress", args.identifier))

	transcriptFile := runlog.TranscriptName(sessionName, runID)
	log.Event(fmt.Sprintf("launching sandbox (cap %d min)", int(cfg.TddTimeout.Minutes())))
	exitCode := session.Run(dockerArgs, session.Options{
		ContainerName:  containerName,
		TranscriptFile: transcriptFile,
		Timeout:        cfg.TddTimeout,
		Verbose:        args.verbose,
		Log:            log,
	})
	log.Event(fmt.Sprintf(
		"session exited (code %d) — transcript at logs/%s/%s", exitCode, args.identifier, transcriptFile,
	))

	// Ground truth, never self-report.
	truth := gitpkg.GatherTddGroundTruth(cfg.HerdPath, slug)
	result := verify.Tdd(truth)
	if result.OK {
		plural := "s"
		if truth.CommitsAhead == 1 {
			plural = ""
		}
		log.Event(fmt.Sprintf(
			"tdd ✓ %s (%d commit%s ahead)", result.Reason, truth.CommitsAhead, plural,
		))
	} else {
		log.Event("tdd ✗ " + result.Reason)
	}

	// File any harness-improvement findings the session dropped (after every session, per ADR-0001).
	filing.File(findingsDir, t.TeamID, args.identifier, client, log)

	if result.OK {
		return 0, nil
	}
	return 1, nil
}
