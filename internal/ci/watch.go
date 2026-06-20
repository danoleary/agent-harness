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
	// of each fix attempt, not the poll waits (each ≤ PollBudget), so the real
	// elapsed time of a watch can reach roughly Budget + PollBudget.
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
		return Outcome{OK: true, Reason: "CI is green"}
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
		return Outcome{OK: true, Reason: "CI green after re-running the failed checks (flake)"}
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
		v, checks, err = d.Poll()
		if err != nil {
			return pollErrOutcome(err, checks)
		}
		if v == Passed {
			return Outcome{OK: true, Reason: fmt.Sprintf("CI green after %d auto-fix attempt(s)", attempt)}
		}
		failed = FailedChecks(checks)
	}

	return Outcome{
		OK:      false,
		Reason:  fmt.Sprintf("CI still red after %d auto-fix attempt(s)", cfg.MaxFixAttempts),
		Failing: failed,
	}
}

// pollErrOutcome turns a poll error into a non-success outcome. A timeout means
// CI never settled (report the still-non-green checks); any other error is a real
// gh failure surfaced verbatim.
func pollErrOutcome(err error, checks []Check) Outcome {
	if errors.Is(err, ErrPollTimeout) {
		return Outcome{
			OK:      false,
			Reason:  "CI never reached a terminal state (still pending past the poll budget)",
			Failing: notGreen(checks),
		}
	}
	return Outcome{OK: false, Reason: "polling CI failed: " + err.Error(), Failing: FailedChecks(checks)}
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
