package stages

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/ci"
	"github.com/beherd/agent-harness/internal/config"
	gitpkg "github.com/beherd/agent-harness/internal/git"
	"github.com/beherd/agent-harness/internal/linear"
	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/pr"
	"github.com/beherd/agent-harness/internal/proc"
	"github.com/beherd/agent-harness/internal/prompt"
	"github.com/beherd/agent-harness/internal/runlog"
	"github.com/beherd/agent-harness/internal/sandbox"
	"github.com/beherd/agent-harness/internal/session"
	"github.com/beherd/agent-harness/internal/ticket"
	"github.com/beherd/agent-harness/internal/verify"
)

// reviewSession prefixes this stage's transcript under the ticket's log dir.
const reviewSession = "review"

// prCreateTimeout bounds the `gh pr create` network round-trip. Like the git
// remote ops it is a remote call — a stalled network or a blocking gh auth prompt
// would otherwise hang the harness at the very end of a run, stranding a finished,
// already-pushed branch (BEH-386, same hang class as the git fetch/push bound).
const prCreateTimeout = 2 * time.Minute

// ciGhTimeout bounds each individual host-side `gh` call in the CI watch (checks,
// run rerun, run view --log-failed). Generous because `--log-failed` can stream a
// large failed-job log, but still bounded so a stalled gh can't hang the harness
// (same hang class as prCreateTimeout, BEH-386).
const ciGhTimeout = 5 * time.Minute

// oomMaxAttempts / oomRetryBackoff bound the OOM-kill retry of the throwaway
// install container (BEH-524). A 137 under host memory pressure is transient — the
// same frozen install succeeded in ~4s on a bare retry once memory freed — so a
// couple of attempts with a short *fixed* beat between them (the install's
// footprint recovers in seconds) is enough; a persistent OOM still falls through.
const (
	oomMaxAttempts  = 3
	oomRetryBackoff = 10 * time.Second
)

// gateOOM* bound the OOM-kill retry of the heavier host-side gate (install + check
// + build) separately (BEH-530). A full build's footprint is far larger than an
// install's, so the install's short fixed beat doesn't transfer: on BEH-501 three
// back-to-back retries 10s apart died progressively EARLIER (memory pressure
// compounding, not recovering). An exponential backoff (30s → 60s → 120s) over one
// more attempt gives the starved Docker VM real time to reclaim before each retry.
const (
	gateOOMMaxAttempts = 4
	gateOOMBackoffBase = 30 * time.Second
	gateOOMBackoffCap  = 120 * time.Second
)

// Review runs the second stage: over the worktree implementation left behind, run
// a *cold* /review-worktree session (fixes committed locally only), then —
// host-side, holding GH_TOKEN — independently re-run the quality gates in a
// throwaway container and, only if they pass, push the branch and open the PR.
// Ground truth is the harness's own gate run, never the agent's self-report.
func Review(cfg config.Config, log *runlog.Logger, runID string, args Args) Result {
	slug := strings.ToLower(args.Identifier)
	worktreePath := gitpkg.WorktreePath(cfg.HerdPath, slug)

	dry := ""
	if args.DryRun {
		dry = " (dry-run)"
	}
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: args.Identifier, Stage: "review", Message: fmt.Sprintf("run %s — review %s%s", runID, args.Identifier, dry)})

	client := linear.NewClient(linear.NewTransport(cfg.LinearAPIKey))

	// The ticket is the intent to review against (review reconstructs intent from
	// the branch/issue/diff). Review does NOT claim or move it — implementation
	// already did, and the harness owns Linear state (no transition here).
	t, err := client.FetchTicket(args.Identifier)
	if err != nil {
		return Result{Err: err}
	}
	log.Event(fmt.Sprintf("fetched %s — %s", t.Identifier, t.Title))

	p := prompt.BuildReview(t, slug, worktreePath)
	// Review emits no findings (retrospective owns them) → no findings mount.
	containerName := fmt.Sprintf("herd-harness-%s-%d-%s", runID, os.Getpid(), reviewSession)
	dockerArgs := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     "",
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          p,
		Model:           cfg.Model,
		ContainerName:   containerName,
	})

	// Closures so an OOM-retry (BEH-524) can re-launch under a distinct --name: the
	// name must match Options.ContainerName for the timeout `docker kill` to target
	// the right container, and a retry must not collide with the killed attempt's.
	gateConfig := sandbox.GateConfig{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		WorktreePath:    worktreePath,
		PnpmStoreVolume: cfg.PnpmStoreVolume,
	}
	buildGateArgs := func(name string) []string {
		c := gateConfig
		c.ContainerName = name
		return sandbox.BuildGateRunArgs(c)
	}
	buildInstallArgs := func(name string) []string {
		c := gateConfig
		c.ContainerName = name
		return sandbox.BuildInstallRunArgs(c)
	}

	gateName := fmt.Sprintf("herd-harness-%s-%d-gate", runID, os.Getpid())
	gateArgs := buildGateArgs(gateName)

	// The implementation tool strips web/node_modules on handoff (BEH-412), so the
	// cold review session would otherwise discover it missing and pay a full
	// `pnpm install` mid-gate (BEH-490). Pre-populate it with a throwaway install
	// container before the session, mirroring new-worktree.sh.
	installName := fmt.Sprintf("herd-harness-%s-%d-install", runID, os.Getpid())
	installArgs := buildInstallArgs(installName)

	if args.DryRun {
		log.Event("dry-run — not launching the install, review, or gate containers")
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- install docker command ---\ndocker %s\n\n--- review docker command ---\ndocker %s\n\n--- gate docker command ---\ndocker %s\n",
			p, strings.Join(installArgs, " "), strings.Join(dockerArgs, " "), strings.Join(gateArgs, " "),
		)
		return Result{OK: true}
	}

	// Precondition: the implementation slice must have left a worktree. Without it
	// there is nothing to review (run `implementation <ticket>` first).
	if _, statErr := os.Stat(worktreePath); statErr != nil {
		log.Event(fmt.Sprintf("review ✗ no worktree at %s — run `implementation %s` first", worktreePath, args.Identifier))
		return Result{OK: false}
	}

	// Fail fast if Docker can't run the container before we burn the session.
	if err := sandbox.Preflight(cfg.Image, filepath.Join(cfg.HerdPath, "agent-harness"), sandbox.ProbeRunner, sandbox.BuildImage, sandbox.FreeDiskBytes); err != nil {
		return Result{Err: err}
	}

	// Heads-up if GH_TOKEN can't read check runs: the post-PR CI-watch needs a
	// classic repo-scoped PAT (fine-grained PATs lack the Checks permission). Not
	// fatal — the PR still ships and the watch degrades gracefully (BEH-476); this
	// just warns at the start instead of only surfacing after the PR is open.
	if ok, detail := ci.ChecksReadable(func(name string, args ...string) ([]byte, error) {
		return proc.CombinedOutputInDir(ciGhTimeout, cfg.HerdPath, name, args...)
	}); !ok {
		log.Event("review … warning: " + detail)
		fmt.Fprintln(os.Stderr, detail)
	}

	// --- prep: repopulate web/node_modules before the cold session (BEH-490) ---
	// The handoff strip (BEH-412) leaves the worktree without node_modules; install
	// it up front against the warm pnpm store so the session opens onto a ready
	// worktree instead of paying it mid-gate. Warn-only: the review-worktree skill
	// already treats a missing node_modules as "install first", so a transient prep
	// failure degrades to the agent installing in-session rather than aborting.
	//
	// Retry on an OOM-kill (exit 137) before that fallback (BEH-524): under host
	// memory pressure this install gets SIGKILLed, and degrading to warn-and-continue
	// dumped the ~10-min recovery install into the capped review session, starving it
	// of time to actually review. The OOM is transient (it succeeded in ~4s on a bare
	// retry once memory freed), so a short backoff-and-retry recovers it up front.
	installTranscript := runlog.StepLogName("install", runID)
	log.Event("prepping worktree (pnpm install --frozen-lockfile) before the review session")
	installOutcome, installAttempts := session.RetryTransient(oomMaxAttempts, session.ConstantBackoff(oomRetryBackoff), time.Sleep, func(attempt int) session.Outcome {
		name, transcript := installName, installTranscript
		if attempt > 1 {
			name = fmt.Sprintf("%s-retry%d", installName, attempt)
			transcript = runlog.StepLogName(fmt.Sprintf("install-retry%d", attempt), runID)
			log.Event(fmt.Sprintf(
				"review ↻ prep install OOM-killed (exit 137) — retry %d/%d after %s (BEH-524)",
				attempt-1, oomMaxAttempts-1, oomRetryBackoff,
			))
		}
		out := session.Run(buildInstallArgs(name), session.Options{
			ContainerName:  name,
			TranscriptFile: transcript,
			Timeout:        cfg.ReviewTimeout,
			// A memory-pressured install thrashes silently before it 137s, so give the
			// idle watchdog the same early-reap as the rest of the review family rather
			// than waiting out the full hard cap (BEH-535).
			IdleTimeout: cfg.SessionIdleTimeout,
			Verbose:     args.Verbose,
			Log:         log,
		})
		// This is a raw-stdout step log, not a stream-json transcript — pnpm output
		// just stops at the kill point. Stamp the exit so a reader sees the verdict
		// instead of an opaque truncation (BEH-537).
		log.TeeLine(transcript, runlog.StepFooter(out.ExitCode))
		return out
	})
	if installOutcome.ExitCode != 0 {
		log.Event(fmt.Sprintf(
			"review … warning: worktree prep install exited %d after %d attempt(s) — session will install in-session if needed",
			installOutcome.ExitCode, installAttempts,
		))
	}

	// --- review session (cold /review-worktree; fixes committed locally) ---
	transcriptFile := runlog.TranscriptName(reviewSession, runID)
	log.Structured(loopstream.Record{Kind: loopstream.KindSandboxLaunch, Ticket: args.Identifier, Stage: "review", Message: fmt.Sprintf("launching review session (cap %d min)", int(cfg.ReviewTimeout.Minutes()))})
	reviewOutcome := session.Run(dockerArgs, session.Options{
		ContainerName:  containerName,
		TranscriptFile: transcriptFile,
		Timeout:        cfg.ReviewTimeout,
		IdleTimeout:    cfg.SessionIdleTimeout,
		Verbose:        args.Verbose,
		Log:            log,
	})
	log.Event(fmt.Sprintf(
		"review session exited (code %d) — transcript at logs/%s/%s", reviewOutcome.ExitCode, args.Identifier, transcriptFile,
	))
	// A spending-cap abort (BEH-494) killed the review session before it could
	// review anything — name that distinct retry-after-reset class so a log reader
	// isn't misled by the host-side gate result below (which still runs against the
	// committed TDD handoff regardless of whether the review agent did any work).
	if reviewOutcome.SpendingCapAbort {
		log.Event("review ↻ session aborted before running — spending cap reached, retry after reset (BEH-494)")
	}

	// Did the qualitative seven-lens review actually run? The host-side gate re-run
	// below authorises the push, but a green gate only proves the diff compiles — it
	// is NOT a review. A session OOM-killed mid-gate (exit 137) emits no "## Review:"
	// verdict, and silently shipping it on green gates loses exactly the review that
	// matters most on a risky diff (BEH-525, from the BEH-499 OOM). Flag that here so
	// the run log carries the signal rather than masquerading as a full review pass.
	completeness := verify.ReviewQualitative(reviewOutcome.ExitCode, reviewOutcome.ReviewVerdictEmitted)
	if !completeness.Complete {
		log.Event("review ⚠ " + completeness.Reason)
		fmt.Fprintln(os.Stderr, "warning: "+completeness.Reason)
	} else {
		log.Event("review ✓ " + completeness.Reason)
	}

	// A review that ran but declared a blocked disposition found a Blocker/Important
	// finding it could not autonomously resolve (BEH-580). The autonomous pipeline has
	// no human to answer the skill's approval prompt, so this fails the push closed
	// below rather than shipping the finding to a PR unaddressed (the BEH-439 leak).
	if reviewOutcome.ReviewBlocked {
		log.Event("review ⚠ verdict declared a blocked disposition — an unresolved blocker/important finding needs a human decision; will not push (BEH-580)")
	}

	// --- ground truth + push gate (harness, host-side) ---
	// Refresh origin/main so the commit range + PR base are current.
	if err := gitpkg.FetchMain(cfg.HerdPath); err != nil {
		log.Event("review … warning: could not fetch origin/main: " + err.Error())
	}

	// Re-run `pnpm check && pnpm typecheck` in a throwaway container. The gate's
	// exit code is the ONLY thing that authorises a push — never the agent's report.
	// It deliberately runs `typecheck`, not the full memory-heavy `pnpm run build`,
	// which OOM-kills correct diffs in the sandbox (BEH-529); CI's full build is the
	// SSR-shell backstop (this stage watches CI post-PR via ci.WatchAndFix).
	//
	// Retry on an OOM-kill (exit 137) here too (BEH-524): the gate's own
	// `pnpm install` (and even typecheck) can be SIGKILLed under memory pressure,
	// which would flip a genuinely green branch red and push nothing. A 137 is
	// environmental, never the diff — a real gate failure (check/typecheck error)
	// returns a non-137 code and is final on the first attempt.
	// runHostGate runs `pnpm check && typecheck` in a throwaway container under the
	// OOM-retry, keyed by a base container name + a transcript tag so it can run more
	// than once per stage (the pre-push conflict path re-gates the resolved tree —
	// BEH-581). baseName/transcriptTag are the (runID-derived) first-attempt names;
	// retries suffix them.
	runHostGate := func(baseName, transcriptTag string) session.Outcome {
		gateBackoff := session.ExponentialBackoff(gateOOMBackoffBase, gateOOMBackoffCap)
		outcome, _ := session.RetryTransient(gateOOMMaxAttempts, gateBackoff, time.Sleep, func(attempt int) session.Outcome {
			name, transcript := baseName, runlog.GateTranscriptName(transcriptTag)
			if attempt > 1 {
				name = fmt.Sprintf("%s-retry%d", baseName, attempt)
				transcript = runlog.GateTranscriptName(fmt.Sprintf("%s-retry%d", transcriptTag, attempt))
				log.Event(fmt.Sprintf(
					"review ↻ host-side gate OOM-killed (exit 137) — retry %d/%d after %s (BEH-530)",
					attempt-1, gateOOMMaxAttempts-1, gateBackoff(attempt-1),
				))
			}
			out := session.Run(buildGateArgs(name), session.Options{
				ContainerName:  name,
				TranscriptFile: transcript,
				Timeout:        cfg.ReviewTimeout,
				IdleTimeout:    cfg.SessionIdleTimeout,
				Verbose:        args.Verbose,
				Log:            log,
			})
			// Raw-stdout step log: the gate's `pnpm install`/`tsgo` just stops mid-output
			// on an OOM-kill. Stamp the exit so the abrupt end is self-describing rather
			// than needing a run.jsonl cross-reference to confirm the 137 (BEH-537).
			log.TeeLine(transcript, runlog.StepFooter(out.ExitCode))
			return out
		})
		return outcome
	}

	log.Event(fmt.Sprintf("re-running gates host-side (cap %d min)", int(cfg.ReviewTimeout.Minutes())))
	gateOutcome := runHostGate(gateName, runID)
	gateExit := gateOutcome.ExitCode

	// The gate runs against the worktree's working tree (committed + uncommitted),
	// but Push ships only the committed tip — so the push is authorised only when
	// the worktree is also clean, guaranteeing what shipped is exactly what the gate
	// validated (never the agent's say-so, and never an unverified working tree).
	clean := gitpkg.WorktreeClean(worktreePath)
	// A clean worktree whose committed tip makes zero net change against origin/main is
	// the BEH-603 recommend-close disposition: nothing to ship, so the ticket should be
	// closed as a duplicate/superseded rather than opened as an empty-commit PR (the PR
	// #642 mistake). Read host-side; checked inside verify.Review only when the tree is
	// clean (a dirty tree's "empty" committed diff may hide uncommitted work).
	emptyDiff := gitpkg.BranchDiffEmpty(worktreePath)

	// comment posts a best-effort Linear breadcrumb so a gate-green, reviewed branch
	// that can't ship autonomously surfaces on the ticket instead of sitting silent in
	// a worktree (BEH-581 conflict path). A failure to comment is logged, never fatal.
	comment := func(body string) {
		if err := client.AddComment(args.Identifier, body); err != nil {
			log.Event("review … warning: could not post Linear breadcrumb: " + err.Error())
		}
	}

	// completeness gates the push closed (BEH-569): a green gate over a clean worktree
	// is not enough — the review session must have emitted its verdict. A spending-cap
	// abort / OOM that killed the review before it reviewed leaves the diff unreviewed,
	// so the push fails closed and the worktree is kept for a resumed review rather than
	// opening a PR on a gate re-run that nobody mistakes for a review.
	result := verify.Review(verify.ReviewOutcome{GatesGreen: gateExit == 0, WorktreeClean: clean, ReviewComplete: completeness.Complete, ReviewBlocked: reviewOutcome.ReviewBlocked, EmptyDiff: emptyDiff})
	// Recommend-close (BEH-603): a zero-net-diff branch is NOT a push and NOT a failure
	// to retry. Keep the worktree as the audit artifact and return the disposition so
	// the loop keeps the ticket In Progress for a human (never released to Todo, never
	// re-run to the same conclusion) and posts the recommending-close breadcrumb. The
	// breadcrumb is left by the loop alone — the authoritative ticket-state owner, as
	// for every other no-PR disposition (cap-abort, no-PR release) — so the stage stays
	// silent here rather than double-posting. The worktree is clean (verify requires
	// it), so there is nothing uncommitted to recover.
	if result.RecommendClose {
		log.Event("review ⊘ " + result.Reason + " — keeping worktree, nothing pushed; recommending close (BEH-603)")
		return Result{OK: false, RecommendClose: true}
	}
	if !result.OK {
		// A review killed mid-edit (spending cap / OOM) leaves its in-progress fixes
		// uncommitted. Without capturing them they vanish on the next resume — the
		// worktree is re-derived from the committed tip — and the resumed review
		// re-judges the original diff from scratch, flipping the verdict on the very
		// line review-1 had started fixing (BEH-559). Checkpoint-commit them (mirroring
		// the tdd stage's BEH-479 safety net) so the started work survives and the
		// resuming review's merge-base diff includes it. Done AFTER the push decision
		// above (made on the pre-checkpoint `clean`), so an unverified, half-applied fix
		// is never pushed — only preserved. A no-op when the worktree is already clean
		// (a red-but-clean gate has nothing uncommitted to recover).
		if !clean {
			if cErr := gitpkg.CheckpointCommit(worktreePath, args.Identifier, reviewSession); cErr != nil {
				log.Event("⚠ review session left uncommitted edits and the recovery checkpoint commit failed (" + cErr.Error() + ") — recover them manually at " + worktreePath)
			} else {
				log.Event("✓ harness recovery checkpoint committed on " + gitpkg.BranchName(slug) + " — the review session's in-progress edits are preserved (unverified: a resumed review will see them, finish or re-run before opening a PR)")
			}
		}
		// Red/crash/dirty → keep the worktree (recoverable artifact), do not push.
		// A spending-cap abort that killed the review before it could ship is surfaced
		// so the loop classifies this as retry-after-reset (breaker-neutral), not a
		// ship failure that should advance the circuit breaker.
		log.Event(fmt.Sprintf("review ✗ %s (gate exit %d) — keeping worktree, nothing pushed", result.Reason, gateExit))
		return Result{OK: false, SpendingCapAbort: reviewOutcome.SpendingCapAbort}
	}
	log.Event("review ✓ " + result.Reason)

	// --- rebase onto the latest base, then push + PR (only on a green harness gate) ---
	// Re-fetch origin/main right before the rebase: the earlier FetchMain ran before the
	// multi-minute host gate re-run, and a sibling PR can merge *during* that gate — the
	// exact long-pipeline staleness BEH-570 targets — so rebasing onto the pre-gate ref
	// would still open a stale-base PR and lean on the (expensive) reactive CI-watch
	// rebase. A fresh fetch here makes the proactive rebase replay onto the truly-latest
	// base. Non-fatal like the earlier fetch: on failure we rebase onto the ref we have
	// and the reactive path remains the backstop.
	if err := gitpkg.FetchMain(cfg.HerdPath); err != nil {
		log.Event("review … warning: could not re-fetch origin/main before rebase: " + err.Error())
	}

	// Rebase onto origin/main before pushing so a sibling PR that merged during this long
	// pipeline can't strand the branch on a stale base — the merge-conflict dead-end
	// BEH-570 hit, where the conflict only surfaced post-PR in the CI watch and was left
	// as a manual step. The worktree is clean here (verified just above), so the replay's
	// reset --hard can't discard uncommitted work (BEH-618). A clean replay moves the tip
	// onto the current base. A genuine content
	// conflict no longer dead-ends (BEH-581): rather than strand ~30 min of reviewed,
	// gate-green work, the harness launches a bounded sandboxed conflict-resolution
	// session over the worktree (mirroring the post-PR ciFixRunner), re-runs the host
	// gate on the resolved tree, and only then pushes. resolvePrePushConflict returns
	// false when it could not land a clean, re-gated rebase — keeping the worktree and
	// leaving a Linear breadcrumb so the work surfaces autonomously.
	if gitpkg.RebaseOntoMain(worktreePath) == gitpkg.RebaseConflict {
		// A disjoint history (no common ancestor with origin/main) is NOT a content
		// conflict (BEH-597): the rebase collided trying to replay every one of the
		// branch's disjoint commits, so the BEH-581 conflict-resolution session is the
		// wrong tool — it would burn a sandbox re-discovering the empty merge-base. The
		// tdd gate now fails this upstream (verify.Tdd's DisjointHistory check), so a
		// disjoint branch should never reach here; this is the defence-in-depth backstop
		// for a resumed/standalone review. Keep the worktree for manual recovery
		// (`git reset --hard origin/main` + cherry-pick the handoff commits) and surface it.
		if gitpkg.IsDisjointFrom(worktreePath, "origin/main") {
			gitpkg.AbortRebase(worktreePath)
			log.Event("review ✗ disjoint branch history — no common ancestor with origin/main; not a content conflict, needs manual recovery (BEH-597) — keeping worktree, nothing pushed")
			comment(fmt.Sprintf(
				"Branch `%s` has a disjoint history from `main` (no common ancestor / empty merge-base), so it cannot be rebased or merged as-is. This is not a content conflict — the handoff commits need to be re-applied onto current `main` (e.g. `git reset --hard origin/main` then cherry-pick them). The branch passed cold review and the harness gate; it is waiting in a worktree. (BEH-597)",
				gitpkg.BranchName(slug),
			))
			return Result{OK: false}
		}
		resolved := resolvePrePushConflict(cfg, args, slug, worktreePath, runID, t, log, comment,
			func() session.Outcome { return runHostGate(gateName+"-postrebase", runID+"-postrebase") })
		if !resolved {
			return Result{OK: false}
		}
	}
	log.Event("rebased " + gitpkg.BranchName(slug) + " onto origin/main")

	if err := gitpkg.Push(cfg.HerdPath, slug); err != nil {
		log.Event("review ✗ push failed: " + err.Error() + " — keeping worktree")
		return Result{OK: false}
	}
	log.Event("pushed " + gitpkg.BranchName(slug) + " to origin")

	subjects := gitpkg.CommitSubjects(cfg.HerdPath, slug)
	url, err := createPR(cfg.HerdPath, slug, pr.BuildTitle(t), pr.BuildBody(t, subjects))
	if err != nil {
		log.Event("review ✗ gh pr create failed: " + err.Error() + " — branch pushed, open the PR manually")
		return Result{OK: false}
	}
	log.Structured(loopstream.Record{Kind: loopstream.KindPROpened, Ticket: args.Identifier, Stage: "review", Message: "review ✓ PR opened: " + url})

	// The branch is pushed and the PR is open: from here on the ticket has "reached a
	// pushed PR" regardless of how the CI watch turns out, so the loop's circuit
	// breaker must treat it as a success (reset), never a ship failure (DESIGN.md
	// §Circuit breaker). Carry the signal on every return below.

	// --- post-PR: watch CI and auto-fix red checks (host-side, BEH-414) ---
	// Local gates passing is not CI passing (env/toolchain/flake/lockfile skew).
	// Poll the PR head's checks; on a real failure, run a sandboxed fix session
	// over the same worktree, push the fix, and re-poll — bounded by attempts +
	// wall-clock. The gh polling/log-fetch is host-side (ADR-0002); only the
	// diagnose+fix happens in the sandbox.
	ciCfg := ci.Config{
		MaxFixAttempts: cfg.CIMaxFixAttempts,
		Budget:         cfg.CIFixBudget,
		PollInterval:   cfg.CIPollInterval,
		PollBudget:     cfg.CIPollBudget,
		PollStall:      cfg.CIPollStall,
	}
	driver := ci.NewGhDriver(
		cfg.HerdPath, gitpkg.BranchName(slug), ciCfg, ciGhTimeout,
		ciFixRunner(cfg, args, slug, worktreePath, runID, t, log),
		func() error { return gitpkg.Push(cfg.HerdPath, slug) },
		func() (ci.RebaseVerdict, error) { return rebaseOntoBase(cfg.HerdPath, worktreePath, slug) },
		func() bool { return gitpkg.BranchDiffEmpty(worktreePath) },
	)
	log.Event("watching CI for " + gitpkg.BranchName(slug) + " …")
	ciResult := ci.WatchAndFix(driver, ciCfg, time.Now)
	// Zero-net-diff short-circuit (BEH-602): the branch became a no-op against the
	// latest origin/main only AFTER the pre-push rebase (a sibling PR landed the same
	// fix during the multi-minute gate, which the pre-rebase BEH-603 push-gate check
	// couldn't see), so the PR is open but there is nothing for CI to validate. Honour
	// the recommend-close disposition rather than burn the poll budget — keep the PR +
	// worktree and let the loop flag the ticket for a human to close as superseded.
	if ciResult.RecommendClose {
		log.Event("review ⊘ " + ciResult.Reason + " — keeping PR + worktree; recommending close (BEH-602)")
		return Result{OK: false, ReachedPushedPR: true, RecommendClose: true}
	}
	if ciResult.SpendingCapAbort {
		// The auto-fix session hit an active spending cap — not a code defect or
		// unfixable CI, just a billing window that resets. Name the retry-after-reset
		// class (↻) so a re-dispatch after the cap resets lands the fix, instead of a
		// spurious "CI did not go green" failure (BEH-571). PR + worktree are kept.
		log.Event("review ↻ CI auto-fix deferred — spending cap reached, retry after reset (BEH-494) — keeping PR + worktree")
		return Result{OK: false, ReachedPushedPR: true, SpendingCapAbort: true}
	}
	if !ciResult.OK {
		log.Event("review ✗ CI did not go green: " + ciResult.Reason + " — keeping PR + worktree")
		if s := ci.Summarize(ciResult.Failing); s != "" {
			fmt.Fprintf(os.Stderr, "\nFailing CI checks for %s:\n%s", gitpkg.BranchName(slug), s)
		}
		// CI red after the auto-fix budget still leaves a reviewable PR for a human to
		// take over — NOT a ship failure, so the breaker must not count it.
		return Result{OK: false, ReachedPushedPR: true}
	}
	log.Event("review ✓ " + ciResult.Reason)

	return Result{OK: true, ReachedPushedPR: true}
}

// ciFixRunner returns the ci.GhDriver's fix callback: it launches a sandboxed
// Claude session over the existing worktree, steered by BuildCIFix with the
// fetched failing logs, then enforces ground truth — a non-zero session exit or
// a worktree left dirty (the agent didn't commit) is a failure, so the harness
// never pushes an unverified or self-reported-only fix. Each attempt gets a
// unique container name + transcript.
func ciFixRunner(cfg config.Config, args Args, slug, worktreePath, runID string, t ticket.Ticket, log *runlog.Logger) func(string, bool) error {
	attempt := 0
	return func(ciLogs string, logAvailable bool) error {
		attempt++
		// Capture the tip before the session so we can tell afterwards whether the
		// agent actually committed a fix or correctly concluded there was nothing to
		// fix (BEH-561). A read failure leaves headBefore empty, which degrades to
		// "treat any HEAD as a real commit" — never a spurious empty commit.
		headBefore, _ := gitpkg.HeadSHA(worktreePath)
		fixPrompt := prompt.BuildCIFix(t, slug, worktreePath, ciLogs, logAvailable)
		containerName := fmt.Sprintf("herd-harness-%s-%d-cifix-%d", runID, os.Getpid(), attempt)
		fixArgs := sandbox.BuildDockerRunArgs(sandbox.Config{
			Image:           cfg.Image,
			HerdPath:        cfg.HerdPath,
			FindingsDir:     "",
			PnpmStoreVolume: cfg.PnpmStoreVolume,
			Prompt:          fixPrompt,
			Model:           cfg.Model,
			ContainerName:   containerName,
		})
		transcript := runlog.TranscriptName(fmt.Sprintf("cifix-%d", attempt), runID)
		log.Event(fmt.Sprintf("CI red — launching auto-fix session %d (cap %d min)", attempt, int(cfg.ReviewTimeout.Minutes())))
		outcome := session.Run(fixArgs, session.Options{
			ContainerName:  containerName,
			TranscriptFile: transcript,
			Timeout:        cfg.ReviewTimeout,
			IdleTimeout:    cfg.SessionIdleTimeout,
			Verbose:        args.Verbose,
			Log:            log,
		})
		if err := fixSessionError(outcome, attempt); err != nil {
			return err
		}
		// Ground truth, never the agent's say-so: the fix must be committed (clean
		// worktree) or the harness has nothing trustworthy to push.
		if !gitpkg.WorktreeClean(worktreePath) {
			return fmt.Errorf("auto-fix session %d left uncommitted changes — not pushing", attempt)
		}
		// The agent may have correctly concluded the red is not a code defect (a
		// cancelled/superseded/flaky run) and committed nothing. Don't force a
		// speculative diff: add an empty commit so the re-push gives CI a fresh HEAD
		// to re-run against; a no-op if a real fix moved HEAD (BEH-561).
		if err := gitpkg.EnsureCIRerunCommit(worktreePath, headBefore); err != nil {
			return fmt.Errorf("auto-fix session %d: re-trigger commit failed: %w", attempt, err)
		}
		return nil
	}
}

// resolvePrePushConflict launches a bounded sandboxed conflict-resolution session
// when the proactive pre-push rebase hits a genuine content conflict (BEH-581) —
// the dead-end that previously stranded a fully-reviewed, gate-green branch in a
// worktree with no autonomous recovery. It mirrors ciFixRunner's pattern: a Claude
// session over the existing worktree (steered by BuildRebaseFix to rebase onto
// origin/main, resolve, and commit), then ground-truth enforcement — the verdict
// is never the agent's say-so but verify.RebaseResolution over the worktree's git
// state (session exit, clean tree, branch actually rebased). On a clean resolution
// it re-runs the host gate over the rewritten tree (regate) and only returns true
// when that gate is green and the worktree clean, authorising the push. Any failure
// keeps the worktree and leaves a Linear breadcrumb so the work surfaces (a
// spending-cap abort defers quietly — it retries after reset, not a conflict).
func resolvePrePushConflict(
	cfg config.Config, args Args, slug, worktreePath, runID string,
	t ticket.Ticket, log *runlog.Logger, comment func(string),
	regate func() session.Outcome,
) bool {
	log.Event("review ↻ pre-push rebase hit a content conflict — launching a sandboxed conflict-resolution session (BEH-581)")
	fixPrompt := prompt.BuildRebaseFix(t, slug, worktreePath)
	containerName := fmt.Sprintf("herd-harness-%s-%d-rebasefix", runID, os.Getpid())
	fixArgs := sandbox.BuildDockerRunArgs(sandbox.Config{
		Image:           cfg.Image,
		HerdPath:        cfg.HerdPath,
		FindingsDir:     "",
		PnpmStoreVolume: cfg.PnpmStoreVolume,
		Prompt:          fixPrompt,
		Model:           cfg.Model,
		ContainerName:   containerName,
	})
	transcript := runlog.TranscriptName("rebasefix", runID)
	log.Event(fmt.Sprintf("launching conflict-resolution session (cap %d min)", int(cfg.ReviewTimeout.Minutes())))
	outcome := session.Run(fixArgs, session.Options{
		ContainerName:  containerName,
		TranscriptFile: transcript,
		Timeout:        cfg.ReviewTimeout,
		IdleTimeout:    cfg.SessionIdleTimeout,
		Verbose:        args.Verbose,
		Log:            log,
	})
	log.Event(fmt.Sprintf("conflict-resolution session exited (code %d)", outcome.ExitCode))

	// Ground truth over the worktree, never the agent's report: did the session
	// actually rebase onto origin/main and leave a clean tree?
	res := verify.RebaseResolution(verify.RebaseResolutionOutcome{
		SessionExit:      outcome.ExitCode,
		SpendingCapAbort: outcome.SpendingCapAbort,
		WorktreeClean:    gitpkg.WorktreeClean(worktreePath),
		Rebased:          gitpkg.IsRebasedOnto(worktreePath, "origin/main"),
	})
	if !res.OK {
		// Restore a clean, on-branch worktree for the next resume (a failed session may
		// have left it mid-rebase). Best-effort: a no-op when none is in progress.
		gitpkg.AbortRebase(worktreePath)
		if res.SpendingCapAbort {
			// Retry-after-reset, not a content conflict: defer quietly, no breadcrumb.
			log.Event("review ↻ conflict resolution deferred — spending cap reached, retry after reset (BEH-494) — keeping worktree")
			return false
		}
		log.Event("review ✗ " + res.Reason + " — keeping worktree, nothing pushed")
		comment(fmt.Sprintf(
			"Pre-push auto-rebase onto `main` could not be completed for `%s`: %s This branch passed cold review and the harness gate, but it now needs a manual rebase onto `main`. It is waiting in a worktree. (BEH-581)",
			gitpkg.BranchName(slug), res.Reason,
		))
		return false
	}
	log.Event("review ✓ " + res.Reason)

	// The resolution rewrote the tree, so the earlier host-gate result is stale —
	// re-run it before trusting the push (the ticket's "re-run the gate before
	// pushing"). Push only on a green gate over a still-clean worktree.
	gateOutcome := regate()
	if gateOutcome.ExitCode != 0 || !gitpkg.WorktreeClean(worktreePath) {
		log.Event(fmt.Sprintf("review ✗ post-rebase gate re-run failed (exit %d) — keeping worktree, nothing pushed", gateOutcome.ExitCode))
		comment(fmt.Sprintf(
			"Pre-push conflict was auto-resolved on `%s`, but the post-rebase gate re-run failed (exit %d). The rebased branch is waiting in a worktree for a look. (BEH-581)",
			gitpkg.BranchName(slug), gateOutcome.ExitCode,
		))
		return false
	}
	log.Event("review ✓ post-rebase gate re-run is green — clear to push")
	return true
}

// fixSessionError maps a finished auto-fix session's outcome to the error the
// ci.Driver.Fix callback should return — nil if the session ran cleanly and the
// caller should proceed to the ground-truth (committed-fix) checks. A spending-cap
// abort (BEH-571) is its own retry-after-reset class, so it is surfaced as
// ci.ErrSpendingCapActive and takes precedence over the exit code (a cap abort
// also exits non-zero) — letting WatchAndFix defer rather than count the 1-second
// no-op as a fix-attempt failure.
func fixSessionError(outcome session.Outcome, attempt int) error {
	if outcome.SpendingCapAbort {
		return ci.ErrSpendingCapActive
	}
	if outcome.ExitCode != 0 {
		return fmt.Errorf("auto-fix session %d exited %d", attempt, outcome.ExitCode)
	}
	return nil
}

// rebaseOntoBase is the reactive auto-rebase the CI watch invokes when a green PR
// reads CONFLICTING against base (main moved underneath it after the push). It
// refreshes origin/main — the conflict means main advanced since the pre-push
// rebase, so we must replay onto the truly-latest base — rebases the worktree, and
// on a clean replay force-with-lease re-pushes the rewritten branch. A genuine
// content conflict returns RebaseConflict (branch left untouched) for a human; any
// fetch/push failure is surfaced as an error the watch reports. This is the
// host-side git effect wired into the ci.Driver, kept here so internal/ci needn't
// import internal/git (BEH-570).
func rebaseOntoBase(herdPath, worktreePath, slug string) (ci.RebaseVerdict, error) {
	if err := gitpkg.FetchMain(herdPath); err != nil {
		return ci.RebaseConflict, fmt.Errorf("fetch origin/main before rebase: %w", err)
	}
	if gitpkg.RebaseOntoMain(worktreePath) == gitpkg.RebaseConflict {
		return ci.RebaseConflict, nil
	}
	if err := gitpkg.PushForceWithLease(herdPath, slug); err != nil {
		return ci.RebaseClean, fmt.Errorf("force-with-lease re-push after rebase: %w", err)
	}
	return ci.RebaseClean, nil
}

// createPR opens the pull request from the main checkout with `gh`, which infers
// the origin repo from the checkout. GH_TOKEN stays host-only (ADR-0002) — gh
// reads it from the harness env. Returns the created PR URL (gh prints it to
// stdout).
func createPR(herdPath, slug, title, body string) (string, error) {
	out, err := proc.CombinedOutputInDir(
		prCreateTimeout, herdPath,
		"gh", "pr", "create",
		"--head", gitpkg.BranchName(slug),
		"--base", "main",
		"--title", title,
		"--body", body,
	)
	if err != nil {
		return "", fmt.Errorf("%s: %s", err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}
