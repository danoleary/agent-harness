package stages

import (
	"fmt"
	"os"
	"path/filepath"
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

// implementationSession prefixes this stage's transcript + findings dir under
// the ticket's log dir (DESIGN.md "Logging": logs/BEH-NNN/<session>-<run-id>.jsonl).
const implementationSession = "implementation"

// claimReleaser is the slice of the Linear client the implementation stage's
// claim/release path needs. An interface keeps the PreClaimed branching (ADR-0003)
// unit-testable without a live Linear.
type claimReleaser interface {
	MoveToInProgress(identifier string) error
	ReleaseToTodo(identifier string) error
}

// eventLogger is the runlog narration surface these claim helpers write to.
type eventLogger interface{ Event(string) }

// claimForImplementation moves the ticket to In Progress for the implementation
// stage — unless selection already claimed it (preClaimed, the `pipeline --next`
// path), in which case re-claiming is a redundant no-op and is skipped (ADR-0003).
// On the hand-passed path preClaimed is false and it claims exactly as before,
// after the Docker preflight (BEH-316 ordering intact). It returns an error only
// when an actual claim mutation fails.
func claimForImplementation(client claimReleaser, identifier string, preClaimed bool, log eventLogger) error {
	if preClaimed {
		log.Event(identifier + " already claimed during selection (--next) — skipping redundant claim")
		return nil
	}
	if err := client.MoveToInProgress(identifier); err != nil {
		return err
	}
	log.Event(fmt.Sprintf("claimed %s → In Progress", identifier))
	return nil
}

// releaseIfPreClaimed returns a pre-claimed ticket to Todo after a Docker-preflight
// failure (ADR-0003): selection dequeued (claimed) a ticket the host turns out not
// to be able to work, so release it rather than strand it In Progress with no
// worktree. A no-op on the hand-passed path — nothing was claimed before preflight
// there, so BEH-316's claim-after-preflight ordering is untouched. Best-effort: a
// release error is warned, never fatal, since the preflight error stays the verdict.
func releaseIfPreClaimed(client claimReleaser, identifier string, preClaimed bool, log eventLogger) {
	if !preClaimed {
		return
	}
	if err := client.ReleaseToTodo(identifier); err != nil {
		log.Event("⚠ Docker preflight failed and releasing the pre-claimed ticket " + identifier + " back to Todo failed (" + err.Error() + ") — move it out of In Progress manually")
		return
	}
	log.Event("↩ released pre-claimed " + identifier + " back to Todo — Docker preflight failed before any work (ADR-0003)")
}

// retryableEnvCrash reports whether a failed implementation attempt crashed
// environmentally with nothing to salvage — no worktree was ever created — as
// opposed to running to completion and producing no handoff commit. Only the
// former is worth re-attempting: the crash strikes at the worktree-creation
// step's heavy host I/O and is usually transient, whereas a worktree that exists
// (even with no commit) means the agent ran and the diff, if any, is recoverable.
// A spending-cap abort is excluded — it has its own retry-after-reset handling.
// The pipeline reads the resulting Result.Retryable to decide whether to
// re-attempt the whole stage (BEH-543).
func retryableEnvCrash(truth verify.GroundTruth, capAborted bool) bool {
	return !truth.WorktreeExists && !capAborted
}

// Implementation runs the first stage: fetch + claim one ticket, run only the
// /tdd session in a Docker sandbox, verify the worktree + handoff commit by
// ground truth, and file any dropped findings. No push, no PR (review owns
// those). It is the extracted body of cmd/implementation's run() — the wrapper
// supplies cfg/log/runID/args so the pipeline can share one config load + runlog.
func Implementation(cfg config.Config, log *runlog.Logger, runID string, args Args) Result {
	slug := strings.ToLower(args.Identifier)

	dry := ""
	if args.DryRun {
		dry = " (dry-run)"
	}
	log.Event(fmt.Sprintf("run %s — implementation %s%s", runID, args.Identifier, dry))

	client := linear.NewClient(linear.NewTransport(cfg.LinearAPIKey))

	t, err := client.FetchTicket(args.Identifier)
	if err != nil {
		return Result{Err: err}
	}
	priority := t.Priority
	if priority == "" {
		priority = "No priority"
	}
	log.Event(fmt.Sprintf("fetched %s (%s) — %s", t.Identifier, priority, t.Title))

	// Advisory only: warn (don't skip) when the ticket cites code symbols that no
	// longer exist in web/src — the BEH-544 signal that the work likely already
	// merged, often under a *sibling* ticket the own-key TicketAlreadyOnMain scan
	// can't catch. Unlike that high-confidence exact-key skip below, the symbol
	// signal is heuristic (a cited symbol can be absent because the ticket asks to
	// *create* it), so it only surfaces for the human + the in-session agent
	// (steered by premiseCheckSteer) to act on — never drops the dispatch itself.
	if adv := gitpkg.ResolvedAdvisory(filepath.Join(cfg.HerdPath, "web", "src"), t.Identifier, t.Description); adv != "" {
		log.Event(adv)
	}

	// Advisory + prompt swap (BEH-554): when the ticket's OWN feature branch already
	// carries un-merged commits referencing it, a prior session resumed this
	// worktree and likely already landed a complete fix — a footgun TicketAlreadyOnMain
	// (work merged TO main) and ResolvedAdvisory (a sibling merge / deleted symbol)
	// both miss, because the fix lives on the SAME branch as un-merged commits and may
	// have ADDED code rather than deleting any. We don't skip (the branch can hold
	// incomplete work) — instead we steer the session to verify-and-handoff over
	// re-implementing by swapping in BuildTddResumedBranch.
	p := prompt.BuildTdd(t, slug)
	if adv := gitpkg.ResumedBranchAdvisory(cfg.HerdPath, slug, t.Identifier); adv != "" {
		log.Event(adv)
		p = prompt.BuildTddResumedBranch(t, slug)
	}
	findingsDir := log.FindingsDir(implementationSession)
	if err := os.MkdirAll(findingsDir, 0o755); err != nil {
		// Degrade a full-disk ENOSPC to a clear warning instead of an opaque hard
		// error (BEH-540); this runs before the claim, so nothing is left stranded.
		if isDiskFull(err) {
			log.Event("implementation ⚠ disk full — cannot create findings dir (" + err.Error() + "); free space (`pnpm store prune`, prune merged worktrees) and re-run")
			return Result{OK: false}
		}
		return Result{Err: err}
	}
	// Clear any stale dropbox from a prior run of this ticket before the session
	// writes (the findings dir is reused across runs; a leftover would be re-filed).
	if err := filing.ClearDropbox(findingsDir); err != nil {
		return Result{Err: err}
	}

	// runID is second-resolution; include the pid so two runs started in the same
	// second still get distinct container names (and distinct `docker kill` targets).
	containerName := fmt.Sprintf("herd-harness-%s-%d-%s", runID, os.Getpid(), implementationSession)
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

	if args.DryRun {
		log.Event("dry-run — not claiming the ticket, not launching the container")
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- docker command ---\ndocker %s\n",
			p, strings.Join(dockerArgs, " "),
		)
		return Result{OK: true}
	}

	// Skip a ticket whose work already merged on main: re-dispatching it spends a
	// whole worktree + install only to discover an empty diff and raise no PR, and
	// (worse) flips a done ticket back to In Progress (BEH-528). This runs before
	// the image build, the claim, and the launch so none of those costs are paid.
	// --force overrides for the rare false positive — a key that only coincidentally
	// appears in an unrelated downstream commit. (Deliberately after the dry-run
	// branch above: --dry-run stays a pure prompt/command inspector.)
	if !args.Force && gitpkg.TicketAlreadyOnMain(cfg.HerdPath, args.Identifier) {
		log.Event(fmt.Sprintf(
			"skipped %s — already merged on main (a recent commit references it); re-run with --force to dispatch anyway",
			args.Identifier,
		))
		return Result{OK: true}
	}

	// Fail fast if Docker can't run the container, so we never claim a ticket we
	// cannot actually work (the launch failure would otherwise leave it In
	// Progress with no worktree — BEH-316's exit-125 footgun). On the --next path
	// the ticket was already claimed during selection (ADR-0003), so a preflight
	// failure means we dequeued a ticket we can't work: release it back to Todo
	// rather than strand it In Progress (a no-op on the hand-passed path).
	if err := sandbox.Preflight(cfg.Image, filepath.Join(cfg.HerdPath, "agent-harness"), sandbox.ProbeRunner, sandbox.BuildImage, sandbox.FreeDiskBytes); err != nil {
		releaseIfPreClaimed(client, args.Identifier, args.PreClaimed, log)
		return Result{Err: err}
	}

	// Claim the ticket — unless selection already did on the --next path. On the
	// hand-passed path this runs after preflight exactly as before (BEH-316).
	if err := claimForImplementation(client, args.Identifier, args.PreClaimed, log); err != nil {
		return Result{Err: err}
	}

	// The tdd session runs at most twice. A terminal usage-policy refusal is a
	// known intermittent false-positive that disproportionately strikes long
	// agentic sessions (BEH-389); because the diff survives on disk (ADR-0002
	// real-path mount), a refusal that left no handoff commit is retried once on
	// the same ticket rather than discarded. Any other outcome — success, a real
	// failure, a non-refusal error — is final on the first attempt.
	const maxTddAttempts = 2

	worktreePath := gitpkg.WorktreePath(cfg.HerdPath, slug)
	var (
		truth      verify.GroundTruth
		result     verify.Result
		capAborted bool
	)
	for attempt := 1; attempt <= maxTddAttempts; attempt++ {
		attemptContainer := containerName
		attemptPrompt := p
		attemptLabel := implementationSession
		if attempt > 1 {
			attemptContainer = fmt.Sprintf("%s-retry%d", containerName, attempt)
			// The retry resumes the existing worktree (it already holds the surviving
			// diff) rather than recreating it (BEH-389).
			attemptPrompt = prompt.BuildTddResume(t, slug, worktreePath)
			attemptLabel = fmt.Sprintf("%s-retry%d", implementationSession, attempt)
			log.Event(fmt.Sprintf(
				"tdd ↻ usage-policy refusal on attempt %d — retrying once on the same ticket (BEH-389); the worktree diff survives on disk",
				attempt-1,
			))
		}

		log.Event(fmt.Sprintf("launching sandbox (cap %d min)", int(cfg.TddTimeout.Minutes())))
		// Retry a transient launch failure (overlay2/read-only-fs exit 125, or a 137
		// OOM-kill) before it becomes the verdict (BEH-542). Such a crash at the
		// worktree-creation step — the session's very first heavy host I/O — otherwise
		// discards the whole ticket with no commit and strands it In Progress (BEH-543).
		// Each launch retry uses a fresh --name (a wedged container's `--rm` teardown
		// may have failed, leaving the old name taken) and re-runs the SAME prompt: a
		// creation-time 125 left no worktree, so recreating is the correct recovery.
		var attemptTranscript string
		outcome, _ := session.RetryTransient(oomMaxAttempts, session.ConstantBackoff(oomRetryBackoff), time.Sleep, func(launch int) session.Outcome {
			name, label := attemptContainer, attemptLabel
			if launch > 1 {
				name = fmt.Sprintf("%s-launch%d", attemptContainer, launch)
				label = fmt.Sprintf("%s-launch%d", attemptLabel, launch)
				log.Event(fmt.Sprintf(
					"tdd ↻ transient launch failure — retry %d/%d after %s (BEH-542)",
					launch-1, oomMaxAttempts-1, oomRetryBackoff,
				))
			}
			attemptTranscript = runlog.TranscriptName(label, runID)
			return session.Run(buildArgs(name, attemptPrompt), session.Options{
				ContainerName:  name,
				TranscriptFile: attemptTranscript,
				Timeout:        cfg.TddTimeout,
				IdleTimeout:    cfg.SessionIdleTimeout,
				Verbose:        args.Verbose,
				Log:            log,
			})
		})
		log.Event(fmt.Sprintf(
			"session exited (code %d) — transcript at logs/%s/%s", outcome.ExitCode, args.Identifier, attemptTranscript,
		))

		// Ground truth, never self-report.
		truth = gitpkg.GatherTddGroundTruth(cfg.HerdPath, slug)
		result = verify.Tdd(truth)
		capAborted = outcome.SpendingCapAbort

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
		// A spending-cap abort (BEH-494) killed the session before it did any work,
		// so the failure isn't the agent's — surface it as a distinct
		// retry-after-reset class (↻) rather than the generic verdict (✗). The
		// recovery-checkpoint pass below is still a safe no-op (a capped session
		// leaves a clean worktree).
		if capAborted {
			log.Event("tdd ↻ session aborted before running — spending cap reached, retry after reset (BEH-494)")
		} else {
			log.Event("tdd ✗ " + result.Reason)
		}
		// Don't let a recoverable diff vanish silently: if the session left
		// uncommitted work in the worktree (cap hit mid-verify — BEH-479; refusal
		// footgun — BEH-389), capture it as a harness recovery checkpoint commit so
		// the finished diff is a `git log` away on the feature branch instead of a
		// bare worktree needing manual rescue. This does NOT flip the verdict: the
		// work is unverified and the run still fails (exit 1); the checkpoint only
		// makes recovery cheap. The commit subject loudly marks it a checkpoint so a
		// reviewer never mistakes it for a verified handoff.
		if truth.WorktreeExists && !gitpkg.WorktreeClean(worktreePath) {
			// "tdd", not implementationSession: the checkpoint subject is reviewer-
			// facing, so it uses this stage's human name (matching its "tdd ✓/✗" logs)
			// rather than the machine identifier used for container/transcript names.
			if cErr := gitpkg.CheckpointCommit(worktreePath, args.Identifier, "tdd"); cErr != nil {
				log.Event("⚠ uncommitted work remains in the worktree at " + worktreePath + " and the recovery checkpoint commit failed (" + cErr.Error() + ") — recover it manually before re-running")
			} else {
				log.Event("✓ harness recovery checkpoint committed on " + gitpkg.BranchName(slug) + " — the session's uncommitted diff is preserved (unverified: finish or re-run, then amend, before opening a PR)")
			}
		} else if retryableEnvCrash(truth, capAborted) {
			// The session crashed environmentally before it ever created a worktree —
			// the transient launch failures retried above (125/137) were exhausted, an
			// idle/cap kill struck pre-work, or the like. There is no partial state to
			// salvage, so leaving the ticket In Progress just strands it (BEH-543, from
			// the BEH-324 run that yielded nothing and sat claimed). Release the claim
			// back to Todo so a later run re-grabs it instead. Best-effort: a Linear
			// hiccup here must not crash the stage — warn and leave it claimed. (The
			// spending-cap abort is excluded: it's its own retry-after-reset class above.)
			if rErr := client.ReleaseToTodo(args.Identifier); rErr != nil {
				log.Event("⚠ no worktree was created and releasing the claim back to Todo failed (" + rErr.Error() + ") — move " + args.Identifier + " out of In Progress manually")
			} else {
				log.Event("↩ released claim — no worktree was created (environmental crash before any work); " + args.Identifier + " back to Todo for a later run to re-grab (BEH-543)")
			}
		}
	}

	// File any harness-improvement findings the session dropped (after every session, per ADR-0001).
	// Dedup runs exact-match first, then a best-effort semantic pass; a match is
	// recorded as a recurrence on the existing issue (client) instead of re-filed (BEH-573).
	filing.File(findingsDir, t.TeamID, args.Identifier, client, client, newSemanticMatcher(cfg), client, log)

	// Surface an environmental no-worktree crash to the pipeline so it can
	// re-attempt the whole stage once rather than discarding the slice (BEH-543).
	// A successful run is never retryable. Surface a spending-cap abort too so the
	// loop classifies a capped implementation as retry-after-reset (breaker-neutral)
	// rather than a ship failure.
	return Result{OK: result.OK, Retryable: !result.OK && retryableEnvCrash(truth, capAborted), SpendingCapAbort: capAborted}
}
