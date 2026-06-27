package stages

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

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

// retrospectiveSession prefixes this stage's transcript + findings dir under the
// ticket's log dir.
const retrospectiveSession = "retrospective"

// toPromptFindings maps the filing layer's already-filed finding classes to the
// prompt layer's shape, keeping the two packages decoupled (neither imports the
// other's type).
func toPromptFindings(prior []filing.PriorFinding) []prompt.FiledFinding {
	if len(prior) == 0 {
		return nil
	}
	out := make([]prompt.FiledFinding, len(prior))
	for i, p := range prior {
		out[i] = prompt.FiledFinding{Key: p.Key, Title: p.Title}
	}
	return out
}

// Retrospective runs the third and terminal stage: the /retrospective skill over
// a ticket's prior session transcripts, then files whatever harness-improvement
// findings the session dropped to Linear. Ground truth is the *presence* of
// /findings/out.json (an empty `[]` is success; absent means it never ran). On a
// fully clean ticket (branch pushed + findings filed) it tears the worktree down.
func Retrospective(cfg config.Config, log *runlog.Logger, runID string, args Args) Result {
	slug := strings.ToLower(args.Identifier)

	dry := ""
	if args.DryRun {
		dry = " (dry-run)"
	}
	log.Event(fmt.Sprintf("run %s — retrospective %s%s", runID, args.Identifier, dry))

	// Skip a misscheduled retrospective host-side, before the Linear fetch and the
	// sandbox cap: a ticket whose upstream /tdd + /review steps produced neither a
	// feature branch nor any session transcript has nothing to retrospect, so the
	// session could only emit an empty [] that masks the misscheduling or manufacture
	// a self-referential finding about the missing inputs (BEH-553). The skip is
	// BOTH-absent, deliberately not either-absent: the pipeline runs the retrospective
	// even on a *failed* slice ("exactly the run worth mining"), which routinely has
	// transcripts but no branch — one real input is enough to proceed. A skip is a
	// clean no-op, not a failure — return OK so it never reds the pipeline.
	if pre := verify.RetrospectivePreconditions(verify.RetrospectiveInputs{
		BranchExists:     gitpkg.BranchExists(cfg.HerdPath, slug),
		PriorTranscripts: hasUpstreamTranscripts(log.Dir),
	}); !pre.OK {
		log.Event(fmt.Sprintf("retrospective ⊘ skipped %s — %s", args.Identifier, pre.Reason))
		return Result{OK: true}
	}

	client := linear.NewClient(linear.NewTransport(cfg.LinearAPIKey))

	// The ticket is fetched for its team id (findings are filed back into it) and
	// for narration. Retrospective runs *last* and never claims the ticket — the
	// implementation tool already moved it to In Progress.
	t, err := client.FetchTicket(args.Identifier)
	if err != nil {
		return Result{Err: err}
	}
	log.Event(fmt.Sprintf("fetched %s — %s", t.Identifier, t.Title))

	findingsDir := log.FindingsDir(retrospectiveSession)
	if err := os.MkdirAll(findingsDir, 0o755); err != nil {
		// A full host disk fails this mkdir with ENOSPC. The disk being full isn't the
		// retrospective's fault and the in-sandbox agent can't fix it, so degrade to a
		// clear, actionable warning rather than a hard pipeline error that masquerades
		// as a stage crash and forces a manual re-run (BEH-540).
		if isDiskFull(err) {
			log.Event("retrospective ⚠ disk full — cannot create findings dir (" + err.Error() + "); free space (`pnpm store prune`, prune merged worktrees) and re-run")
			return Result{OK: false}
		}
		return Result{Err: err}
	}
	// Gather the finding classes a prior pipeline run already filed (the team's
	// open findings + this ticket's prior dropbox) so the session can treat them
	// as settled instead of re-deriving them (BEH-539). MUST run before
	// ClearDropbox, which is about to wipe that prior dropbox.
	prior := filing.AlreadyFiled(findingsDir, t.TeamID, client, log)
	if len(prior) > 0 {
		log.Event(fmt.Sprintf("retrospective re-run: %d already-filed finding class(es) passed to the session as settled context", len(prior)))
	}

	// Clear any stale dropbox from a prior retrospective run of this ticket before
	// the session writes. The dir is ticket+session keyed (reused across runs), so
	// without this a previous run's out.json would both re-file as duplicates and
	// make the "out.json present" ground truth pass even if this run never wrote.
	if err := filing.ClearDropbox(findingsDir); err != nil {
		return Result{Err: err}
	}

	p := prompt.BuildRetrospective(t, slug, toPromptFindings(prior))

	// runID is second-resolution; include the pid so two runs started in the same
	// second still get distinct container names (and distinct `docker kill` targets).
	containerName := fmt.Sprintf("herd-harness-%s-%d-%s", runID, os.Getpid(), retrospectiveSession)
	dockerArgs := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     findingsDir,
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          p,
		Model:           cfg.Model,
		ContainerName:   containerName,
	})

	if args.DryRun {
		log.Event("dry-run — not launching the container")
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- docker command ---\ndocker %s\n",
			p, strings.Join(dockerArgs, " "),
		)
		return Result{OK: true}
	}

	// Fail fast if Docker can't run the container before launching the session.
	if err := sandbox.Preflight(cfg.Image, filepath.Join(cfg.HerdPath, "agent-harness"), sandbox.ProbeRunner, sandbox.BuildImage, sandbox.FreeDiskBytes); err != nil {
		return Result{Err: err}
	}

	transcriptFile := runlog.TranscriptName(retrospectiveSession, runID)
	log.Event(fmt.Sprintf("launching sandbox (cap %d min)", int(cfg.RetrospectiveTimeout.Minutes())))
	outcome := session.Run(dockerArgs, session.Options{
		ContainerName:  containerName,
		TranscriptFile: transcriptFile,
		Timeout:        cfg.RetrospectiveTimeout,
		IdleTimeout:    cfg.SessionIdleTimeout,
		Verbose:        args.Verbose,
		Log:            log,
	})
	log.Event(fmt.Sprintf(
		"session exited (code %d) — transcript at logs/%s/%s", outcome.ExitCode, args.Identifier, transcriptFile,
	))

	// A spending-cap abort can fire after the skill wrote its up-front default `[]`
	// (BEH-536's incremental write), leaving an empty dropbox that the ground-truth
	// check below would read as "ran, found nothing" — masking the abort and
	// suppressing the retry-after-reset. Drop that empty default so the "absent file
	// = never ran" contract holds and the abort routes to ↻ retry; any real findings
	// written before the cap fired are preserved and still filed (BEH-568).
	if outcome.SpendingCapAbort {
		if removed, err := filing.ClearEmptyDropbox(findingsDir); err != nil {
			log.Event("retrospective ⚠ could not clear empty dropbox after spending-cap abort: " + err.Error())
		} else if removed {
			log.Event("retrospective — cleared the default empty dropbox left by the spending-cap abort (preserving the 'never ran' contract)")
		}
	}

	// Ground truth, never self-report: the retrospective ran iff it wrote the
	// findings dropbox. An empty `[]` is still present → success; an absent file
	// means the step never ran (DESIGN.md "Success is ground-truth"). Two
	// absent-dropbox cases aren't the agent's fault and get the retry class (↻),
	// not the misleading generic "never ran" (✗): a spending-cap abort (BEH-494)
	// and a 137 kill — OOM or wall-clock cap — that struck after the read-heavy
	// analysis but before the write (BEH-536).
	result := verify.Retrospective(filing.DropboxExists(findingsDir), outcome.SpendingCapAbort, outcome.ExitCode)
	switch {
	case result.OK:
		log.Event("retrospective ✓ " + result.Reason)
	case outcome.SpendingCapAbort, outcome.ExitCode == sandbox.ExitOOMKill:
		log.Event("retrospective ↻ " + result.Reason)
	default:
		log.Event("retrospective ✗ " + result.Reason)
	}

	// File whatever the session dropped: one Linear issue per finding, `[]` files
	// nothing. Safe to call even on failure — an absent dropbox files nothing.
	filing.File(findingsDir, t.TeamID, args.Identifier, client, client, log)

	if !result.OK {
		// Keep the worktree as a recoverable breadcrumb (DESIGN.md failure matrix).
		return Result{OK: false}
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

	return Result{OK: true}
}
