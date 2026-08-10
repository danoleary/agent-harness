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
	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/prompt"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/session"
	"github.com/beherd/agent-harness/internal/ticket"
	"github.com/beherd/agent-harness/internal/trackers"
	"github.com/beherd/agent-harness/internal/verify"
)

// tddCap selects the active-time cap for a tdd session by ticket shape: a
// multi-file extract-and-rewire refactor gets the larger TddLargeRefactorTimeout
// (it is inherently sequential and overran the ordinary 30m cap mid-surgery —
// BEH-441, BEH-688 Symptom 2); every other ticket gets the ordinary TddTimeout.
func tddCap(cfg config.Config, t ticket.Ticket) time.Duration {
	if t.IsLargeRefactor() {
		return cfg.TddLargeRefactorTimeout
	}
	return cfg.TddTimeout
}

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
// environmentally — the in-session launch retries (125/137, oomMaxAttempts)
// exhausted, or an idle-timeout kill struck mid-session — as opposed to running
// to completion and producing no handoff commit. Only the former is worth
// re-attempting: it is the host momentarily wedging, not a code/config fault, so
// the same run usually succeeds once it recovers. It keys on the session outcome,
// NOT truth.WorktreeExists: the harness now pre-creates the worktree host-side
// (BEH-636), so a crashed session still leaves the worktree present and the old
// worktree-existence proxy is permanently false (BEH-707). outcome.Retryable()
// already excludes a watchdog cap-kill (which ran a full session and has its own
// checkpoint handling) and a clean exit-0 no-op (which must not retry, preserving
// the pre-BEH-636 behaviour). A spending-cap abort is excluded too — it has its
// own retry-after-reset handling. The pipeline reads the resulting
// Result.Retryable to decide whether to re-attempt the whole stage (BEH-543).
func retryableEnvCrash(outcome session.Outcome, capAborted bool) bool {
	return outcome.Retryable() && !capAborted
}

// disjointWorkTrapped reports whether a failed tdd verdict is the BEH-609
// "verified work trapped on a disjoint branch" case: the session produced a
// worktree AND committed real work, but the branch roots at a disjoint history
// (an empty merge-base with origin/main — the BEH-355/BEH-500 condition), so the
// gate fails it even though the diff is genuine. Re-launching can never escape
// this — no in-sandbox work changes the branch's root commit — so the harness
// instead re-grafts the branch's content diff onto a fresh base off origin/main
// (RegraftOntoBase) and re-verifies, rather than discarding the run and re-running
// the same doomed pipeline. It must NOT fire on the ordinary failure shapes (no
// worktree, empty diff, or a healthy branch that failed for another reason).
func disjointWorkTrapped(truth verify.GroundTruth) bool {
	return truth.WorktreeExists && truth.CommitsAhead > 0 && truth.DisjointHistory
}

// provisionWorktree creates the feature worktree + canonical branch host-side and
// runs the Consumer's post_create toolchain hook in it (ADR-0008/BEH-636), retiring
// the coupling where the sandboxed agent ran new-worktree.sh and the host depended
// on its output. A CreateWorktree failure is fatal (there is nothing to run the
// session against); a post_create failure is warn-only, mirroring review's install
// degradation (BEH-490): the session can still install toolchain deps itself.
func provisionWorktree(cfg config.Config, slug, runID string, args Args, log *runlog.Logger) error {
	return provisionWorktreeWith(cfg, slug, log, gitpkg.CreateWorktree, func(worktreePath string) {
		runPostCreate(cfg, worktreePath, runID, args, log)
	})
}

// provisionWorktreeWith is provisionWorktree with its two side-effecting halves
// injected, so the create/post_create split is testable without a git repo or a
// docker daemon.
//
// Only the git creation is conditional on the worktree's absence; post_create runs
// on EVERY provisioning pass, including for a worktree that already exists. That
// existing-worktree path is not the already-provisioned state it looks like: a
// successful handoff strips web/node_modules so the (possibly non-Linux) reviewer
// installs for their own platform (BEH-412), and an OOM-killed install (BEH-523)
// never wrote it at all — yet a tree created by new-worktree.sh keeps its
// .worktree-ready sentinel, which by design means "the run finished", not "the
// install succeeded" (BEH-549). Skipping post_create there handed the session a tree
// that advertised readiness it did not have, and it only surfaced on the first failed
// test (BEH-796). Re-running is cheap and safe because a Consumer's post_create is
// required to be idempotent — herd's is `ln -sf` + a frozen install + an already-
// installed browser check, all near-no-ops on a provisioned tree.
func provisionWorktreeWith(
	cfg config.Config,
	slug string,
	log *runlog.Logger,
	createWorktree func(herdPath, branchPrefix, slug string) error,
	postCreate func(worktreePath string),
) error {
	worktreePath := gitpkg.WorktreePath(cfg.HerdPath, slug)
	if _, err := os.Stat(worktreePath); err != nil {
		if cErr := createWorktree(cfg.HerdPath, cfg.BranchPrefix, slug); cErr != nil {
			return fmt.Errorf("creating worktree %s: %w", worktreePath, cErr)
		}
		log.Event("created worktree " + worktreePath + " on " + gitpkg.BranchName(cfg.BranchPrefix, slug) + " (host-side)")
	} else {
		reused := "reusing existing worktree " + worktreePath
		if cfg.PostCreate != "" {
			reused += " — re-running post_create so a resumed tree opens with its toolchain deps present"
		}
		log.Event(reused)
	}
	if cfg.PostCreate != "" {
		postCreate(worktreePath)
	}
	return nil
}

// runPostCreate runs the Consumer's post_create command in the provisioned
// worktree via a secret-free container (BuildPostCreateRunArgs). It retries an
// OOM-kill (exit 137) — the same transient host-memory-pressure class the review
// prep-install rides out (BEH-524) — and degrades to a warning on a persistent
// non-zero exit rather than failing the run, so a flaky toolchain install doesn't
// strand an otherwise-workable ticket.
func runPostCreate(cfg config.Config, worktreePath, runID string, args Args, log *runlog.Logger) {
	base := fmt.Sprintf("herd-harness-%s-%d-postcreate", runID, os.Getpid())
	gc := sandbox.GateConfig{
		Image:          cfg.Image,
		HerdPath:       cfg.HerdPath,
		WorktreePath:   worktreePath,
		CacheVolume:    cfg.CacheVolume,
		CacheMountPath: cfg.CacheMountPath,
	}
	log.Event("provisioning worktree — running post_create toolchain setup")
	outcome, _ := session.RetryTransient(oomMaxAttempts, session.ConstantBackoff(oomRetryBackoff), time.Sleep, func(attempt int) session.Outcome {
		name := base
		transcript := runlog.StepLogName("postcreate", runID)
		if attempt > 1 {
			name = fmt.Sprintf("%s-retry%d", base, attempt)
			transcript = runlog.StepLogName(fmt.Sprintf("postcreate-retry%d", attempt), runID)
			log.Event(fmt.Sprintf(
				"tdd ↻ post_create OOM-killed (exit 137) — retry %d/%d after %s (BEH-524)",
				attempt-1, oomMaxAttempts-1, oomRetryBackoff,
			))
		}
		c := gc
		c.ContainerName = name
		out := session.Run(sandbox.BuildPostCreateRunArgs(c, cfg.PostCreate), session.Options{
			ContainerName:  name,
			TranscriptFile: transcript,
			Timeout:        cfg.TddTimeout,
			IdleTimeout:    cfg.SessionIdleTimeout,
			Verbose:        args.Verbose,
			Log:            log,
		})
		log.TeeLine(transcript, runlog.StepFooter(out.ExitCode))
		return out
	})
	if outcome.ExitCode != 0 {
		log.Event(fmt.Sprintf(
			"⚠ post_create exited %d — the worktree may lack toolchain deps; the session must install them before the gates run",
			outcome.ExitCode,
		))
	}
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
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: args.Identifier, Stage: "implementation", Message: fmt.Sprintf("run %s — implementation %s%s", runID, args.Identifier, dry)})

	client, err := trackers.New(cfg.Tracker, trackers.Secrets{LinearKey: cfg.LinearAPIKey, GitHubToken: cfg.GitHubToken, JiraBaseURL: cfg.JiraBaseURL, JiraEmail: cfg.JiraEmail, JiraToken: cfg.JiraAPIToken})
	if err != nil {
		return Result{Err: err}
	}

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
	p := prompt.BuildTdd(t, slug, cfg.BranchPrefix, cfg.Prompts.Implement)
	if adv := gitpkg.ResumedBranchAdvisory(cfg.HerdPath, cfg.BranchPrefix, slug, t.Identifier); adv != "" {
		log.Event(adv)
		p = prompt.BuildTddResumedBranch(t, slug, cfg.BranchPrefix, cfg.Prompts.Implement)
	}
	findingsDir := log.FindingsDir(implementationSession)
	if err := os.MkdirAll(findingsDir, 0o755); err != nil {
		// Degrade a full-disk ENOSPC to a clear warning instead of an opaque hard
		// error (BEH-540); this runs before the claim, so nothing is left stranded.
		if isDiskFull(err) {
			log.Event(diskFullWarning(implementationSession, err))
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
			Image:          cfg.Image,
			HerdPath:       cfg.HerdPath,
			FindingsDir:    findingsDir,
			CacheVolume:    cfg.CacheVolume,
			CacheMountPath: cfg.CacheMountPath,
			Prompt:         prmpt,
			Model:          cfg.Model,
			ContainerName:  name,
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
	if err := sandbox.Preflight(sandbox.PreflightFor(cfg.Image, cfg.HerdPath, cfg.Dockerfile)); err != nil {
		releaseIfPreClaimed(client, args.Identifier, args.PreClaimed, log)
		// PreflightAbort marks this as environmental (full disk / daemon down), not a
		// ticket failure, so the loop reclaims disk + backs off and the breaker stays
		// blind to it — otherwise the same top-of-queue ticket racks up identical
		// preflight failures and trips the breaker in seconds (the observed pattern).
		return Result{Err: err, PreflightAbort: true}
	}

	// Claim the ticket — unless selection already did on the --next path. On the
	// hand-passed path this runs after preflight exactly as before (BEH-316).
	if err := claimForImplementation(client, args.Identifier, args.PreClaimed, log); err != nil {
		return Result{Err: err}
	}

	// Create the worktree + branch host-side and run the Consumer's post_create
	// toolchain hook before launching the session (ADR-0008/BEH-636). This retires
	// the coupling where the sandboxed agent ran new-worktree.sh and the host
	// depended on its output. A failure means there is no worktree to run against, so
	// release the claim (best-effort) and surface it as a retryable env failure — the
	// same class as a launch crash that left no worktree (BEH-543).
	if err := provisionWorktree(cfg, slug, runID, args, log); err != nil {
		log.Event("tdd ✗ host-side worktree provisioning failed: " + err.Error())
		if rErr := client.ReleaseToTodo(args.Identifier); rErr != nil {
			log.Event("⚠ releasing the claim after a provisioning failure also failed (" + rErr.Error() + ") — move " + args.Identifier + " out of In Progress manually")
		}
		return Result{OK: false, Retryable: true}
	}

	// The tdd session runs at most twice. A terminal usage-policy refusal is a
	// known intermittent false-positive that disproportionately strikes long
	// agentic sessions (BEH-389); because the diff survives on disk (ADR-0002
	// real-path mount), a refusal that left no handoff commit is retried once on
	// the same ticket rather than discarded. Any other outcome — success, a real
	// failure, a non-refusal error — is final on the first attempt.
	const maxTddAttempts = 2

	worktreePath := gitpkg.WorktreePath(cfg.HerdPath, slug)
	// A multi-file extract-and-rewire refactor gets a larger active-time cap: it is
	// inherently sequential (extract N modules, then rewire N call sites) and
	// overran the ordinary cap mid-surgery, leaving an uncompilable checkpoint
	// (BEH-441, BEH-688 Symptom 2).
	sessionCap := tddCap(cfg, t)
	if sessionCap != cfg.TddTimeout {
		log.Event(fmt.Sprintf(
			"large extract-and-rewire refactor detected — granting a %d min active cap (vs the ordinary %d min) (BEH-688)",
			int(sessionCap.Minutes()), int(cfg.TddTimeout.Minutes()),
		))
	}
	var (
		truth      verify.GroundTruth
		result     verify.Result
		capAborted bool
		outcome    session.Outcome
	)
	for attempt := 1; attempt <= maxTddAttempts; attempt++ {
		attemptContainer := containerName
		attemptPrompt := p
		attemptLabel := implementationSession
		if attempt > 1 {
			attemptContainer = fmt.Sprintf("%s-retry%d", containerName, attempt)
			// The retry resumes the existing worktree (it already holds the surviving
			// diff) rather than recreating it (BEH-389).
			attemptPrompt = prompt.BuildTddResume(t, slug, worktreePath, cfg.BranchPrefix, cfg.Prompts.Implement)
			attemptLabel = fmt.Sprintf("%s-retry%d", implementationSession, attempt)
			log.Event(fmt.Sprintf(
				"tdd ↻ usage-policy refusal on attempt %d — retrying once on the same ticket (BEH-389); the worktree diff survives on disk",
				attempt-1,
			))
		}

		log.Structured(loopstream.Record{Kind: loopstream.KindSandboxLaunch, Ticket: args.Identifier, Stage: "implementation", Message: fmt.Sprintf("launching sandbox (cap %d min active)", int(sessionCap.Minutes()))})
		// Retry a transient launch failure (overlay2/read-only-fs exit 125, or a 137
		// OOM-kill) before it becomes the verdict (BEH-542). Such a crash at the
		// worktree-creation step — the session's very first heavy host I/O — otherwise
		// discards the whole ticket with no commit and strands it In Progress (BEH-543).
		// Each launch retry uses a fresh --name (a wedged container's `--rm` teardown
		// may have failed, leaving the old name taken) and re-runs the SAME prompt: a
		// creation-time 125 left no worktree, so recreating is the correct recovery.
		var attemptTranscript string
		outcome, _ = session.RetryTransient(oomMaxAttempts, session.ConstantBackoff(oomRetryBackoff), time.Sleep, func(launch int) session.Outcome {
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
				Timeout:        sessionCap,
				IdleTimeout:    cfg.SessionIdleTimeout,
				Verbose:        args.Verbose,
				Log:            log,
			})
		})
		log.Event(fmt.Sprintf(
			"session exited (code %d) — transcript at logs/%s/%s", outcome.ExitCode, args.Identifier, attemptTranscript,
		))

		// Ground truth, never self-report.
		truth = gitpkg.GatherTddGroundTruth(cfg.HerdPath, cfg.BranchPrefix, slug)
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

	// Rescue verified work trapped on a disjoint branch (BEH-609). The gate fails a
	// branch with no common ancestor with origin/main even when its committed diff is
	// genuine, and re-launching can never escape it — no in-sandbox work changes the
	// branch's root commit, so the same doomed pipeline would repeat forever. When the
	// ONLY thing wrong is the disjoint root, re-graft the branch's content diff onto a
	// fresh base off origin/main and re-verify; a clean regraft turns the trapped work
	// into a healthy, handoff-able branch. A regraft failure falls through to the
	// existing failed-verdict handling, which keeps the worktree for manual recovery.
	if !result.OK && !capAborted && disjointWorkTrapped(truth) {
		if rErr := gitpkg.RegraftOntoBase(worktreePath, "origin/main"); rErr != nil {
			log.Event("⚠ disjoint branch detected but the regraft failed (" + rErr.Error() + ") — keeping the worktree for manual recovery (BEH-609)")
		} else {
			log.Event("↻ verified work was trapped on a disjoint branch — re-grafted its content diff onto a fresh base off origin/main (BEH-609)")
			truth = gitpkg.GatherTddGroundTruth(cfg.HerdPath, cfg.BranchPrefix, slug)
			result = verify.Tdd(truth)
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
		} else if outcome.NoRealTurns {
			// A terminal $0 result means the model was never invoked — the session did
			// zero real work (a deterministic prompt-expansion no-op, see prompt.defang).
			// Re-launching the identical prompt fails identically, so this is NOT the
			// environmental-crash retry class: fail fast with a distinct diagnostic and
			// leave the ticket In Progress for a human rather than spinning a second
			// sandbox launch (BEH-691). retryableEnvCrash() already returns false here.
			log.Event("tdd ✗ session ran zero real turns ($0 cost) — a deterministic no-work crash (likely a prompt-expansion no-op); not re-attempting, needs a human (BEH-691)")
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
				log.Event("✓ harness recovery checkpoint committed on " + gitpkg.BranchName(cfg.BranchPrefix, slug) + " — the session's uncommitted diff is preserved (unverified: finish or re-run, then amend, before opening a PR)")
			}
		} else if retryableEnvCrash(outcome, capAborted) {
			// The session crashed environmentally and left the pre-provisioned worktree
			// clean — the transient launch failures retried above (125/137) were
			// exhausted, or an idle-timeout kill struck mid-session before any work
			// landed. There is no partial diff to salvage (the checkpoint branch above
			// handles the uncommitted case), so leaving the ticket In Progress just
			// strands it (BEH-543/BEH-707). Release the claim back to Todo so a later
			// run re-grabs it instead. Best-effort: a Linear hiccup here must not crash
			// the stage — warn and leave it claimed. (The spending-cap abort is excluded:
			// it's its own retry-after-reset class above.)
			if rErr := client.ReleaseToTodo(args.Identifier); rErr != nil {
				log.Event("⚠ environmental crash left no work and releasing the claim back to Todo failed (" + rErr.Error() + ") — move " + args.Identifier + " out of In Progress manually")
			} else {
				log.Event("↩ released claim — environmental crash left the worktree clean (no work salvageable); " + args.Identifier + " back to Todo for a later run to re-grab (BEH-543/BEH-707)")
			}
		}
	}

	// File any harness-improvement findings the session dropped (after every session, per ADR-0001).
	// Dedup runs exact-match first, then a best-effort semantic pass; a match is
	// recorded as a recurrence on the existing issue (client) instead of re-filed (BEH-573).
	// Only the retrospective classifies findings by audience (ADR-0011): the tdd
	// dropbox protocol asks for harness/environment friction only and is not taught to
	// classify, so it stays on the plain tracker filer rather than the audience router.
	filing.File(findingsDir, t.TeamID, args.Identifier, client, client, newSemanticMatcher(cfg), client, log)

	// Surface an environmental no-worktree crash to the pipeline so it can
	// re-attempt the whole stage once rather than discarding the slice (BEH-543).
	// A successful run is never retryable. Surface a spending-cap abort too so the
	// loop classifies a capped implementation as retry-after-reset (breaker-neutral)
	// rather than a ship failure.
	return Result{OK: result.OK, Retryable: !result.OK && retryableEnvCrash(outcome, capAborted), SpendingCapAbort: capAborted, SpendingCapResetTime: outcome.SpendingCapResetTime}
}
