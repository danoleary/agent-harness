package stages

import (
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/pr"
	"github.com/danoleary/agent-harness/internal/prompt"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/verify"
)

// reviewSession prefixes this stage's transcript under the ticket's log dir.
const reviewSession = "review"

// prepStep / gateStep label the two throwaway worktree containers this stage
// runs: the pre-session post_create re-provision, and the host-side gate re-run.
// The Runner suffixes them per gate and per retry, so these are the only names
// the stage spells.
const (
	prepStep = "prep"
	gateStep = "gate"
)

// oomMaxAttempts / oomRetryBackoff bound the OOM-kill retry of the throwaway
// install container (BEH-524). A 137 under host memory pressure is transient — the
// same frozen install succeeded in ~4s on a bare retry once memory freed — so a
// couple of attempts with a short *fixed* beat between them (the install's
// footprint recovers in seconds) is enough; a persistent OOM still falls through.
const (
	oomMaxAttempts  = 3
	oomRetryBackoff = 10 * time.Second
)

// gateResult is the outcome of re-running the config-declared named gate list
// host-side (BEH-634). Outcome is the failing gate's session outcome, or — when
// every gate is green — the last gate's. FailedGate names the first gate that
// exited non-zero (empty when all passed); it is what the log surfaces and what
// flows into the CI-fix diagnosis, matching how internal/ci names a failing check.
type gateResult struct {
	Outcome    session.Outcome
	FailedGate string
}

// runGates runs each configured named gate in order via runOne, stopping at the
// first non-zero exit. runOne launches one gate's throwaway container (with its
// own OOM retry) and returns its outcome; runGates is the pure orchestration over
// it, so it is unit-testable with an injected runner. An empty gate list — a
// Consumer misconfiguration guarded upstream by config.validate — yields a green
// zero result. The first red gate's name is surfaced so it can flow into the log
// + CI-fix diagnosis; the gates after it are skipped (the push gate only needs the
// first red to withhold the push).
func runGates(gates []config.Gate, runOne func(config.Gate) session.Outcome) gateResult {
	var last session.Outcome
	for _, g := range gates {
		last = runOne(g)
		if last.ExitCode != 0 {
			return gateResult{Outcome: last, FailedGate: g.Name}
		}
	}
	return gateResult{Outcome: last}
}

// reviewVerdictMaxAttempts bounds the total review-session launches when a session
// exits cleanly (code 0) one turn short of its verdict over an already-verified,
// clean, gate-green worktree (BEH-624). The initial session plus one in-stage
// re-launch — the diff is byte-identical and the gate is already green, so the
// re-launch just re-reads the same diff and reaches its verdict; a persistent
// no-verdict still falls through to fail-closed (the worktree is kept either way).
const reviewVerdictMaxAttempts = 2

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
// Every host-side effect goes through h, so the whole body is reachable from a
// test with hostio.NewFake().
func Review(h hostio.Host, cfg config.Config, log *runlog.Logger, args Args) Result {
	slug := strings.ToLower(args.Identifier)
	worktreePath := h.WorktreePath(slug)

	dry := ""
	if args.DryRun {
		dry = " (dry-run)"
	}
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: args.Identifier, Stage: "review", Message: fmt.Sprintf("run %s — review %s%s", h.RunID(), args.Identifier, dry)})

	client, err := h.Tracker()
	if err != nil {
		return Result{Err: err}
	}

	// The ticket is the intent to review against (review reconstructs intent from
	// the branch/issue/diff). Review does NOT claim or move it — implementation
	// already did, and the harness owns tracker state (no transition here).
	t, err := client.FetchTicket(args.Identifier)
	if err != nil {
		return Result{Err: err}
	}
	log.Event(fmt.Sprintf("fetched %s — %s", t.Identifier, t.Title))

	// Review emits no findings (retrospective owns them) → no findings mount.
	p := prompt.BuildReview(t, slug, worktreePath, cfg.BranchPrefix, cfg.Prompts.Review)
	reviewRun := hostio.AgentRun{Label: reviewSession, Prompt: p, Cap: cfg.ReviewTimeout}
	prepRun := hostio.ShellRun{
		Label: prepStep, Command: cfg.PostCreate, WorktreePath: worktreePath, Cap: cfg.ReviewTimeout,
		Retry: hostio.Retry{
			MaxAttempts: oomMaxAttempts,
			Backoff:     session.ConstantBackoff(oomRetryBackoff),
			Notify: func(attempt int, waited time.Duration) {
				log.Event(fmt.Sprintf(
					"review ↻ worktree prep OOM-killed (exit 137) — retry %d/%d after %s (BEH-524)",
					attempt-1, oomMaxAttempts-1, waited,
				))
			},
		},
	}

	if args.DryRun {
		log.Event("dry-run — not launching the install, review, or gate containers")
		// One gate container per config-declared named gate, run in order (BEH-634).
		gatePreviews := make([]string, 0, len(cfg.Gates))
		for _, g := range cfg.Gates {
			preview := h.ShellPreview(hostio.ShellRun{Label: gateStep + "-" + g.Name, Command: g.Command, WorktreePath: worktreePath})
			gatePreviews = append(gatePreviews, fmt.Sprintf("# gate %q\ndocker %s", g.Name, strings.Join(preview, " ")))
		}
		prepPreview := "(none — the Consumer declares no post_create)"
		if cfg.PostCreate != "" {
			prepPreview = "docker " + strings.Join(h.ShellPreview(prepRun), " ")
		}
		fmt.Printf(
			"\n--- prompt ---\n%s\n\n--- prep docker command ---\n%s\n\n--- review docker command ---\ndocker %s\n\n--- gate docker commands ---\n%s\n",
			p, prepPreview, strings.Join(h.AgentPreview(reviewRun), " "), strings.Join(gatePreviews, "\n\n"),
		)
		return Result{OK: true}
	}

	// Precondition: the implementation slice must have left a worktree. Without it
	// there is nothing to review (run `implementation <ticket>` first).
	if !h.WorktreeExists(slug) {
		log.Event(fmt.Sprintf("review ✗ no worktree at %s — run `implementation %s` first", worktreePath, args.Identifier))
		return Result{OK: false}
	}

	// Fail fast if Docker can't run the container before we burn the session.
	if err := h.Preflight(); err != nil {
		return Result{Err: err}
	}

	// Heads-up if GH_TOKEN can't read check runs: the post-PR CI-watch needs a
	// classic repo-scoped PAT (fine-grained PATs lack the Checks permission). Not
	// fatal — the PR still ships and the watch degrades gracefully (BEH-476); this
	// just warns at the start instead of only surfacing after the PR is open.
	if ok, detail := h.ChecksReadable(); !ok {
		log.Event("review … warning: " + detail)
		fmt.Fprintln(os.Stderr, detail)
	}

	// --- prep: re-provision the worktree before the cold session (BEH-490) ---
	// The handoff strip (BEH-412) can leave the worktree without its installed
	// dependencies; run the Consumer's post_create up front so the session opens
	// onto a ready worktree instead of paying the install mid-gate. Warn-only: a
	// review skill that finds missing dependencies installs them in-session, so a
	// transient prep failure degrades rather than aborting.
	//
	// Retry on an OOM-kill (exit 137) before that fallback (BEH-524): under host
	// memory pressure this install gets SIGKILLed, and degrading to warn-and-continue
	// dumped the ~10-min recovery install into the capped review session, starving it
	// of time to actually review. The OOM is transient (it succeeded in ~4s on a bare
	// retry once memory freed), so a short backoff-and-retry recovers it up front.
	//
	// A Consumer with no post_create declared has nothing to re-provision, so the
	// container is skipped entirely rather than run with an empty command (BEH-641).
	if cfg.PostCreate == "" {
		log.Event("review — no post_create declared; skipping worktree prep")
	} else {
		log.Event("prepping worktree (post_create) before the review session")
		prep := h.Shell(prepRun)
		if prep.ExitCode != 0 {
			log.Event(fmt.Sprintf(
				"review … warning: worktree prep exited %d after %d attempt(s) — session will provision in-session if needed",
				prep.ExitCode, prep.Attempts,
			))
		}
	}

	// --- review session (cold /review-worktree; fixes committed locally) ---
	// runReviewSession launches one cold /review-worktree session over the worktree.
	// The BEH-624 in-stage re-launch below gets its own label, so the Runner mints it
	// a distinct --name + transcript and it cannot collide with the first attempt.
	runReviewSession := func(attempt int) session.Outcome {
		run := reviewRun
		if attempt > 1 {
			run.Label = fmt.Sprintf("%s-retry%d", reviewSession, attempt)
		}
		log.Structured(loopstream.Record{Kind: loopstream.KindSandboxLaunch, Ticket: args.Identifier, Stage: "review", Message: fmt.Sprintf("launching review session (attempt %d/%d, cap %d min active)", attempt, reviewVerdictMaxAttempts, int(cfg.ReviewTimeout.Minutes()))})
		res := h.Agent(run)
		log.Event(fmt.Sprintf(
			"review session exited (code %d) — transcript at logs/%s/%s", res.ExitCode, args.Identifier, res.Transcript,
		))
		// A spending-cap abort (BEH-494) killed the review session before it could
		// review anything — name that distinct retry-after-reset class so a log reader
		// isn't misled by the host-side gate result below (which still runs against the
		// committed TDD handoff regardless of whether the review agent did any work).
		if res.SpendingCapAbort {
			log.Event("review ↻ session aborted before running — spending cap reached, retry after reset (BEH-494)")
		}
		return res.Outcome
	}

	// runHostGate re-runs the config-declared named gate list host-side in throwaway
	// containers — one per gate, in order, stopping at the first red (BEH-634) — and
	// reports the first failing gate by name (gateResult.FailedGate). Each gate runs
	// under its own OOM-retry (BEH-530). It is keyed by a label prefix so the whole
	// gate run can happen more than once per stage (the pre-push conflict path
	// re-gates the resolved tree — BEH-581; the BEH-624 review re-launch re-gates the
	// possibly-rewritten tree).
	//
	// The gate's exit code is the ONLY thing that authorises a push — never the
	// agent's report. A 137 OOM-kill is environmental, never the diff: the gate's own
	// install (and even a typecheck) can be SIGKILLed under memory pressure, which
	// would flip a genuinely green branch red and push nothing. A real gate failure
	// returns a non-137 code and is final on the first attempt.
	runHostGate := func(labelPrefix string) gateResult {
		// A docs-only diff (root markdown / docs/**) feeds none of the gates, so the heavy
		// host re-run validates nothing a prose edit could break — skip it and treat the
		// gate as green (BEH-687). Read fresh here, not once up front: an earlier
		// review/conflict session may have committed a code edit, and BranchDocsOnly over
		// the current tree (which `git diff origin/main` reads including uncommitted work)
		// reflects that — fail-safe to running the full gate on any doubt.
		if h.BranchDocsOnly(slug) {
			log.Event("review · host gate skipped — diff touches only docs/prose paths no gate depends on (BEH-687)")
			return gateResult{}
		}
		return runGates(cfg.Gates, func(g config.Gate) session.Outcome {
			log.Event(fmt.Sprintf("review · gate %q: %s", g.Name, g.Command))
			gateBackoff := session.ExponentialBackoff(gateOOMBackoffBase, gateOOMBackoffCap)
			return h.Shell(hostio.ShellRun{
				Label:        labelPrefix + "-" + g.Name,
				Command:      g.Command,
				WorktreePath: worktreePath,
				Cap:          cfg.ReviewTimeout,
				Retry: hostio.Retry{
					MaxAttempts: gateOOMMaxAttempts,
					Backoff:     gateBackoff,
					Notify: func(attempt int, waited time.Duration) {
						log.Event(fmt.Sprintf(
							"review ↻ host-side gate %q OOM-killed (exit 137) — retry %d/%d after %s (BEH-530)",
							g.Name, attempt-1, gateOOMMaxAttempts-1, waited,
						))
					},
				},
			}).Outcome
		})
	}

	reviewOutcome := runReviewSession(1)

	// --- ground truth + push gate (harness, host-side) ---
	// Refresh origin/main so the commit range + PR base are current.
	if err := h.FetchMain(); err != nil {
		log.Event("review … warning: could not fetch origin/main: " + err.Error())
	}

	log.Event(fmt.Sprintf("re-running gates host-side (cap %d min)", int(cfg.ReviewTimeout.Minutes())))
	gateRes := runHostGate(gateStep)
	gateExit := gateRes.Outcome.ExitCode

	// The gate runs against the worktree's working tree (committed + uncommitted),
	// but Push ships only the committed tip — so the push is authorised only when
	// the worktree is also clean, guaranteeing what shipped is exactly what the gate
	// validated (never the agent's say-so, and never an unverified working tree).
	clean := h.WorktreeClean(slug)

	// Did the qualitative seven-lens review actually run? The host-side gate re-run
	// above authorises the push, but a green gate only proves the diff compiles — it
	// is NOT a review. A session that ends before the "## Review:" verdict (an OOM
	// mid-gate, or just stopping a turn short) leaves the diff unreviewed.
	completeness := verify.ReviewQualitative(reviewOutcome.ExitCode, reviewOutcome.ReviewVerdictEmitted)

	// BEH-624: the cheapest incompleteness class to recover. A review that exits
	// cleanly (code 0) one turn short of its verdict — over a clean worktree whose
	// committed tip the host gate already proved green — has a byte-identical, fully
	// verified diff; it just needs its last few turns to print the verdict. Re-launch
	// it in-stage (bounded) rather than discard the verified diff for a whole-pipeline
	// re-review, mirroring the OOM / conflict-resolution re-launch pattern. Ground
	// truth stays the harness gate + the emitted verdict, so nothing ships on
	// self-report; verify.ReviewVerdictRetry gates eligibility (no retry on an OOM, a
	// cap abort, a dirty tree, or a red gate). Re-gate after each re-launch: the review
	// may have committed fixes, so the prior gate would be stale.
	for attempt := 2; attempt <= reviewVerdictMaxAttempts && verify.ReviewVerdictRetry(verify.ReviewRetryInputs{
		VerdictEmitted:   reviewOutcome.ReviewVerdictEmitted,
		ExitCode:         reviewOutcome.ExitCode,
		SpendingCapAbort: reviewOutcome.SpendingCapAbort,
		WorktreeClean:    clean,
		GatesGreen:       gateExit == 0,
	}).Retry; attempt++ {
		log.Event(fmt.Sprintf(
			"review ↻ session exited cleanly (code 0) one turn short of its verdict over a clean, gate-green worktree — re-launching review %d/%d (BEH-624)",
			attempt-1, reviewVerdictMaxAttempts-1,
		))
		reviewOutcome = runReviewSession(attempt)
		gateRes = runHostGate(fmt.Sprintf("%s-retry%d", gateStep, attempt))
		gateExit = gateRes.Outcome.ExitCode
		clean = h.WorktreeClean(slug)
		completeness = verify.ReviewQualitative(reviewOutcome.ExitCode, reviewOutcome.ReviewVerdictEmitted)
	}

	// Flag the final review completeness so the run log carries the signal rather than
	// a green gate masquerading as a full review pass (BEH-525, from the BEH-499 OOM).
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
	// A clean worktree whose committed tip makes zero net change against origin/main is
	// the BEH-603 recommend-close disposition: nothing to ship, so the ticket should be
	// closed as a duplicate/superseded rather than opened as an empty-commit PR (the PR
	// #642 mistake). Read host-side; checked inside verify.Review only when the tree is
	// clean (a dirty tree's "empty" committed diff may hide uncommitted work).
	emptyDiff := h.BranchDiffEmpty(slug)

	// comment posts a best-effort tracker breadcrumb so a gate-green, reviewed branch
	// that can't ship autonomously surfaces on the ticket instead of sitting silent in
	// a worktree (BEH-581 conflict path). A failure to comment is logged, never fatal.
	comment := func(body string) {
		if err := client.AddComment(args.Identifier, body); err != nil {
			log.Event("review … warning: could not post tracker breadcrumb: " + err.Error())
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
			if cErr := h.Checkpoint(slug, args.Identifier, reviewSession); cErr != nil {
				log.Event("⚠ review session left uncommitted edits and the recovery checkpoint commit failed (" + cErr.Error() + ") — recover them manually at " + worktreePath)
			} else {
				log.Event("✓ harness recovery checkpoint committed on " + h.BranchName(slug) + " — the review session's in-progress edits are preserved (unverified: a resumed review will see them, finish or re-run before opening a PR)")
			}
		}
		// Red/crash/dirty → keep the worktree (recoverable artifact), do not push.
		// A spending-cap abort that killed the review before it could ship is surfaced
		// so the loop classifies this as retry-after-reset (breaker-neutral), not a
		// ship failure that should advance the circuit breaker. Name the failing gate
		// (BEH-634) so a red gate is diagnosable by name, matching how internal/ci names
		// a failing check — the empty-name case (a non-gate failure) omits the clause.
		gateClause := ""
		if gateRes.FailedGate != "" {
			gateClause = fmt.Sprintf(" [gate %q]", gateRes.FailedGate)
		}
		log.Event(fmt.Sprintf("review ✗ %s (gate exit %d)%s — keeping worktree, nothing pushed", result.Reason, gateExit, gateClause))
		return Result{OK: false, SpendingCapAbort: reviewOutcome.SpendingCapAbort, SpendingCapResetTime: reviewOutcome.SpendingCapResetTime}
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
	if err := h.FetchMain(); err != nil {
		log.Event("review … warning: could not re-fetch origin/main before rebase: " + err.Error())
	}

	// Rebase onto origin/main before pushing so a sibling PR that merged during this long
	// pipeline can't strand the branch on a stale base — the merge-conflict dead-end
	// BEH-570 hit, where the conflict only surfaced post-PR in the CI watch and was left
	// as a manual step. The worktree is clean here (verified just above), so the replay's
	// reset --hard can't discard uncommitted work (BEH-618). A clean replay moves the tip
	// onto the current base. A genuine content conflict no longer dead-ends (BEH-581):
	// rather than strand ~30 min of reviewed, gate-green work, the harness launches a
	// bounded sandboxed conflict-resolution session over the worktree (mirroring the
	// post-PR ciFixRunner), re-runs the host gate on the resolved tree, and only then
	// pushes. resolvePrePushConflict returns false when it could not land a clean,
	// re-gated rebase — keeping the worktree and leaving a tracker breadcrumb so the
	// work surfaces autonomously.
	if h.Rebase(slug) == hostio.RebaseConflict {
		// A disjoint history (no common ancestor with origin/main) is NOT a content
		// conflict (BEH-597): the rebase collided trying to replay every one of the
		// branch's disjoint commits, so the BEH-581 conflict-resolution session is the
		// wrong tool — it would burn a sandbox re-discovering the empty merge-base. The
		// tdd gate now fails this upstream (verify.Tdd's DisjointHistory check), so a
		// disjoint branch should never reach here; this is the defence-in-depth backstop
		// for a resumed/standalone review. Keep the worktree for manual recovery
		// (`git reset --hard origin/main` + cherry-pick the handoff commits) and surface it.
		if h.IsDisjoint(slug) {
			h.AbortRebase(slug)
			log.Event("review ✗ disjoint branch history — no common ancestor with origin/main; not a content conflict, needs manual recovery (BEH-597) — keeping worktree, nothing pushed")
			comment(fmt.Sprintf(
				"Branch `%s` has a disjoint history from `main` (no common ancestor / empty merge-base), so it cannot be rebased or merged as-is. This is not a content conflict — the handoff commits need to be re-applied onto current `main` (e.g. `git reset --hard origin/main` then cherry-pick them). The branch passed cold review and the harness gate; it is waiting in a worktree. (BEH-597)",
				h.BranchName(slug),
			))
			return Result{OK: false}
		}
		resolved := resolvePrePushConflict(h, cfg, log, slug, t, comment,
			func() session.Outcome { return runHostGate(gateStep + "-postrebase").Outcome })
		if !resolved {
			return Result{OK: false}
		}
	}
	log.Event("rebased " + h.BranchName(slug) + " onto origin/main")

	// Re-check the branch's emptiness AFTER the rebase, before the push (BEH-680). The
	// pre-rebase BEH-603 EmptyDiff gate above ran on the stale tree; the rebase can
	// collapse the branch to zero net change — a sibling PR landed the same fix during
	// the multi-minute gate, or the BEH-581 conflict-resolution session skipped a
	// now-empty commit — leaving it identical to origin/main. Pushing then and running
	// `gh pr create` hard-fails with "No commits between main and feat/…" (a wasted push
	// and a misleading "open the PR manually" hint for a branch with nothing to open).
	// Route to the same recommend-close disposition instead: keep the worktree, push
	// nothing, and let the loop flag the ticket for a human to close as superseded.
	if postRebase := verify.PostRebasePush(h.BranchDiffEmpty(slug)); postRebase.RecommendClose {
		log.Event("review ⊘ " + postRebase.Reason + " — keeping worktree, nothing pushed; recommending close (BEH-680)")
		return Result{OK: false, RecommendClose: true}
	}

	// force-with-lease, not a plain push: the rebase above is unconditional, so on any
	// branch already on the remote it has just rewritten every SHA and a plain push is
	// rejected non-fast-forward. That is not a rare state — a run whose `gh pr create`
	// timed out after a successful push leaves exactly it, and because the ticket then
	// has no PR the loop re-selects it, rebases again, and is rejected again, forever.
	// The lease keeps the force safe: it refuses to clobber remote commits the harness
	// hasn't observed, and the harness owns this branch outright.
	if err := h.PushForceWithLease(slug); err != nil {
		log.Event("review ✗ push failed: " + err.Error() + " — keeping worktree")
		return Result{OK: false}
	}
	log.Event("pushed " + h.BranchName(slug) + " to origin")

	url, err := h.CreatePR(slug, pr.BuildTitle(t), pr.BuildBody(t, h.CommitSubjects(slug)))
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
	// diagnose+fix happens in the sandbox, which is why Fix is the only thing the
	// stage supplies.
	log.Event("watching CI for " + h.BranchName(slug) + " …")
	ciResult := h.WatchCI(hostio.CIWatch{Slug: slug, Fix: ciFixRunner(h, cfg, log, slug, t)})
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
			fmt.Fprintf(os.Stderr, "\nFailing CI checks for %s:\n%s", h.BranchName(slug), s)
		}
		// CI red after the auto-fix budget still leaves a reviewable PR for a human to
		// take over — NOT a ship failure, so the breaker must not count it.
		return Result{OK: false, ReachedPushedPR: true}
	}
	log.Event("review ✓ " + ciResult.Reason)

	return Result{OK: true, ReachedPushedPR: true}
}

// ciFixRunner returns the CI watch's fix callback: it launches a sandboxed Claude
// session over the existing worktree, steered by BuildCIFix with the fetched
// failing logs, then enforces ground truth — a non-zero session exit or a worktree
// left dirty (the agent didn't commit) is a failure, so the harness never pushes
// an unverified or self-reported-only fix. Each attempt gets a unique label, from
// which the Runner mints the container name and transcript.
func ciFixRunner(h hostio.Host, cfg config.Config, log *runlog.Logger, slug string, t ticket.Ticket) func(string, bool) error {
	attempt := 0
	return func(ciLogs string, logAvailable bool) error {
		attempt++
		// Capture the tip before the session so we can tell afterwards whether the
		// agent actually committed a fix or correctly concluded there was nothing to
		// fix (BEH-561). A read failure leaves headBefore empty, which degrades to
		// "treat any HEAD as a real commit" — never a spurious empty commit.
		headBefore, _ := h.HeadSHA(slug)
		log.Event(fmt.Sprintf("CI red — launching auto-fix session %d (cap %d min active)", attempt, int(cfg.ReviewTimeout.Minutes())))
		res := h.Agent(hostio.AgentRun{
			Label:  fmt.Sprintf("cifix-%d", attempt),
			Prompt: prompt.BuildCIFix(t, slug, cfg.BranchPrefix, h.WorktreePath(slug), ciLogs, logAvailable),
			Cap:    cfg.ReviewTimeout,
		})
		if err := fixSessionError(res.Outcome, attempt); err != nil {
			return err
		}
		// Ground truth, never the agent's say-so: the fix must be committed (clean
		// worktree) or the harness has nothing trustworthy to push.
		if !h.WorktreeClean(slug) {
			return fmt.Errorf("auto-fix session %d left uncommitted changes — not pushing", attempt)
		}
		// The agent may have correctly concluded the red is not a code defect (a
		// cancelled/superseded/flaky run) and committed nothing. Don't force a
		// speculative diff: add an empty commit so the re-push gives CI a fresh HEAD
		// to re-run against; a no-op if a real fix moved HEAD (BEH-561).
		if err := h.EnsureCIRerunCommit(slug, headBefore); err != nil {
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
// keeps the worktree and leaves a tracker breadcrumb so the work surfaces (a
// spending-cap abort defers quietly — it retries after reset, not a conflict).
func resolvePrePushConflict(
	h hostio.Host, cfg config.Config, log *runlog.Logger,
	slug string, t ticket.Ticket, comment func(string),
	regate func() session.Outcome,
) bool {
	log.Event("review ↻ pre-push rebase hit a content conflict — launching a sandboxed conflict-resolution session (BEH-581)")
	log.Event(fmt.Sprintf("launching conflict-resolution session (cap %d min active)", int(cfg.ReviewTimeout.Minutes())))
	res := h.Agent(hostio.AgentRun{
		Label:  "rebasefix",
		Prompt: prompt.BuildRebaseFix(t, slug, cfg.BranchPrefix, h.WorktreePath(slug)),
		Cap:    cfg.ReviewTimeout,
	})
	log.Event(fmt.Sprintf("conflict-resolution session exited (code %d)", res.ExitCode))

	// Ground truth over the worktree, never the agent's report: did the session
	// actually rebase onto origin/main and leave a clean tree?
	verdict := verify.RebaseResolution(verify.RebaseResolutionOutcome{
		SessionExit:      res.ExitCode,
		SpendingCapAbort: res.SpendingCapAbort,
		WorktreeClean:    h.WorktreeClean(slug),
		Rebased:          h.IsRebased(slug),
	})
	if !verdict.OK {
		// Restore a clean, on-branch worktree for the next resume (a failed session may
		// have left it mid-rebase). Best-effort: a no-op when none is in progress.
		h.AbortRebase(slug)
		if verdict.SpendingCapAbort {
			// Retry-after-reset, not a content conflict: defer quietly, no breadcrumb.
			log.Event("review ↻ conflict resolution deferred — spending cap reached, retry after reset (BEH-494) — keeping worktree")
			return false
		}
		log.Event("review ✗ " + verdict.Reason + " — keeping worktree, nothing pushed")
		comment(fmt.Sprintf(
			"Pre-push auto-rebase onto `main` could not be completed for `%s`: %s This branch passed cold review and the harness gate, but it now needs a manual rebase onto `main`. It is waiting in a worktree. (BEH-581)",
			h.BranchName(slug), verdict.Reason,
		))
		return false
	}
	log.Event("review ✓ " + verdict.Reason)

	// The resolution rewrote the tree, so the earlier host-gate result is stale —
	// re-run it before trusting the push (the ticket's "re-run the gate before
	// pushing"). Push only on a green gate over a still-clean worktree.
	gateOutcome := regate()
	if gateOutcome.ExitCode != 0 || !h.WorktreeClean(slug) {
		log.Event(fmt.Sprintf("review ✗ post-rebase gate re-run failed (exit %d) — keeping worktree, nothing pushed", gateOutcome.ExitCode))
		comment(fmt.Sprintf(
			"Pre-push conflict was auto-resolved on `%s`, but the post-rebase gate re-run failed (exit %d). The rebased branch is waiting in a worktree for a look. (BEH-581)",
			h.BranchName(slug), gateOutcome.ExitCode,
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
