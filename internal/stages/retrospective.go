package stages

import (
	"fmt"
	"os"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/filing"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/sandbox"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/verify"
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
// a ticket's prior session transcripts, then routes whatever harness-improvement
// findings the session dropped by audience. Ground truth is the *presence* of
// /findings/out.json (an empty `[]` is success; absent means it never ran). On a
// fully clean ticket (branch pushed + findings filed) it tears the worktree down.
// Every host-side effect goes through h, so the whole body is reachable from a
// test with hostio.NewFake().
func Retrospective(h hostio.Host, cfg config.Config, log *runlog.Logger, args Args) Result {
	slug := ticket.Slug(args.Identifier)

	dry := ""
	if args.DryRun {
		dry = " (dry-run)"
	}
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: args.Identifier, Stage: "retrospective", Message: fmt.Sprintf("run %s — retrospective %s%s", h.RunID(), args.Identifier, dry)})

	// Skip a misscheduled retrospective host-side, before the tracker fetch and the
	// sandbox cap: a ticket whose upstream /tdd + /review steps produced neither a
	// feature branch nor any session transcript has nothing to retrospect, so the
	// session could only emit an empty [] that masks the misscheduling or manufacture
	// a self-referential finding about the missing inputs (BEH-553). The skip is
	// BOTH-absent, deliberately not either-absent: the pipeline runs the retrospective
	// even on a *failed* slice ("exactly the run worth mining"), which routinely has
	// transcripts but no branch — one real input is enough to proceed. A skip is a
	// clean no-op, not a failure — return OK so it never reds the pipeline.
	if pre := verify.RetrospectivePreconditions(h, slug, hasUpstreamTranscripts(log.Dir)); !pre.OK {
		log.Event(fmt.Sprintf("retrospective ⊘ skipped %s — %s", args.Identifier, pre.Reason))
		return Result{OK: true}
	}

	client, err := h.Tracker()
	if err != nil {
		return Result{Err: err}
	}

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
			log.Event(diskFullWarning(retrospectiveSession, err))
			return Result{OK: false}
		}
		return Result{Err: err}
	}
	// Gather the finding classes a prior pipeline run already filed (the team's
	// open findings + this ticket's prior dropbox) so the session can treat them
	// as settled instead of re-deriving them (BEH-539). MUST run before
	// ClearDropbox, which is about to wipe that prior dropbox.
	prior := h.AlreadyFiled(findingsDir, t.TeamID)
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

	p := prompt.For(prompt.Retrospective, prompt.Context{
		Ticket: t, Slug: slug, BranchPrefix: cfg.BranchPrefix, Body: cfg.Prompts.Retro,
		Filed: toPromptFindings(prior),
	})
	run := hostio.AgentRun{
		Label:       retrospectiveSession,
		Model:       cfg.RetrospectiveModel,
		Prompt:      p,
		FindingsDir: findingsDir,
		Cap:         cfg.RetrospectiveTimeout,
	}

	if args.DryRun {
		log.Event("dry-run — not launching the container")
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- docker command ---\ndocker %s\n",
			p, strings.Join(h.AgentPreview(run), " "),
		)
		return Result{OK: true}
	}

	// Fail fast if Docker can't run the container before launching the session.
	if err := h.Preflight(); err != nil {
		return Result{Err: err}
	}

	log.Structured(loopstream.Record{Kind: loopstream.KindSandboxLaunch, Ticket: args.Identifier, Stage: "retrospective", Message: fmt.Sprintf("launching sandbox (cap %d min active)", int(cfg.RetrospectiveTimeout.Minutes()))})
	res := h.Agent(run)
	outcome := res.Outcome
	log.Event(fmt.Sprintf(
		"session exited (code %d) — transcript at logs/%s/%s", outcome.ExitCode, args.Identifier, res.Transcript,
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
	// means the step never ran (DESIGN.md "Success is ground-truth"). Three
	// absent-dropbox cases aren't the agent's fault and get the retry class (↻),
	// not the misleading generic "never ran" (✗): a spending-cap abort (BEH-494),
	// a 137 kill — OOM or wall-clock cap — that struck after the read-heavy
	// analysis but before the write (BEH-536), and a turn-0 no-op whose prompt
	// crashed the session before any work (BEH-709).
	result := verify.Retrospective(filing.DropboxExists(findingsDir), outcome)
	switch {
	case result.OK:
		log.Event("retrospective ✓ " + result.Reason)
	case outcome.SpendingCapAbort, outcome.ExitCode == sandbox.ExitOOMKill, outcome.TurnZeroNoOp:
		// A turn-0 no-op joins the cap-abort and OOM as a not-the-agent's-fault retry
		// class (↻): its `!`+backtick trigger is content/parse nondeterministic, so a
		// re-run may well succeed — unlike a genuine "never ran" skip (BEH-709).
		log.Event("retrospective ↻ " + result.Reason)
	default:
		log.Event("retrospective ✗ " + result.Reason)
	}

	// Route whatever the session dropped by audience (ADR-0011): project findings
	// file to the tracker (one issue per finding; `[]`/absent files nothing). Harness
	// findings write to the local artifact dir by default, or — when the Consumer opts
	// in with feedback.upstream = github — to the public harness repo (BEH-640), with
	// cross-project key dedup and project-tagged recurrences. Safe to call even on
	// failure — an absent dropbox routes nothing. Tracker dedup runs exact-match first,
	// then a best-effort semantic pass; a match is recorded as a recurrence on the
	// existing issue instead of re-filed (BEH-573).
	h.RouteFindings(findingsDir, t.TeamID, args.Identifier)

	if !result.OK {
		// Keep the worktree as a recoverable breadcrumb (DESIGN.md failure matrix).
		// Surface a spending-cap abort so the loop reads retry-after-reset; a
		// retrospective-only failure never blocks shipping (the PR already exists), so
		// the breaker keys off the run's Disposition (review's ship), not this OK.
		return capFailure(outcome)
	}

	// Reap the worktree only once the work has escaped it — retrospective filed AND
	// the branch has reached a PR (verify.WorktreeReap owns the rule and the why).
	// The worktree tears down host-side; the real-path mount makes its .git pointer
	// resolve from the main checkout.
	// WorktreeReap checks the push first and returns, so an unpushed branch never
	// spends the gh round-trip PRExists costs.
	reap := verify.WorktreeReap(h, slug)
	if !reap.OK {
		log.Event("worktree kept — " + reap.Reason)
	} else if err := h.RemoveWorktree(slug); err != nil {
		log.Event("worktree kept — removal failed: " + err.Error())
	} else {
		log.Event("worktree removed — " + reap.Reason)
	}

	return Result{OK: true}
}
