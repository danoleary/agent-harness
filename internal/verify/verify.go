// Package verify decides whether a finished Stage really did its job, judged
// against git ground truth rather than the agent's self-report.
//
// Every decision here gathers its own ground truth. Before, a Stage read the git
// state, packed it into a parameter struct, and handed the struct over: the
// gathering lived in internal/stages (and, for the tdd truth, in internal/git),
// the deciding lived here, and the boolean expressions that bridged them — the
// part that actually goes wrong — belonged to neither. Taking a [GroundTruth]
// instead moves the seam outward, so one module owns "did this Stage do its job?"
// end to end and a test exercises the real reads rather than a hand-written
// struct.
package verify

import (
	"fmt"

	"github.com/danoleary/agent-harness/internal/session"
)

// oomExitCode is the container exit code for a SIGKILL (128+9), which the sandbox
// memory-pressure OOM-killer produces — the kill that struck the BEH-499 review
// session mid-gate (BEH-407/477/491/519 document the same class for build/typecheck).
const oomExitCode = 137

// GroundTruth is the host-side git state the decisions below read for themselves:
// the harness's own assertions about a ticket's worktree and branch, verified
// independently of whatever the sandbox reported (CONTEXT.md "Ground truth").
// Every method is keyed by the ticket slug, matching the port a Stage already
// holds — [hostio.Host] satisfies it, so a Stage passes the Host it has and
// nothing re-packs the reads into a struct on the way in.
//
// Declaring the port HERE, rather than having internal/git hand back a verify
// type, is also what keeps the dependency pointing the right way: the low-level
// git module no longer imports the decision module, and can be used without it.
type GroundTruth interface {
	// WorktreeExists reports whether the ticket's worktree directory exists.
	WorktreeExists(slug string) bool
	// WorktreeClean reports whether the worktree has no uncommitted changes.
	WorktreeClean(slug string) bool
	// CommitsAhead is the number of commits on the feature branch ahead of
	// origin/main.
	CommitsAhead(slug string) int
	// BranchDisjoint reports whether the feature branch shares NO common ancestor
	// with origin/main (an empty `git merge-base`). A disjoint branch is "ahead" by
	// all of its own commits (963 in BEH-355), so it passes the commits-ahead check,
	// yet it is never a real single-ticket handoff and its rebase collides
	// immediately.
	BranchDisjoint(slug string) bool
	// BranchDiffEmpty reports whether the branch's committed tip makes zero net
	// change against origin/main (an empty `git diff origin/main`).
	BranchDiffEmpty(slug string) bool
	// BranchExists reports whether the feature branch resolves to a git revision in
	// the host checkout.
	BranchExists(slug string) bool
	// BranchPushed reports whether the feature branch reached origin.
	BranchPushed(slug string) bool
	// IsRebased reports whether origin/main is now an ancestor of the branch tip.
	IsRebased(slug string) bool
	// PRExists reports whether the branch has reached a PR in ANY state (open,
	// merged, or closed).
	PRExists(slug string) bool
}

// Result is the one verdict every decision in this package returns: OK plus the
// Reason that is logged either way, so a blocked push or a kept worktree always
// says why. The remaining fields are the extra bits a particular decision needs
// to hand back; each names the decision that sets it and is the zero value
// everywhere else.
type Result struct {
	// OK is the decision. For [WorktreeReap] it means "reap"; everywhere else it
	// means the Stage did its job.
	OK bool
	// Reason is the human-readable why, narrated by the caller on both verdicts.
	Reason string
	// RecommendClose marks the BEH-603 no-op disposition: a clean, gate-green,
	// reviewed branch whose committed tip makes zero net change against origin/main
	// (an empty `git diff origin/main`). It is NOT a push (OK stays false — there is
	// nothing to ship) and NOT a plain failure — the agents correctly concluded the
	// ticket should be closed as a duplicate/superseded. Set by [Review] and
	// [PostRebasePush].
	RecommendClose bool
	// SpendingCapAbort distinguishes the retry-after-reset class (the session never
	// ran) so the caller defers quietly instead of filing a "couldn't resolve the
	// conflict" breadcrumb. Set by [RebaseResolution].
	SpendingCapAbort bool
	// Retry marks the one incompleteness class worth a bounded in-stage re-launch
	// over the same unchanged worktree (BEH-624). Set by [Review].
	Retry bool
	// ReviewComplete and ReviewCompleteReason report whether the in-sandbox
	// /review-worktree session actually performed its qualitative seven-lens pass.
	// They are deliberately separate from OK (the push gate): the push is authorised
	// by the harness's own host-side gate re-run, but a green gate only proves the
	// diff compiles/lints — it says nothing about whether the human-style review ran.
	// Both are reported so a review killed before its verdict isn't silently treated
	// as a full review pass. Set by [Review].
	ReviewComplete       bool
	ReviewCompleteReason string
	// DisjointWorkTrapped marks the BEH-609 "verified work trapped on a disjoint
	// branch" case: the session produced a worktree AND committed real work, but the
	// branch roots at a disjoint history (an empty merge-base with origin/main — the
	// BEH-355/BEH-500 condition), so the gate fails it even though the diff is
	// genuine. Re-launching can never escape this — no in-sandbox work changes the
	// branch's root commit — so the caller instead re-grafts the branch's content
	// diff onto a fresh base off origin/main and re-verifies, rather than discarding
	// the run and re-running the same doomed pipeline. It is deliberately false on
	// the ordinary failure shapes (no worktree, empty diff, or a healthy branch that
	// failed for another reason). Set by [Tdd].
	DisjointWorkTrapped bool
}

// Tdd decides whether a tdd session really did its job. Success requires both a
// worktree and at least one commit on the feature branch — the agent's own
// "I'm done" is never authoritative (DESIGN.md: success is ground-truth).
func Tdd(g GroundTruth, slug string) Result {
	if !g.WorktreeExists(slug) {
		return Result{OK: false, Reason: "worktree was not created"}
	}
	ahead := g.CommitsAhead(slug)
	if ahead < 1 {
		return Result{OK: false, Reason: "no handoff commit ahead of origin/main"}
	}
	// A disjoint history (no common ancestor with origin/main) is checked AFTER the
	// commits-ahead gate because a disjoint branch always reads as ahead — by all of
	// its own commits. It is a suspect ground truth, never a healthy handoff: the
	// pre-push rebase would try to replay every disjoint commit onto main and collide
	// immediately, and the BEH-355 incident showed it masquerading as a 963-commits-
	// ahead success. Fail it here so the slice never reaches the push path (BEH-597).
	//
	// The three conditions that have held by the time we get here — a worktree, real
	// committed work, and a disjoint root — are exactly the BEH-609 trapped-work
	// shape, so the failure carries DisjointWorkTrapped for the caller's regraft
	// rescue. Reading it off the same walk is what keeps the rescue's precondition
	// from drifting from the gate that produced it.
	if g.BranchDisjoint(slug) {
		return Result{
			OK:                  false,
			DisjointWorkTrapped: true,
			Reason:              "branch shares no common ancestor with origin/main (disjoint history / empty merge-base) — a suspect ground truth, not a real handoff",
		}
	}
	plural := "s"
	if ahead == 1 {
		plural = ""
	}
	return Result{OK: true, Reason: fmt.Sprintf("worktree present and branch is %d commit%s ahead of main", ahead, plural)}
}

// Retrospective decides whether a retrospective session really ran, judged by
// the *presence* of its findings dropbox (`/findings/out.json`) rather than the
// agent's self-report. An empty `[]` is a valid "ran, found nothing" (the file
// is still present → success); an absent file means the step never ran and is a
// failure — the rule that stops a silently-skipped retrospective from
// masquerading as "no issues found" (DESIGN.md "Success is ground-truth").
//
// dropboxExists is the caller's read of the mounted dropbox — the one input here
// that is not git, so it does not come from [GroundTruth]. Everything else is read
// off the session outcome.
//
// Three signals distinguish the failure class when the dropbox is absent, so a
// crash the agent couldn't avoid isn't mislabelled as the agent skipping the step:
//
//   - SpendingCapAbort (BEH-494): a session killed by a billing/usage cap before
//     doing any work never gets the chance to write the dropbox, so the generic
//     "never ran" reads as the agent misbehaving. When the cap fired, report the
//     distinct retry-after-reset class instead. It is the most specific cause, so
//     it takes precedence over BOTH the exit code AND a present dropbox: the
//     synthetic cap abort replaces a genuine assistant turn, so any out.json that
//     exists alongside it can only be a stale prior-run file or the early `[]` the
//     skill writes before its analysis — never proof the retrospective completed.
//     Checking dropbox-present first would let that file silently mask the abort
//     into a false "ran, found nothing" success and skip the re-run (BEH-568).
//   - ExitCode (BEH-536): the retrospective is a long, read-heavy step that hit
//     its wall-clock cap (exit 137) mid-investigation — after the analysis but
//     before its write. A 137 kill with no dropbox is "killed before writing —
//     retry", NOT "never ran" (mirrors reviewQualitative's OOM branch). Combined
//     with the skill's incremental write, this leaves a clear, actionable signal.
//     Unlike the cap abort, a 137 is checked *after* dropbox-present: a present
//     dropbox there is genuine output written before a teardown kill, so it stands.
//   - TurnZeroNoOp (BEH-709): the session exited 0 with is_error=false but did zero
//     real work — the pinned CLI's turn-0 no-op, where a `!`+backtick directive in
//     the prompt errored host-of-sandbox and degenerated the whole session before
//     it could write. Its exit-0/success shape otherwise falls through to the plain
//     "never ran", masking a genuine crash as the agent skipping the step; naming it
//     distinctly (and as a retry, since the trigger is content/parse nondeterministic)
//     stops that. Checked after the 137 kill: a turn-0 no-op is exit 0 by definition,
//     so the two never collide.
//
// Only a genuinely absent-and-not-crashed dropbox keeps the "never ran" wording.
func Retrospective(dropboxExists bool, out session.Outcome) Result {
	if out.SpendingCapAbort {
		return Result{OK: false, Reason: "session aborted before running — spending cap reached, retry after reset"}
	}
	if dropboxExists {
		return Result{OK: true, Reason: "findings dropbox out.json present"}
	}
	if out.ExitCode == oomExitCode {
		return Result{OK: false, Reason: "session killed (exit 137) before writing findings — likely OOM or wall-clock cap, retry"}
	}
	if out.TurnZeroNoOp {
		return Result{OK: false, Reason: "session turn-0-crashed (exited 0 with no output after ≤3 turns) before writing findings — the prompt likely poisoned the session, retry"}
	}
	return Result{OK: false, Reason: "findings dropbox out.json was not written — retrospective never ran"}
}

// RetrospectivePreconditions decides whether it is worth launching the
// retrospective sandbox at all (BEH-553). A retrospective scheduled for a ticket
// whose upstream /tdd + /review steps produced neither a branch nor any session
// transcript has no inputs: it can only emit an empty [] that masks the
// misscheduling, or manufacture a self-referential finding about the missing
// inputs. So when BOTH inputs are absent the caller skips host-side, before
// spending the sandbox cap. OK == true means "proceed"; OK == false means "skip".
//
// The two inputs mirror what the /retrospective skill reads: the feature branch
// (the diff), read here off ground truth, and the ticket's prior
// implementation/review session transcripts — a log-dir read, not a git one, so
// priorTranscripts arrives as a parameter.
//
// The skip is BOTH-absent, deliberately NOT "either is absent" (the literal
// reading of the finding). The pipeline runs the retrospective even on a FAILED
// slice — "exactly the run worth mining for findings" — and a failed slice
// routinely has transcripts but no branch (the session crashed before creating
// the worktree). Skipping whenever the branch is missing would suppress those
// legitimate retrospectives, so a single real input is enough to proceed.
func RetrospectivePreconditions(g GroundTruth, slug string, priorTranscripts bool) Result {
	if !priorTranscripts && !g.BranchExists(slug) {
		return Result{OK: false, Reason: "no upstream sessions to retrospect — feature branch does not resolve and no implementation/review transcripts exist; the /tdd + /review steps produced nothing"}
	}
	return Result{OK: true, Reason: "upstream inputs present — a feature branch and/or prior session transcripts exist to retrospect"}
}

// Review decides whether a reviewed branch may ship, and — on the one recoverable
// incompleteness class — whether the review session is worth re-launching first.
//
// gateExit is the exit code of the harness's OWN host-side gate re-run: green iff
// every config-declared named gate (herd: `pnpm run check`, `pnpm run typecheck`;
// BEH-634) passed in the throwaway containers on the feature branch (DESIGN.md:
// ground truth = the harness's own gate run is green). out is the review session's
// outcome, read only for what the *stream* saw — whether the verdict was emitted,
// whether it declared blocked, how the container exited. Everything else Review
// reads for itself off git. Because the inputs are only the harness's own gate
// result, the worktree's git state, and whether the review emitted a verdict, a
// branch can never be pushed on the agent's say-so (AC: no push on self-report).
//
// The worktree being clean is checked first because it qualifies the gate result:
// the gate runs against the worktree's working tree (committed + uncommitted), but
// the push ships only the committed branch tip. A dirty worktree therefore means the
// gate validated a different tree than would ship (e.g. a review session that edited
// but never committed), so its green/red verdict can't be trusted as the push gate.
//
// An empty diff is checked next — after the clean-tree gate, before the gate/review
// checks (BEH-603). A clean worktree whose committed tip makes zero net change
// against origin/main has nothing to ship: the gate is trivially green and any
// review was over an empty diff. Rather than open the empty-commit PR the harness
// previously did (PR #642), Review returns the recommend-close disposition
// (OK false, RecommendClose true) so the caller surfaces a handoff for a human to
// close the ticket as a duplicate/superseded. Checking it after the clean tree keeps
// a dirty tree — where the real change may still be uncommitted — from ever
// recommending the close of a live ticket.
//
// ReviewComplete is checked before the blocked disposition and fails the gate closed
// (BEH-569): a review session killed before its verdict (a spending-cap abort, an
// OOM) leaves the diff with a green gate but ZERO qualitative review. Pushing then
// opens a PR that no one actually reviewed, while the gate re-run masquerades as the
// review signal. Blocking keeps the worktree for a resumed review rather than
// shipping unreviewed.
//
// The blocked disposition is checked last and also fails closed (BEH-580): a review
// that DID run but declared blocked found a Blocker/Important finding it could not
// autonomously resolve. The autonomous pipeline has no human to answer the skill's
// approval prompt, so pushing would open a PR with the finding unaddressed (the
// BEH-439 leak). Blocking keeps the worktree for a human decision — distinct from
// the BEH-569 case (there the review never ran; here it ran and found a real,
// unresolved issue).
func Review(g GroundTruth, slug string, gateExit int, out session.Outcome) Result {
	clean := g.WorktreeClean(slug)
	gatesGreen := gateExit == 0
	complete, completeReason := reviewQualitative(out)

	// The retry verdict is independent of the push verdict below — it asks "is this
	// worth another turn?", not "may this ship" — so it rides along on every Review
	// and the caller's re-launch loop re-reads it from the same walk that will
	// eventually decide the push. Nothing downstream has to re-derive the inputs.
	r := Result{
		ReviewComplete:       complete,
		ReviewCompleteReason: completeReason,
		Retry:                reviewVerdictRetry(out, clean, gatesGreen),
	}

	switch {
	case !clean:
		r.Reason = "worktree has uncommitted changes — the gate validated a different tree than would ship; not pushing"
	case g.BranchDiffEmpty(slug):
		r.RecommendClose = true
		r.Reason = "branch makes zero net change against origin/main (empty diff) — nothing to ship; recommend closing the ticket as a duplicate/superseded rather than opening an empty-commit PR"
	case !gatesGreen:
		r.Reason = "harness gate re-run is red — not pushing"
	case !complete:
		r.Reason = "qualitative review never produced a verdict — gates green but the seven-lens review did not run; not pushing (fail-closed)"
	case out.ReviewBlocked:
		r.Reason = "review verdict declared a blocked disposition — an unresolved blocker/important finding needs a human decision; not pushing (fail-closed)"
	default:
		r.OK = true
		r.Reason = "harness gate re-run is green and the qualitative review emitted a clear verdict — clear to push + open PR"
	}
	return r
}

// reviewQualitative classifies whether the review session emitted its verdict —
// the "## Review:" report that ends the seven-lens pass (BEH-525). The verdict is
// the only proof the lenses ran, so its presence means complete regardless of how
// the container exited; its absence means incomplete regardless of a green host-
// side gate. The OOM case (exit 137) is the one the BEH-499 review hit — killed
// mid-gate before reaching the report — so it gets a distinct, named reason; any
// other end before the verdict is reported with its exit code but not mislabelled
// as an OOM.
func reviewQualitative(out session.Outcome) (complete bool, reason string) {
	if out.ReviewVerdictEmitted {
		return true, "review session emitted its seven-lens verdict"
	}
	if out.ExitCode == oomExitCode {
		return false, "review session OOM-killed (exit 137) before emitting a verdict — gates green but qualitative review incomplete"
	}
	return false, fmt.Sprintf("review session exited %d before emitting a verdict — qualitative review incomplete", out.ExitCode)
}

// reviewVerdictRetry decides whether a review session that ended WITHOUT its verdict
// is eligible for a bounded in-stage re-launch over the same unchanged worktree
// (BEH-624) — the one incompleteness class that previously had no in-stage recovery
// and instead discarded a fully verified, gate-green diff to be re-reviewed from
// scratch on a later whole-pipeline dispatch.
//
// It is the cheapest case to retry, so the bar is deliberately narrow: re-launch
// ONLY when the diff is already proven good and the review merely stopped a turn
// short of printing the verdict. Every other condition routes elsewhere:
//
//   - A verdict already emitted → there is nothing to retry (reviewQualitative
//     already calls it complete).
//   - A non-zero exit code (an OOM 137, a crash) is NOT this class — it is the
//     killed-before-verdict case reviewQualitative already names; re-launching over a
//     possibly-corrupt run is not the cheap byte-identical retry this targets.
//   - A spending-cap abort is its own retry-after-reset class (the caller defers
//     until the cap resets), never an in-stage re-launch that would burn the same cap.
//   - A dirty worktree means the gate validated a different tree than would ship, so
//     the diff is not the proven-good artifact this retry assumes.
//   - Red gates mean the diff itself is broken — re-running the review can't make it
//     shippable, so it falls through to fail-closed with the worktree kept.
func reviewVerdictRetry(out session.Outcome, clean, gatesGreen bool) bool {
	return !out.ReviewVerdictEmitted &&
		out.ExitCode == 0 &&
		!out.SpendingCapAbort &&
		clean &&
		gatesGreen
}

// PostRebasePush is the second empty-diff gate, checked AFTER the pre-push rebase
// replays the branch onto the latest origin/main and BEFORE the push + `gh pr create`
// (BEH-680). The BEH-603 empty-diff check in [Review] runs on the pre-rebase tree, so
// it cannot see a branch that collapses to zero net change DURING the replay: a
// sibling PR merged the same fix while the multi-minute gate ran, or the BEH-581
// conflict-resolution session skipped a now-empty commit, leaving the branch
// identical to origin/main. Pushing that branch and running `gh pr create` hard-fails
// with "No commits between main and feat/…" — a wasted push, a hard error, and a
// misleading "open the PR manually" hint for a branch that has nothing to open a PR
// for. When the branch is empty, route to the same recommend-close disposition Review
// uses (OK false, RecommendClose true) so the ticket is handed off for a human to
// close as superseded rather than pushed. A non-empty branch is the normal ship path
// (OK true), clearing the push. A clean worktree is the rebase precondition, so —
// unlike Review — no clean-tree re-check is needed here.
func PostRebasePush(g GroundTruth, slug string) Result {
	if g.BranchDiffEmpty(slug) {
		return Result{OK: false, RecommendClose: true, Reason: "rebase collapsed the branch to zero net change against origin/main (empty diff) — nothing to ship; recommend closing the ticket as superseded rather than pushing an empty branch that `gh pr create` cannot open"}
	}
	return Result{OK: true, Reason: "branch still carries a real diff after the rebase — clear to push + open PR"}
}

// RebaseResolution decides whether a sandboxed conflict-resolution session
// actually rebased the branch onto origin/main cleanly — the gate on whether the
// pre-push path re-gates + pushes, or keeps the worktree for a human (BEH-581).
// Like every other harness gate it is judged on ground truth, never the agent's
// self-report: a green verdict requires a clean session exit, a clean worktree,
// AND the branch genuinely rebased onto base.
//
// Order matters. A spending-cap abort wins outright — the session took no real
// action, so the worktree's incidental state says nothing, and it is the one
// failure that must NOT surface as a content conflict (it just retries after the
// cap resets). A non-zero exit is next (the session crashed/timed out). Then a
// dirty worktree (unresolved or uncommitted conflict — the more actionable signal
// than the not-rebased one that also holds when dirty). Finally the silent trap
// the whole guard exists for: a CLEAN worktree still on the stale base, because
// the session resolved nothing and `rebase --abort`ed — a clean tree alone would
// wave that through and push a still-stale branch.
func RebaseResolution(g GroundTruth, slug string, out session.Outcome) Result {
	if out.SpendingCapAbort {
		return Result{OK: false, SpendingCapAbort: true, Reason: "conflict-resolution session aborted before resolving — spending cap reached, retry after reset"}
	}
	if out.ExitCode != 0 {
		return Result{OK: false, Reason: fmt.Sprintf("conflict-resolution session exited %d before resolving the rebase conflict", out.ExitCode)}
	}
	if !g.WorktreeClean(slug) {
		return Result{OK: false, Reason: "conflict-resolution session left the worktree dirty (unresolved or uncommitted conflict) — rebase did not cleanly complete"}
	}
	if !g.IsRebased(slug) {
		return Result{OK: false, Reason: "conflict-resolution session left the branch on its stale base (origin/main is not an ancestor) — it aborted the rebase rather than resolving it"}
	}
	return Result{OK: true, Reason: "conflict resolved and branch rebased onto origin/main — re-gating before push"}
}

// WorktreeReap decides whether the finished ticket's worktree is pure disk cost.
// OK means "reap"; Reason is logged either way so a kept worktree always says why
// it was kept.
//
// Both conditions must hold. A pushed branch alone is NOT enough, and that gap is
// what stranded BEH-783: a run pushed the branch, its `gh pr create` timed out, and
// the retrospective reaped the worktree anyway on the push alone. The loop's
// committed-fix recovery — the one mechanism that completes a pushed-but-PR-less
// branch — reads that worktree and bails immediately when it is missing, so the
// ticket had no route out. It was released to Todo, re-selected, re-implemented,
// re-reviewed, and re-rejected at the push every cycle until the circuit breaker
// tripped. Keeping the worktree until a PR actually exists preserves the breadcrumb
// the recovery needs.
//
// Only a PR makes reaping safe, because only a PR captures the branch somewhere a
// human can see it. Any PR state counts: merged and closed are both terminal
// dispositions a human owns, whereas "no PR" means the work is still invisible.
//
// The push is checked first, so the gh round-trip PRExists costs is never spent on
// a branch git says never reached origin — and an incoherent ground truth (gh
// answering for such a branch) takes the conservative keep: a stale breadcrumb
// costs disk, a wrong reap costs the work.
func WorktreeReap(g GroundTruth, slug string) Result {
	if !g.BranchPushed(slug) {
		return Result{OK: false, Reason: "branch not pushed to origin yet"}
	}
	if !g.PRExists(slug) {
		return Result{OK: false, Reason: "branch is pushed but has no PR; the committed-fix recovery needs the worktree to complete it"}
	}
	return Result{OK: true, Reason: "ticket clean (PR open + retrospective filed)"}
}
