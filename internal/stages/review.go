package stages

import (
	"fmt"
	"os"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/fix"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/ship"
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

// reviewVerdictMaxAttempts bounds the total review-session launches when a session
// exits cleanly (code 0) one turn short of its verdict over an already-verified,
// clean, gate-green worktree (BEH-624). The initial session plus one in-stage
// re-launch — the diff is byte-identical and the gate is already green, so the
// re-launch just re-reads the same diff and reaches its verdict; a persistent
// no-verdict still falls through to fail-closed (the worktree is kept either way).
const reviewVerdictMaxAttempts = 2

// Review runs the second stage: over the worktree implementation left behind, run
// a *cold* /review-worktree session (fixes committed locally only), then —
// host-side, holding GH_TOKEN — independently re-run the quality gates in a
// throwaway container and, only if they pass, push the branch and open the PR.
// Ground truth is the harness's own gate run, never the agent's self-report.
// Every host-side effect goes through h, so the whole body is reachable from a
// test with hostio.NewFake over a temp repo.
func Review(h hostio.Host, cfg config.Config, log *runlog.Logger, args Args) Result {
	wt := h.Worktree()
	worktreePath := wt.Path()
	f := newFrame(h, log, reviewSession, args.Identifier)
	f.start(args.DryRun)

	// The ticket is the intent to review against (review reconstructs intent from
	// the branch/issue/diff).
	client, t, err := f.fetch()
	if err != nil {
		return Result{Err: err}
	}

	subject := NewSubject(h, args.Identifier, log.Dir, t)
	subject.Base, _ = wt.BaseSHA()
	// A re-run whose branch gained no commit since the last review is handed that
	// review's verdict and the range it covered, so the session re-emits it after the
	// gate rather than re-derive it and stack a churn commit to show for it (#44).
	headAtStart, _ := wt.HeadSHA()
	prior := readPriorReview(log.Dir, headAtStart)
	if prior != nil {
		log.Event(fmt.Sprintf("review — branch unchanged since the last review at %s (%s); handing the session that verdict (#44)", prior.Head, prior.Disposition))
	}
	reviewSessionRun := reviewRun(cfg, subject, prior)
	prep := prepRun(cfg, worktreePath)
	prep.Retry = hostio.Retry{
		MaxAttempts: oomMaxAttempts,
		Backoff:     session.ConstantBackoff(oomRetryBackoff),
		Notify: func(attempt int, waited time.Duration) {
			log.Event(fmt.Sprintf(
				"review ↻ worktree prep OOM-killed (exit 137) — retry %d/%d after %s (BEH-524)",
				attempt-1, oomMaxAttempts-1, waited,
			))
		},
	}

	if args.DryRun {
		log.Event("dry-run — not launching the install, review, or gate containers")
		fmt.Print(ReviewPlan(cfg, subject, prior).Render(h))
		return Result{OK: true}
	}

	// Precondition: the implementation slice must have left a worktree. Without it
	// there is nothing to review (run `implementation <ticket>` first).
	if !wt.Exists() {
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
	// transient prep failure degrades rather than aborting — unless the shell could
	// not run the command at all (exit 127/126), which aborts before the session (#46).
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
		res := h.Shell(prep)
		// A prep the shell could not run at all is the environment, not a flaky
		// install: the session's own container has a different PATH, so it cannot fix
		// it, and the host gate would hit the same missing command. Stop before the
		// session launches (#46). The worktree is untouched and kept.
		if missing, ok := verify.CommandUnrunnable(res.Outcome); ok {
			log.Event("review ✗ worktree prep: " + unrunnableReport("post_create", missing, res.Outcome) + "; no review session launched — keeping worktree")
			return Result{OK: false}
		}
		if res.ExitCode != 0 {
			log.Event(fmt.Sprintf(
				"review … warning: worktree prep exited %d after %d attempt(s) — session will provision in-session if needed",
				res.ExitCode, res.Attempts,
			))
		}
	}

	// --- review session (cold /review-worktree; fixes committed locally) ---
	// runReviewSession launches one cold /review-worktree session over the worktree.
	// The BEH-624 in-stage re-launch below gets its own label, so the Runner mints it
	// a distinct --name + transcript and it cannot collide with the first attempt.
	runReviewSession := func(attempt int) session.Outcome {
		run := reviewSessionRun
		if attempt > 1 {
			run.Label = fmt.Sprintf("%s-retry%d", reviewSession, attempt)
		}
		res := f.launch(run, fmt.Sprintf("attempt %d/%d", attempt, reviewVerdictMaxAttempts))
		// A spending-cap abort (BEH-494) killed the review session before it could
		// review anything — name that distinct retry-after-reset class so a log reader
		// isn't misled by the host-side gate result below (which still runs against the
		// committed TDD handoff regardless of whether the review agent did any work).
		if res.SpendingCapAbort {
			log.Event("review ↻ session aborted before running — spending cap reached, retry after reset (BEH-494)")
		}
		return res.Outcome
	}

	// The host gate re-runs the config-declared Gates in throwaway containers and is
	// the ONLY thing that authorises a push (internal/fix). It is keyed by a label
	// prefix so it can run more than once per stage: the pre-push conflict path
	// re-gates the resolved tree (BEH-581) and the BEH-624 review re-launch re-gates
	// the possibly-rewritten tree.
	fixes := fix.Env{Host: h, Cfg: cfg, Log: log, Who: reviewSession}
	target := fix.Target{Slug: subject.Slug, Ticket: t}

	reviewOutcome := runReviewSession(1)
	if res, ok := f.interrupted(reviewOutcome); ok {
		return res
	}

	// --- ground truth + push gate (harness, host-side) ---
	// Refresh origin/main so the commit range + PR base are current.
	if err := h.FetchMain(); err != nil {
		log.Event("review … warning: could not fetch origin/main: " + err.Error())
	}

	log.Event(fmt.Sprintf("re-running gates host-side (cap %d min)", int(cfg.ReviewTimeout.Minutes())))
	gateRes := fixes.Gates(gateStep)
	gateExit := gateRes.Outcome.ExitCode

	// verify.Review is the whole push gate: it re-reads the worktree's git state for
	// itself (a clean tree, a non-empty diff against origin/main) and folds in the
	// gate's exit code and what the session's stream reported, so the four boolean
	// expressions that used to fill a parameter struct here live next to the truth
	// table they feed. It also carries the two verdicts that ride along with the
	// push decision — whether the qualitative review completed, and whether the one
	// recoverable incompleteness class is worth another turn.
	result := verify.Review(wt, gateRes.Outcome, reviewOutcome)

	// BEH-624: the cheapest incompleteness class to recover. A review that exits
	// cleanly (code 0) one turn short of its verdict — over a clean worktree whose
	// committed tip the host gate already proved green — has a byte-identical, fully
	// verified diff; it just needs its last few turns to print the verdict. Re-launch
	// it in-stage (bounded) rather than discard the verified diff for a whole-pipeline
	// re-review, mirroring the OOM / conflict-resolution re-launch pattern. Ground
	// truth stays the harness gate + the emitted verdict, so nothing ships on
	// self-report; Result.Retry gates eligibility (no retry on an OOM, a cap abort, a
	// dirty tree, or a red gate). Re-gate after each re-launch: the review may have
	// committed fixes, so the prior gate would be stale — and re-deciding through the
	// same verify.Review call is what keeps the loop's condition and the push verdict
	// from ever being computed from different state.
	for attempt := 2; attempt <= reviewVerdictMaxAttempts && result.Retry; attempt++ {
		log.Event(fmt.Sprintf(
			"review ↻ session exited cleanly (code 0) one turn short of its verdict over a clean, gate-green worktree — re-launching review %d/%d (BEH-624)",
			attempt-1, reviewVerdictMaxAttempts-1,
		))
		reviewOutcome = runReviewSession(attempt)
		if res, ok := f.interrupted(reviewOutcome); ok {
			return res
		}
		gateRes = fixes.Gates(fmt.Sprintf("%s-retry%d", gateStep, attempt))
		gateExit = gateRes.Outcome.ExitCode
		result = verify.Review(wt, gateRes.Outcome, reviewOutcome)
	}

	// Record the verdict and the HEAD it covered for a re-run (#44). Only a clean
	// tree is recorded: uncommitted edits are not part of any HEAD the verdict names.
	rememberVerdict := func() {
		head, err := wt.HeadSHA()
		if err != nil || reviewOutcome.ReviewDisposition == "" || !wt.Clean() {
			return
		}
		if err := recordReview(log.Dir, head, reviewOutcome.ReviewDisposition); err != nil {
			log.Event("review … warning: could not record the review verdict for a re-run: " + err.Error())
		}
	}
	rememberVerdict()

	// Flag the final review completeness so the run log carries the signal rather than
	// a green gate masquerading as a full review pass (BEH-525, from the BEH-499 OOM).
	if !result.ReviewComplete {
		log.Event("review ⚠ " + result.ReviewCompleteReason)
		fmt.Fprintln(os.Stderr, "warning: "+result.ReviewCompleteReason)
	} else {
		log.Event("review ✓ " + result.ReviewCompleteReason)
	}

	// A review that ran but declared a blocked disposition found a Blocker/Important
	// finding it could not autonomously resolve (BEH-580). The autonomous pipeline has
	// no human to answer the skill's approval prompt, so verify.Review fails the push
	// closed rather than shipping the finding to a PR unaddressed (the BEH-439 leak).
	if reviewOutcome.ReviewBlocked {
		log.Event("review ⚠ verdict declared a blocked disposition — an unresolved blocker/important finding needs a human decision; will not push (BEH-580)")
	}

	// comment posts a best-effort tracker breadcrumb so a gate-green, reviewed branch
	// that can't ship autonomously surfaces on the ticket instead of sitting silent in
	// a worktree (BEH-581 conflict path). A failure to comment is logged, never fatal.
	comment := func(body string) {
		if err := client.AddComment(args.Identifier, body); err != nil {
			log.Event("review … warning: could not post tracker breadcrumb: " + err.Error())
		}
	}

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
		return Result{OK: false, Disposition: RecommendClose}
	}
	if !result.OK {
		// A review killed mid-edit (spending cap / OOM) leaves its in-progress fixes
		// uncommitted. Without capturing them they vanish on the next resume — the
		// worktree is re-derived from the committed tip — and the resumed review
		// re-judges the original diff from scratch, flipping the verdict on the very
		// line review-1 had started fixing (BEH-559). Checkpoint-commit them (mirroring
		// the tdd stage's BEH-479 safety net) so the started work survives and the
		// resuming review's merge-base diff includes it. Done AFTER the push decision
		// above (which verify.Review made over the same, pre-checkpoint tree), so an
		// unverified, half-applied fix is never pushed — only preserved. A no-op when
		// the worktree is already clean (a red-but-clean gate has nothing uncommitted
		// to recover).
		if !wt.Clean() {
			if cErr := wt.Checkpoint(args.Identifier, reviewSession); cErr != nil {
				log.Event("⚠ review session left uncommitted edits and the recovery checkpoint commit failed (" + cErr.Error() + ") — recover them manually at " + worktreePath)
			} else {
				log.Event("✓ harness recovery checkpoint committed on " + wt.Branch() + " — the review session's in-progress edits are preserved (unverified: a resumed review will see them, finish or re-run before opening a PR)")
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
		// The gate's stderr tail goes in the run log, so a gate that could not run its
		// tool reads as PATH without opening the step log (#46).
		if gateExit != 0 && gateRes.Outcome.StderrTail != "" {
			log.Event("review · gate stderr tail:\n" + gateRes.Outcome.StderrTail)
		}
		return capFailure(reviewOutcome)
	}
	log.Event("review ✓ " + result.Reason)

	// --- land the branch: rebase onto the latest base, then push + PR (only on a green harness gate) ---
	// internal/ship owns the whole landing — the refetch (a sibling PR can merge during
	// the multi-minute gate, BEH-570), the replay, the disjoint-history guard (BEH-597),
	// the post-rebase collapse check (BEH-680), the force-with-lease push and the PR —
	// the same sequence the daemon's committed-fix recovery and the CI watch's reactive
	// rebase use, so the three can never drift on how the harness lands a branch. The
	// worktree is clean here (verified just above), so the replay's reset --hard can't
	// discard uncommitted work (BEH-618).
	//
	// The review contributes one thing: a genuine content conflict no longer dead-ends
	// (BEH-581). Rather than strand ~30 min of reviewed, gate-green work, it launches a
	// bounded sandboxed conflict-resolution session over the worktree (internal/fix,
	// beside the post-PR CI fix) and re-runs the host gate on the resolved tree
	// before ship pushes. The hook returns false when it could not land a clean, re-gated rebase,
	// having already left a tracker breadcrumb so the work surfaces autonomously.
	landing := ship.Land(h, log, t, func() bool {
		return resolveRebaseConflict(fixes, target, comment, "pre-push rebase", gateStep+"-postrebase")
	})
	switch landing.Verdict {
	case ship.Disjoint:
		// A disjoint history (no common ancestor with origin/main) is NOT a content
		// conflict: the replay collided on every one of the branch's disjoint commits, so
		// the BEH-581 session is the wrong tool and ship never offers it. The tdd gate
		// fails this upstream (verify.Tdd's DisjointHistory check), so this is the
		// defence-in-depth backstop for a resumed/standalone review. Keep the worktree for
		// manual recovery (`git reset --hard origin/main` + cherry-pick the handoff
		// commits) and surface it.
		log.Event("review ✗ disjoint branch history — no common ancestor with origin/main; not a content conflict, needs manual recovery (BEH-597) — keeping worktree, nothing pushed")
		comment(fmt.Sprintf(
			"Branch `%s` has a disjoint history from `main` (no common ancestor / empty merge-base), so it cannot be rebased or merged as-is. This is not a content conflict — the handoff commits need to be re-applied onto current `main` (e.g. `git reset --hard origin/main` then cherry-pick them). The branch passed cold review and the harness gate; it is waiting in a worktree. (BEH-597)",
			wt.Branch(),
		))
		return Result{OK: false}
	case ship.Conflict:
		// resolveRebaseConflict already narrated why and left the breadcrumb.
		return Result{OK: false}
	case ship.Collapsed:
		// The replay collapsed the branch to zero net change — a sibling PR landed the
		// same fix during the gate. Route to the same recommend-close disposition as the
		// pre-rebase BEH-603 check: keep the worktree, push nothing, and let the loop
		// flag the ticket for a human to close as superseded.
		log.Event("review ⊘ rebase collapsed the branch to zero net change against origin/main — keeping worktree, nothing pushed; recommending close (BEH-680)")
		return Result{OK: false, Disposition: RecommendClose}
	case ship.PushFailed:
		log.Event("review ✗ push failed: " + landing.Err.Error() + " — keeping worktree")
		return Result{OK: false}
	case ship.PRFailed:
		log.Event("review ✗ gh pr create failed: " + landing.Err.Error() + " — branch pushed, open the PR manually")
		return Result{OK: false}
	}
	log.Structured(loopstream.Record{Kind: loopstream.KindPROpened, Ticket: args.Identifier, Stage: "review", Message: "review ✓ PR opened: " + landing.URL})
	// Landing replayed the reviewed commits onto the latest base, rewriting their
	// SHAs: the verdict covers the landed tip, so re-record it there (#44).
	rememberVerdict()

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
	log.Event("watching CI for " + wt.Branch() + " …")
	ciResult := h.WatchCI(hostio.CIWatch{
		Fix: fixes.CI(target),
		Resolve: mergedPrerequisiteResolver(log, t,
			func() (ticket.Ticket, error) { return client.FetchTicket(args.Identifier) },
			func(trigger string) bool {
				return resolveRebaseConflict(fixes, target, comment, trigger, gateStep+"-cirebase")
			}),
	})
	// Zero-net-diff short-circuit (BEH-602): the branch became a no-op against the
	// latest origin/main only AFTER the pre-push rebase (a sibling PR landed the same
	// fix during the multi-minute gate, which the pre-rebase BEH-603 push-gate check
	// couldn't see), so the PR is open but there is nothing for CI to validate. Honour
	// the recommend-close disposition rather than burn the poll budget — keep the PR +
	// worktree and let the loop flag the ticket for a human to close as superseded.
	// Recommend-close outranks the open PR here: a no-op PR is not a ship, and the
	// loop's close breadcrumb already covers the already-opened case.
	if ciResult.RecommendClose {
		log.Event("review ⊘ " + ciResult.Reason + " — keeping PR + worktree; recommending close (BEH-602)")
		return Result{OK: false, Disposition: RecommendClose}
	}
	if ciResult.SpendingCapAbort {
		// The auto-fix session hit an active spending cap — not a code defect or
		// unfixable CI, just a billing window that resets. Name the retry-after-reset
		// class (↻) so a re-dispatch after the cap resets lands the fix, instead of a
		// spurious "CI did not go green" failure (BEH-571). PR + worktree are kept.
		// The disposition is Shipped, not CapAborted: the branch is already pushed and
		// the PR open, and a shipped PR outranks a cap abort for both the loop (which
		// never releases a shipped ticket) and the breaker (which counts it a success).
		log.Event("review ↻ CI auto-fix deferred — spending cap reached, retry after reset (BEH-494) — keeping PR + worktree")
		return Result{OK: false, Disposition: Shipped}
	}
	if !ciResult.OK {
		log.Event("review ✗ CI did not go green: " + ciResult.Reason + " — keeping PR + worktree")
		if s := ci.Summarize(ciResult.Failing); s != "" {
			fmt.Fprintf(os.Stderr, "\nFailing CI checks for %s:\n%s", wt.Branch(), s)
		}
		// CI red after the auto-fix budget still leaves a reviewable PR for a human to
		// take over — NOT a ship failure, so the breaker must not count it.
		return Result{OK: false, Disposition: Shipped}
	}
	log.Event("review ✓ " + ciResult.Reason)

	return Result{OK: true, Disposition: Shipped}
}

// mergedPrerequisiteResolver is the CI watch's Resolve (#82): when the reactive
// rebase hits a content conflict, it runs the conflict-resolution session and the
// host gate once — but only when the conflict is a prerequisite's merge. The
// ticket was fetched while each of t.Prerequisites was still an open PR; if a
// re-fetch no longer lists one, that PR merged into the base underneath this one
// (the incident: a sibling editing the same paragraph of a shared design doc
// merged ~15s after this PR opened). Any other conflict, or a second one, is left
// for a human as before. Nil when the ticket named no open prerequisite, so the
// watch behaves exactly as it always did.
func mergedPrerequisiteResolver(
	log *runlog.Logger, t ticket.Ticket,
	refetch func() (ticket.Ticket, error), resolve func(trigger string) bool,
) func() bool {
	if len(t.Prerequisites) == 0 {
		return nil
	}
	used := false
	return func() bool {
		if used {
			return false
		}
		used = true
		now, err := refetch()
		if err != nil {
			log.Event("review ⚠ PR conflicts with base; could not re-read the ticket's prerequisites (" + err.Error() + ") — leaving the conflict for a human")
			return false
		}
		still := map[string]bool{}
		for _, p := range now.Prerequisites {
			still[p.Identifier] = true
		}
		for _, p := range t.Prerequisites {
			if !still[p.Identifier] {
				return resolve("post-PR rebase after prerequisite " + p.Identifier + " (`" + p.Branch + "`) merged")
			}
		}
		log.Event("review · PR conflicts with base but every prerequisite is still open — not a prerequisite's merge; leaving the conflict for a human (#82)")
		return false
	}
}

// resolveRebaseConflict launches the bounded sandboxed conflict resolution
// (internal/fix) when a rebase onto base hits a genuine content conflict: the
// proactive pre-push rebase (BEH-581), or the CI watch's reactive one once a
// prerequisite merged (#82) — the dead-end that previously stranded a
// fully-reviewed, gate-green branch in a worktree with no autonomous recovery. The
// branch is the harness's own, so it is replayed (fix.Rebase) and force-pushed. It
// returns true only when the resolution is verified and the re-gate (gateLabel)
// over the rewritten tree is green, authorising the push. Any other failure keeps
// the worktree and leaves a tracker breadcrumb so the work surfaces (a
// spending-cap abort defers quietly — it retries after reset, not a conflict).
func resolveRebaseConflict(fixes fix.Env, target fix.Target, comment func(string), trigger, gateLabel string) bool {
	fixes.Log.Event("review ↻ " + trigger + " hit a content conflict — launching a sandboxed conflict-resolution session (BEH-581)")
	res := fixes.ResolveConflict(fix.Conflict{
		Target:   target,
		Strategy: fix.Rebase,
		Regate:   func() fix.GateResult { return fixes.Gates(gateLabel) },
	})
	branch := fixes.Host.Worktree().Branch()
	switch {
	case res.OK, res.SpendingCapAbort:
	case res.Resolved:
		comment(fmt.Sprintf(
			"The %s conflict was auto-resolved on `%s`, but the post-rebase gate re-run failed (exit %d). The rebased branch is waiting in a worktree for a look. (BEH-581)",
			trigger, branch, res.Gate.Outcome.ExitCode,
		))
	default:
		comment(fmt.Sprintf(
			"Auto-rebase onto `main` (%s) could not be completed for `%s`: %s This branch passed cold review and the harness gate, but it now needs a manual rebase onto `main`. It is waiting in a worktree. (BEH-581)",
			trigger, branch, res.Reason,
		))
	}
	return res.OK
}
