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
}

// Outcome is the terminal result of watching (and trying to fix) CI.
type Outcome struct {
	OK bool
	// Reason is a one-line human summary (green / fixed / why it gave up).
	Reason string
	// Failing carries the last failing checks when !OK, for the operator report.
	Failing []Check
}

// WatchAndFix polls CI for the just-pushed PR and, on failure, drives a bounded
// auto-fix loop: one flake re-run first (a flaky real-time spec shouldn't trigger
// a code edit), then up to MaxFixAttempts diagnose+fix+push+re-poll cycles within
// the wall-clock Budget. It returns OK on green (immediately, after the re-run, or
// after a fix) and non-OK — keeping the PR + worktree for a human — on a poll
// timeout, a driver error, or attempt/budget exhaustion. now is injected so the
// budget is deterministic in tests.
func WatchAndFix(d Driver, cfg Config, now func() time.Time) Outcome {
	deadline := now().Add(cfg.Budget)

	v, checks, err := d.Poll()
	if err != nil {
		return pollErrOutcome(err, checks)
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
		return pollErrOutcome(err, checks)
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
			return pollErrOutcome(err, checks)
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

// greenOutcome confirms the PR is also mergeable before declaring CI a pass.
// Green checks on a PR that conflicts with base are not shippable, so a
// MergeConflicting verdict turns the pass into a distinct non-OK outcome that is
// never auto-fixed (a merge conflict is a rebase problem, not a code edit). An
// unreadable or persistently-UNKNOWN merge state must not block an otherwise-green
// PR, so it degrades to a green pass — but with the conflict guard flagged as
// unrun (mergeUnconfirmedNote), mirroring the checks-unobservable degrade rather
// than passing silently.
func greenOutcome(d Driver, reason string) Outcome {
	switch mv, err := d.MergeState(); {
	case err == nil && mv == MergeConflicting:
		return Outcome{OK: false, Reason: mergeConflictReason}
	case err == nil && mv == MergeClean:
		return Outcome{OK: true, Reason: reason}
	default:
		// Indeterminate (persistent UNKNOWN) or unreadable merge state: never block
		// an otherwise-green PR, but flag that mergeability wasn't confirmed.
		return Outcome{OK: true, Reason: reason + mergeUnconfirmedNote}
	}
}

// unobservableReason is the operator-facing summary when the token cannot read
// CI: the PR is open and gates passed host-side, so the run succeeds — a human
// just has to eyeball CI manually because the harness can't (BEH-476).
const unobservableReason = "CI status unobservable with this token — skipping watch/auto-fix; PR is open, check CI manually"

// pollErrOutcome turns a poll error into an outcome. An unobservable-checks error
// is a *success* that degrades — the PR shipped, the token just can't read CI, so
// leave it for a human rather than abort or auto-fix. A timeout means CI never
// settled (report the still-non-green checks); any other error is a real gh
// failure surfaced verbatim.
func pollErrOutcome(err error, checks []Check) Outcome {
	if errors.Is(err, errChecksUnobservable) {
		return Outcome{OK: true, Reason: unobservableReason}
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
