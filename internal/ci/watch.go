package ci

import (
	"errors"
	"fmt"
	"time"
)

// Driver is the set of effects WatchAndFix orchestrates, each host-side or
// sandbox glue the real implementation (newGhDriver) wires to gh/git/session.
// Keeping them behind this seam lets the loop logic stay pure and unit-tested.
type Driver interface {
	// Poll blocks until the PR head's checks reach a terminal verdict (or the
	// poll budget is spent → ErrPollTimeout), returning that verdict + checks.
	Poll() (Verdict, []Check, error)
	// Rerun re-triggers the given failed checks once, to wash out flakes before
	// the harness starts editing code.
	Rerun(failed []Check) error
	// Fix fetches the failed logs and runs a sandboxed Claude session over the
	// worktree to diagnose + fix + commit locally.
	Fix(failed []Check) error
	// Push pushes the new fix commit to the PR branch (no force, append-only).
	Push() error
	// AwaitHeadRun blocks until CI has registered a run whose head commit matches the
	// branch HEAD — the commit the just-pushed fix produced — so the following Poll
	// reads the new commit's checks instead of the prior run's stale red. Without it a
	// fast fix lands before CI re-evaluates and the immediate re-poll re-acts on the
	// predating run, burning another fix session on an already-fixed problem (BEH-493).
	// It returns ErrCIRerunTimeout if CI never picks up the new HEAD within the budget.
	AwaitHeadRun() error
	// MergeState reports the PR's mergeability against base, polling through
	// GitHub's async UNKNOWN window. Green checks on a PR that conflicts with base
	// (main moved underneath it) are not actually shippable, so WatchAndFix gates
	// every green verdict on this. A persistently-UNKNOWN or unreadable state must
	// not block a green PR — it returns MergeUnknown / an error and the watch
	// degrades to passing rather than failing on something indeterminate.
	MergeState() (MergeVerdict, error)
	// DiffEmpty reports whether the pushed PR branch makes ZERO net change against
	// origin/main (an empty `git diff origin/main`). Such a branch has nothing for CI
	// to validate that main hasn't already validated, so watching it just burns the
	// poll budget on a PR that can never meaningfully go green (BEH-602/BEH-604, the
	// PR #642 waste). It is fail-safe: any doubt (an unreadable ref, a git error)
	// reports false so the normal watch still runs — the harness would rather watch
	// than wrongly skip a real branch.
	DiffEmpty() bool
	// DocsOnly reports whether the pushed PR branch's net diff against origin/main
	// touches ONLY documentation/prose paths no build gate or CI job reads (root
	// markdown, docs/**) — a change that provably cannot break CI, so polling the full
	// job just burns the poll budget (BEH-687, the PR #743 waste: a one-line AGENTS.md
	// edit died to the ~12-min poll timeout). Fail-safe like DiffEmpty: any git doubt
	// reports false so the normal watch still runs. Delegated to gitpkg.BranchDocsOnly.
	DocsOnly() bool
	// RebaseOntoBase auto-resolves a stale-base conflict: it rebases the PR branch
	// onto the latest origin/main and, if the replay is clean, re-pushes (force-with-
	// lease) and returns RebaseClean; a genuine content conflict aborts the rebase
	// (branch untouched) and returns RebaseConflict for a human. This is what keeps a
	// long pipeline from dead-ending when a sibling PR merged underneath it (BEH-570).
	RebaseOntoBase() (RebaseVerdict, error)
}

// Config bounds the whole CI watch: the auto-fix loop (used by WatchAndFix) and
// the per-poll wait (used by the production GhDriver). One struct so the cmd
// configures it all in one place.
type Config struct {
	// MaxFixAttempts caps how many diagnose+fix+push cycles to try before giving
	// up (the flake re-run is separate and does not count against it).
	MaxFixAttempts int
	// Budget is the overall wall-clock cap across the whole watch; no new fix
	// attempt starts once it is spent. It is a soft bound: it gates only the start
	// of each fix attempt, not the per-attempt waits — the poll waits and the
	// post-push wait-for-CI-rerun (AwaitHeadRun) are each ≤ PollBudget — so the real
	// elapsed time of a watch can overrun Budget by several PollBudget-bounded waits.
	Budget time.Duration
	// PollInterval is how often the GhDriver re-checks CI while it is pending.
	PollInterval time.Duration
	// PollBudget caps a single wait for checks to reach a terminal state.
	PollBudget time.Duration
	// PollStall is the no-progress window: once a pending check set that carries stall
	// evidence (a settled real gate, or an EXPECTED context — see stallEvidence) stops
	// changing for this long, the run is treated as wedged (a merge-queue/main-only
	// context reported as expected-but-never-run on the PR) and the poll bails with
	// ErrPollStalled instead of burning the full PollBudget. An all-pending cold start
	// with no such evidence is never stalled — it rides to PollBudget (BEH-620). Zero
	// disables it.
	PollStall time.Duration
	// PollMaxBudget is the hard ceiling for the adaptive budget extension (BEH-685). When it
	// exceeds PollBudget, a poll that reaches the soft PollBudget while a real (non-EXPECTED)
	// required gate is still in flight keeps polling up to this ceiling rather than timing
	// out — a slow-but-running gate (the browser-backed linting_and_tests routinely outlasts
	// the soft budget) must not be abandoned mid-run. Zero (or ≤ PollBudget) disables it;
	// PollBudget is then the only bound.
	PollMaxBudget time.Duration
}

// ErrSpendingCapActive is the sentinel a Fix callback returns when the auto-fix
// session could not run because the account's spending cap is active (BEH-571).
// It is a retry-after-reset condition — not a code defect, not unfixable CI — so
// WatchAndFix surfaces it as a distinct class rather than counting the 1-second
// no-op session as a fix-attempt failure (mirroring the implementation/review/
// retrospective stages' SpendingCapAbort handling from BEH-494).
var ErrSpendingCapActive = errors.New("auto-fix skipped: spending cap reached, retry after reset")

// Outcome is the terminal result of watching (and trying to fix) CI.
type Outcome struct {
	OK bool
	// Reason is a one-line human summary (green / fixed / why it gave up).
	Reason string
	// Failing carries the last failing checks when !OK, for the operator report.
	Failing []Check
	// SpendingCapAbort marks the non-OK outcome where the auto-fix session hit an
	// active spending cap (ErrSpendingCapActive) rather than failing on the code.
	// It lets the caller log a retry-after-reset breadcrumb instead of a spurious
	// "CI did not go green" failure (BEH-571).
	SpendingCapAbort bool
	// RecommendClose marks the zero-net-diff short-circuit (BEH-602): the pushed
	// branch makes no change against origin/main, so there is nothing for CI to
	// validate and the PR is a no-op. The caller surfaces this as the same
	// recommend-close disposition the review push-gate uses (BEH-603) — keep the
	// PR + worktree, flag the ticket for a human to close as superseded — rather
	// than burning the poll budget on a PR that can never meaningfully go green.
	RecommendClose bool
}

// WatchAndFix polls CI for the just-pushed PR and, on failure, drives a bounded
// auto-fix loop: one flake re-run first (a flaky real-time spec shouldn't trigger
// a code edit), then up to MaxFixAttempts diagnose+fix+push+re-poll cycles within
// the wall-clock Budget. It returns OK on green (immediately, after the re-run, or
// after a fix) and non-OK — keeping the PR + worktree for a human — on a poll
// timeout, a driver error, or attempt/budget exhaustion. now is injected so the
// budget is deterministic in tests.
func WatchAndFix(d Driver, cfg Config, now func() time.Time) Outcome {
	// A pushed branch that makes zero net change against origin/main has nothing for
	// CI to validate that main hasn't already validated — watching it just burns the
	// whole poll budget on a PR that can never meaningfully go green (BEH-602/BEH-604,
	// the PR #642 waste). The normal flow rarely reaches here: the review push-gate
	// recommend-closes a zero-diff branch before the push (BEH-603). But a branch that
	// became a no-op only AFTER the pre-push rebase (a sibling PR merged the same fix
	// during the gate) is already pushed, so this is the backstop. Short-circuit to the
	// recommend-close disposition before the first poll. DiffEmpty is fail-safe (false
	// on any git doubt), so a flaky read falls through to the normal watch.
	if d.DiffEmpty() {
		return Outcome{OK: false, RecommendClose: true, Reason: zeroDiffReason}
	}

	// A docs-only diff (root markdown / docs/**, nothing a gate or CI job reads) cannot
	// break CI, so polling the full job just burns the whole poll budget on a change that
	// can never fail it (BEH-687, the PR #743 waste: a one-line AGENTS.md edit died to the
	// ~12-min poll timeout). Short-circuit to a green ship — but still through greenOutcome
	// so the mergeability gate runs, exactly like the wedged-ready pass (BEH-614). DocsOnly
	// is fail-safe (false on any git doubt), so a real code change falls through to the
	// normal watch. Ordered after DiffEmpty: a zero-net-diff branch has no paths to classify.
	if d.DocsOnly() {
		return greenOutcome(d, docsOnlyReason)
	}

	deadline := now().Add(cfg.Budget)

	v, checks, err := d.Poll()
	if err != nil {
		return handlePollErr(d, err, checks)
	}
	if v == Passed {
		return greenOutcome(d, "CI is green")
	}
	failed := FailedChecks(checks)

	// One flake re-run before treating the failure as real — re-running the same
	// commit's failed checks washes out infra/flake reds without thrashing on a
	// code edit (per the ticket's flake-vs-real guidance).
	if err := d.Rerun(failed); err != nil {
		return Outcome{OK: false, Reason: "re-running failed checks errored: " + err.Error(), Failing: failed}
	}
	v, checks, err = d.Poll()
	if err != nil {
		return handlePollErr(d, err, checks)
	}
	if v == Passed {
		return greenOutcome(d, "CI green after re-running the failed checks (flake)")
	}
	failed = FailedChecks(checks)

	// Real failure: bounded diagnose + fix + push + re-poll.
	for attempt := 1; attempt <= cfg.MaxFixAttempts; attempt++ {
		if !now().Before(deadline) {
			return Outcome{
				OK:      false,
				Reason:  fmt.Sprintf("CI still red and the %s auto-fix budget is spent", cfg.Budget),
				Failing: failed,
			}
		}
		if err := d.Fix(failed); err != nil {
			// A spending-cap abort means the fix session never ran — defer rather than
			// count it as a fix-attempt failure, so a cap-active window doesn't sink an
			// otherwise-recoverable CI-fix that a post-reset re-dispatch would land
			// (BEH-571).
			if errors.Is(err, ErrSpendingCapActive) {
				return Outcome{
					OK:               false,
					Reason:           "auto-fix deferred — spending cap reached, retry after reset",
					Failing:          failed,
					SpendingCapAbort: true,
				}
			}
			return Outcome{OK: false, Reason: "auto-fix session failed: " + err.Error(), Failing: failed}
		}
		if err := d.Push(); err != nil {
			return Outcome{OK: false, Reason: "pushing the fix failed: " + err.Error(), Failing: failed}
		}
		// Don't re-poll until CI has actually started a run for the commit we just
		// pushed. A fast fix routinely lands before CI re-evaluates; the prior run's
		// red is still showing, and an immediate re-poll would re-act on that stale
		// failure — launching another fix session against an already-fixed problem
		// (BEH-493). Wait for CI to catch up to the new HEAD first.
		if err := d.AwaitHeadRun(); err != nil {
			return ciRerunErrOutcome(err, failed)
		}
		v, checks, err = d.Poll()
		if err != nil {
			return handlePollErr(d, err, checks)
		}
		if v == Passed {
			return greenOutcome(d, fmt.Sprintf("CI green after %d auto-fix attempt(s)", attempt))
		}
		failed = FailedChecks(checks)
	}

	return Outcome{
		OK:      false,
		Reason:  fmt.Sprintf("CI still red after %d auto-fix attempt(s)", cfg.MaxFixAttempts),
		Failing: failed,
	}
}

// zeroDiffReason is the operator-facing summary when the watch short-circuits a
// pushed branch that makes zero net change against origin/main (BEH-602): nothing
// for CI to validate, so recommend closing the ticket/PR as superseded rather than
// polling a PR that can never meaningfully go green.
const zeroDiffReason = "pushed branch makes zero net change against origin/main (empty diff) — nothing for CI to validate; recommend closing the PR/ticket as a duplicate/superseded rather than watching a no-op PR"

// docsOnlyReason is the operator-facing summary when the watch short-circuits a
// docs-only branch (BEH-687): the diff touches only documentation/prose paths no
// gate or CI job reads, so the change cannot break CI and the full poll budget would
// be burned for nothing. The PR is a trivially-mergeable green ship (still gated on
// mergeability via greenOutcome), so the watch passes immediately.
const docsOnlyReason = "diff touches only docs/prose paths no gate or CI job reads — the change cannot break CI; passing the watch immediately as trivially-mergeable instead of burning the poll budget on a job it cannot fail"

// wedgedReadyReason is the operator-facing summary for a wedged-ready pass (BEH-614):
// every real gate is green and the only thing left pending is a structurally-wedged
// required context — a merge-queue/main-only check GitHub never schedules on the PR
// head. The PR is done and ready for the merge queue, so the watch passes (after the
// usual mergeability gate) instead of burning the poll budget on a check that will
// never move on the PR.
const wedgedReadyReason = "CI green on all gates that run on the PR; the only pending check is a structurally-wedged required context (merge-queue/main-only) — ready for the merge queue"

// handlePollErr routes a poll error to an outcome. The wedged-ready sentinel is a
// SUCCESS — route it through greenOutcome so the mergeability gate still runs before
// declaring the PR ready (BEH-614); every other error is the failure-shaped
// pollErrOutcome (timeout, stall, unobservable, real gh failure).
func handlePollErr(d Driver, err error, checks []Check) Outcome {
	if errors.Is(err, ErrWedgedReadyForMergeQueue) {
		return greenOutcome(d, wedgedReadyReason)
	}
	return pollErrOutcome(err, checks)
}

// mergeConflictReason is the operator-facing summary when CI is green but the PR
// conflicts with base. It is deliberately distinct from a check-failure reason:
// this is a rebase/merge problem, not a test failure, so the human resolves it
// by rebasing rather than chasing a red check (BEH-484).
const mergeConflictReason = "CI is green but the PR conflicts with base (merge conflict — main moved underneath it); rebase the branch and re-push"

// mergeUnconfirmedNote is appended to a green pass when the mergeability probe
// could not resolve — a persistently-UNKNOWN window or an unreadable/errored
// merge state. CI is green and was observed, so the run still passes; the
// conflict-with-base guard simply could not run, and the operator is told to
// eyeball mergeability rather than handed a bare "CI is green". This mirrors the
// checks-unobservable degrade, which likewise surfaces a *distinct* reason
// instead of silently passing (BEH-484/BEH-476).
const mergeUnconfirmedNote = " — note: mergeability against base could not be confirmed; check the PR has no merge conflict before merging"

// maxRebaseAttempts caps how many times a green watch will auto-rebase a stale-base
// conflict before deferring to a human. main can keep moving underneath the branch,
// so the loop must not chase it forever; in practice one rebase resolves it. The cap
// is the backstop against a pathological merge storm (BEH-570).
const maxRebaseAttempts = 2

// rebaseFailedReason / rebaseBrokeCIReason are the operator summaries for the two
// ways an auto-rebase can fall short of a clean pass: the rebase/re-push itself
// errored (a gh/git failure), or it applied cleanly but the rebased commit is now
// red on CI (the moved base genuinely broke the build — a real failure, not a
// merge-conflict dead-end).
const (
	rebaseFailedReason  = "CI is green but auto-rebasing the PR onto base failed: "
	rebaseBrokeCIReason = "auto-rebased the PR onto the latest base but CI is now red on the rebased commit"
)

// greenOutcome confirms the PR is also mergeable before declaring CI a pass, and —
// new in BEH-570 — auto-resolves the common stale-base case instead of dead-ending
// on it. On a MergeConflicting verdict it rebases the branch onto the latest base
// (RebaseOntoBase): a stale-base "conflict" (a sibling PR merged underneath) replays
// cleanly, is re-pushed, and — once CI re-runs green on the rebased commit and the
// merge re-reads clean — passes with no human needed. Only a genuine content
// conflict (RebaseConflict) keeps the distinct non-OK merge-conflict outcome a human
// resolves. The rebase loop is capped (maxRebaseAttempts) so a continuously-moving
// main can't spin it forever. An unreadable or persistently-UNKNOWN merge state must
// not block an otherwise-green PR, so it degrades to a green pass with the conflict
// guard flagged as unrun (mergeUnconfirmedNote), mirroring the checks-unobservable
// degrade rather than passing silently.
func greenOutcome(d Driver, reason string) Outcome {
	for attempt := 0; ; attempt++ {
		switch mv, err := d.MergeState(); {
		case err == nil && mv == MergeClean:
			return Outcome{OK: true, Reason: reason}
		case err == nil && mv == MergeConflicting:
			// Fall through to the auto-rebase handling below.
		default:
			// Indeterminate (persistent UNKNOWN) or unreadable merge state: never block
			// an otherwise-green PR, but flag that mergeability wasn't confirmed.
			return Outcome{OK: true, Reason: reason + mergeUnconfirmedNote}
		}

		// The PR conflicts with base. Once the rebase attempts are spent, hand off.
		if attempt >= maxRebaseAttempts {
			return Outcome{OK: false, Reason: mergeConflictReason}
		}
		switch rv, err := d.RebaseOntoBase(); {
		case err != nil:
			return Outcome{OK: false, Reason: rebaseFailedReason + err.Error()}
		case rv == RebaseConflict:
			return Outcome{OK: false, Reason: mergeConflictReason}
		}

		// Clean rebase + re-push. Confirm CI re-runs green on the rebased HEAD before
		// re-checking mergeability — the rebased commit could break on the new base,
		// and main may have moved again in the meantime.
		if err := d.AwaitHeadRun(); err != nil {
			return ciRerunErrOutcome(err, nil)
		}
		v, checks, err := d.Poll()
		switch {
		case errors.Is(err, ErrWedgedReadyForMergeQueue):
			// The rebased commit's real gates are green; only the merge-queue/main-only
			// wedge remains. Treat it as passed and re-check mergeability (BEH-614).
		case err != nil:
			return pollErrOutcome(err, checks)
		case v != Passed:
			return Outcome{OK: false, Reason: rebaseBrokeCIReason, Failing: FailedChecks(checks)}
		}
		// Loop: re-check mergeability on the rebased branch.
	}
}

// unobservableReason is the operator-facing summary when the token cannot read
// CI: the PR is open and gates passed host-side, so the run succeeds — a human
// just has to eyeball CI manually because the harness can't (BEH-476).
const unobservableReason = "CI status unobservable with this token — skipping watch/auto-fix; PR is open, check CI manually"

// unauthenticatedReason is the operator-facing summary when `gh pr checks` is
// rejected with a 401 / bad credentials even though push + PR create just succeeded
// with the harness auth (BEH-627). The diff is pushed and gate-green; only the poll
// path's token failed, so this fails soft to a green pass rather than a spurious
// "CI did not go green" — a human just eyeballs CI (and fixes the poll credentials).
const unauthenticatedReason = "CI watch unavailable: gh not authenticated for the checks poll (HTTP 401 / bad credentials) — PR was pushed OK, check CI manually and fix the harness gh auth"

// pollErrOutcome turns a poll error into an outcome. An unobservable-checks error
// is a *success* that degrades — the PR shipped, the token just can't read CI, so
// leave it for a human rather than abort or auto-fix. A timeout means CI never
// settled (report the still-non-green checks); any other error is a real gh
// failure surfaced verbatim.
func pollErrOutcome(err error, checks []Check) Outcome {
	if errors.Is(err, errChecksUnobservable) {
		return Outcome{OK: true, Reason: unobservableReason}
	}
	if errors.Is(err, errChecksUnauthenticated) {
		return Outcome{OK: true, Reason: unauthenticatedReason}
	}
	if errors.Is(err, ErrPollStalled) {
		return Outcome{
			OK:      false,
			Reason:  "CI stalled while still pending (no further progress within the stall window — a required check may be wedged, e.g. a merge-queue/main-only context that won't run on the PR, or a gate that hung); check the PR manually",
			Failing: notGreen(checks),
		}
	}
	if errors.Is(err, ErrPollTimeout) {
		return Outcome{
			OK:      false,
			Reason:  "CI never reached a terminal state (still pending past the poll budget)",
			Failing: notGreen(checks),
		}
	}
	return Outcome{OK: false, Reason: "polling CI failed: " + err.Error(), Failing: FailedChecks(checks)}
}

// ciRerunErrOutcome turns an AwaitHeadRun failure into an outcome. The fix was
// pushed, but the harness could not confirm CI re-ran for it: ErrCIRerunTimeout
// means CI never started a run for the new HEAD within the budget (don't keep
// guessing — leave the PR for a human to re-run); any other error is a real gh/git
// failure surfaced verbatim. Either way the PR + worktree are kept (BEH-493).
func ciRerunErrOutcome(err error, failed []Check) Outcome {
	if errors.Is(err, ErrCIRerunTimeout) {
		return Outcome{
			OK:      false,
			Reason:  "pushed the auto-fix but CI never started a new run for it within the budget — re-run CI / check the PR manually",
			Failing: failed,
		}
	}
	return Outcome{OK: false, Reason: "confirming CI re-ran for the fix failed: " + err.Error(), Failing: failed}
}

// notGreen returns every check that is not passing or skipping — i.e. the
// pending/failed/cancelled ones worth showing a human when CI is stuck.
func notGreen(checks []Check) []Check {
	var out []Check
	for _, c := range checks {
		if c.Bucket != BucketPass && c.Bucket != BucketSkipping {
			out = append(out, c)
		}
	}
	return out
}
