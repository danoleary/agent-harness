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
// install + gate containers (BEH-524). A 137 under host memory pressure is
// transient — the same frozen install succeeded in ~4s on a bare retry once
// memory freed — so a couple of attempts with a short beat between them (to let
// the starved Docker VM recover) is enough; a persistent OOM still falls through.
const (
	oomMaxAttempts  = 3
	oomRetryBackoff = 10 * time.Second
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
	log.Event(fmt.Sprintf("run %s — review %s%s", runID, args.Identifier, dry))

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
	if err := sandbox.Preflight(cfg.Image, filepath.Join(cfg.HerdPath, "agent-harness"), sandbox.ProbeRunner, sandbox.BuildImage); err != nil {
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
	installTranscript := runlog.TranscriptName("install", runID)
	log.Event("prepping worktree (pnpm install --frozen-lockfile) before the review session")
	installOutcome, installAttempts := session.RetryOnOOMKill(oomMaxAttempts, oomRetryBackoff, time.Sleep, func(attempt int) session.Outcome {
		name, transcript := installName, installTranscript
		if attempt > 1 {
			name = fmt.Sprintf("%s-retry%d", installName, attempt)
			transcript = runlog.TranscriptName(fmt.Sprintf("install-retry%d", attempt), runID)
			log.Event(fmt.Sprintf(
				"review ↻ prep install OOM-killed (exit 137) — retry %d/%d after %s (BEH-524)",
				attempt-1, oomMaxAttempts-1, oomRetryBackoff,
			))
		}
		return session.Run(buildInstallArgs(name), session.Options{
			ContainerName:  name,
			TranscriptFile: transcript,
			Timeout:        cfg.ReviewTimeout,
			Verbose:        args.Verbose,
			Log:            log,
		})
	})
	if installOutcome.ExitCode != 0 {
		log.Event(fmt.Sprintf(
			"review … warning: worktree prep install exited %d after %d attempt(s) — session will install in-session if needed",
			installOutcome.ExitCode, installAttempts,
		))
	}

	// --- review session (cold /review-worktree; fixes committed locally) ---
	transcriptFile := runlog.TranscriptName(reviewSession, runID)
	log.Event(fmt.Sprintf("launching review session (cap %d min)", int(cfg.ReviewTimeout.Minutes())))
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
	gateTranscript := runlog.GateTranscriptName(runID)
	log.Event(fmt.Sprintf("re-running gates host-side (cap %d min)", int(cfg.ReviewTimeout.Minutes())))
	gateOutcome, _ := session.RetryOnOOMKill(oomMaxAttempts, oomRetryBackoff, time.Sleep, func(attempt int) session.Outcome {
		name, transcript := gateName, gateTranscript
		if attempt > 1 {
			name = fmt.Sprintf("%s-retry%d", gateName, attempt)
			transcript = runlog.GateTranscriptName(fmt.Sprintf("%s-retry%d", runID, attempt))
			log.Event(fmt.Sprintf(
				"review ↻ host-side gate OOM-killed (exit 137) — retry %d/%d after %s (BEH-524)",
				attempt-1, oomMaxAttempts-1, oomRetryBackoff,
			))
		}
		return session.Run(buildGateArgs(name), session.Options{
			ContainerName:  name,
			TranscriptFile: transcript,
			Timeout:        cfg.ReviewTimeout,
			IdleTimeout:    cfg.SessionIdleTimeout,
			Verbose:        args.Verbose,
			Log:            log,
		})
	})
	gateExit := gateOutcome.ExitCode

	// The gate runs against the worktree's working tree (committed + uncommitted),
	// but Push ships only the committed tip — so the push is authorised only when
	// the worktree is also clean, guaranteeing what shipped is exactly what the gate
	// validated (never the agent's say-so, and never an unverified working tree).
	clean := gitpkg.WorktreeClean(worktreePath)
	result := verify.Review(verify.ReviewOutcome{GatesGreen: gateExit == 0, WorktreeClean: clean})
	if !result.OK {
		// Red/crash/dirty → keep the worktree (recoverable artifact), do not push.
		log.Event(fmt.Sprintf("review ✗ %s (gate exit %d) — keeping worktree, nothing pushed", result.Reason, gateExit))
		return Result{OK: false}
	}
	log.Event("review ✓ " + result.Reason)

	// --- push + PR (only reached on a green harness gate) ---
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
	log.Event("review ✓ PR opened: " + url)

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
	}
	driver := ci.NewGhDriver(
		cfg.HerdPath, gitpkg.BranchName(slug), ciCfg, ciGhTimeout,
		ciFixRunner(cfg, args, slug, worktreePath, runID, t, log),
		func() error { return gitpkg.Push(cfg.HerdPath, slug) },
	)
	log.Event("watching CI for " + gitpkg.BranchName(slug) + " …")
	ciResult := ci.WatchAndFix(driver, ciCfg, time.Now)
	if !ciResult.OK {
		log.Event("review ✗ CI did not go green: " + ciResult.Reason + " — keeping PR + worktree")
		if s := ci.Summarize(ciResult.Failing); s != "" {
			fmt.Fprintf(os.Stderr, "\nFailing CI checks for %s:\n%s", gitpkg.BranchName(slug), s)
		}
		return Result{OK: false}
	}
	log.Event("review ✓ " + ciResult.Reason)

	return Result{OK: true}
}

// ciFixRunner returns the ci.GhDriver's fix callback: it launches a sandboxed
// Claude session over the existing worktree, steered by BuildCIFix with the
// fetched failing logs, then enforces ground truth — a non-zero session exit or
// a worktree left dirty (the agent didn't commit) is a failure, so the harness
// never pushes an unverified or self-reported-only fix. Each attempt gets a
// unique container name + transcript.
func ciFixRunner(cfg config.Config, args Args, slug, worktreePath, runID string, t ticket.Ticket, log *runlog.Logger) func(string) error {
	attempt := 0
	return func(ciLogs string) error {
		attempt++
		fixPrompt := prompt.BuildCIFix(t, slug, worktreePath, ciLogs)
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
		if outcome.ExitCode != 0 {
			return fmt.Errorf("auto-fix session %d exited %d", attempt, outcome.ExitCode)
		}
		// Ground truth, never the agent's say-so: the fix must be committed (clean
		// worktree) or the harness has nothing trustworthy to push.
		if !gitpkg.WorktreeClean(worktreePath) {
			return fmt.Errorf("auto-fix session %d left uncommitted changes — not pushing", attempt)
		}
		return nil
	}
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
