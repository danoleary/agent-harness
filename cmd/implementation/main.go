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
	// A closure so a retry (BEH-389) can re-launch under a distinct --name and a
	// resume prompt: the name must match Options.ContainerName for the timeout
	// `docker kill` to hit the right container, and the retry prompt differs from
	// the first attempt's (it resumes the existing worktree rather than creating one).
	buildArgs := func(name, prmpt string) []string {
		return sandbox.BuildDockerRunArgs(sandbox.Config{
			Image:           cfg.Image,
			HerdPath:        cfg.HerdPath,
			FindingsDir:     findingsDir,
			PnpmStoreVolume: cfg.PnpmStoreVolume,
			Prompt:          prmpt,
			Model:           cfg.Model,
			ContainerName:   name,
		})
	}
	dockerArgs := buildArgs(containerName, p)

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

	// The tdd session runs at most twice. A terminal usage-policy refusal is a
	// known intermittent false-positive that disproportionately strikes long
	// agentic sessions (BEH-389); because the diff survives on disk (ADR-0002
	// real-path mount), a refusal that left no handoff commit is retried once on
	// the same ticket rather than discarded. Any other outcome — success, a real
	// failure, a non-refusal error — is final on the first attempt.
	const maxTddAttempts = 2

	worktreePath := gitpkg.WorktreePath(cfg.HerdPath, slug)
	var (
		truth  verify.GroundTruth
		result verify.Result
	)
	for attempt := 1; attempt <= maxTddAttempts; attempt++ {
		attemptContainer := containerName
		attemptArgs := dockerArgs
		attemptTranscript := runlog.TranscriptName(sessionName, runID)
		if attempt > 1 {
			attemptContainer = fmt.Sprintf("%s-retry%d", containerName, attempt)
			// The retry resumes the existing worktree (it already holds the surviving
			// diff) rather than recreating it (BEH-389).
			attemptArgs = buildArgs(attemptContainer, prompt.BuildTddResume(t, slug, worktreePath))
			attemptTranscript = runlog.TranscriptName(fmt.Sprintf("%s-retry%d", sessionName, attempt), runID)
			log.Event(fmt.Sprintf(
				"tdd ↻ usage-policy refusal on attempt %d — retrying once on the same ticket (BEH-389); the worktree diff survives on disk",
				attempt-1,
			))
		}

		log.Event(fmt.Sprintf("launching sandbox (cap %d min)", int(cfg.TddTimeout.Minutes())))
		outcome := session.Run(attemptArgs, session.Options{
			ContainerName:  attemptContainer,
			TranscriptFile: attemptTranscript,
			Timeout:        cfg.TddTimeout,
			IdleTimeout:    cfg.SessionIdleTimeout,
			Verbose:        args.verbose,
			Log:            log,
		})
		log.Event(fmt.Sprintf(
			"session exited (code %d) — transcript at logs/%s/%s", outcome.ExitCode, args.identifier, attemptTranscript,
		))

		// Ground truth, never self-report.
		truth = gitpkg.GatherTddGroundTruth(cfg.HerdPath, slug)
		result = verify.Tdd(truth)

		// Retry only the usage-policy refusal, and only while it left no handoff
		// commit but DID leave a worktree to resume — a refusal that struck before
		// the worktree existed has no surviving diff to recover, and the resume
		// prompt (which asserts the worktree already exists and forbids recreating
		// it) would otherwise burn a whole session on a false premise. Everything
		// else is the final verdict.
		if result.OK || !outcome.UsagePolicyRefusal || !truth.WorktreeExists {
			break
		}
	}

	if result.OK {
		plural := "s"
		if truth.CommitsAhead == 1 {
			plural = ""
		}
		log.Event(fmt.Sprintf(
			"tdd ✓ %s (%d commit%s ahead)", result.Reason, truth.CommitsAhead, plural,
		))
		// The worktree now goes to a (possibly non-Linux) reviewer. Strip the
		// sandbox-built `web/node_modules` so its Linux-only native bindings don't
		// crash the reviewer's gates — they install fresh for their own platform
		// (BEH-412). Warn-only: the handoff commit already landed, and a leftover
		// node_modules is recoverable, so a strip failure must not fail the run.
		if err := gitpkg.StripWorktreeNodeModules(worktreePath); err != nil {
			log.Event("⚠ could not strip web/node_modules from the worktree (" + err.Error() + ") — reviewer should `rm -rf web/node_modules && pnpm install`")
		}
	} else {
		log.Event("tdd ✗ " + result.Reason)
		// Don't let a recoverable diff vanish silently: if the session left
		// uncommitted work in the worktree (cap hit mid-verify — BEH-479; refusal
		// footgun — BEH-389), capture it as a harness recovery checkpoint commit so
		// the finished diff is a `git log` away on the feature branch instead of a
		// bare worktree needing manual rescue. This does NOT flip the verdict: the
		// work is unverified and the run still fails (exit 1); the checkpoint only
		// makes recovery cheap. The commit subject loudly marks it a checkpoint so a
		// reviewer never mistakes it for a verified handoff.
		if truth.WorktreeExists && !gitpkg.WorktreeClean(worktreePath) {
			if cErr := gitpkg.CheckpointCommit(worktreePath, args.identifier); cErr != nil {
				log.Event("⚠ uncommitted work remains in the worktree at " + worktreePath + " and the recovery checkpoint commit failed (" + cErr.Error() + ") — recover it manually before re-running")
			} else {
				log.Event("✓ harness recovery checkpoint committed on " + gitpkg.BranchName(slug) + " — the session's uncommitted diff is preserved (unverified: finish or re-run, then amend, before opening a PR)")
			}
		}
	}

	// File any harness-improvement findings the session dropped (after every session, per ADR-0001).
	filing.File(findingsDir, t.TeamID, args.identifier, client, client, log)

	if result.OK {
		return 0, nil
	}
	return 1, nil
}
