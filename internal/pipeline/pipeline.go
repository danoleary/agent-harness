// Package pipeline orchestrates the single-ticket chain — implementation →
// review → retrospective — over one hand-passed ticket, then exits. It has no
// ticket selection, stop control, or circuit breaker; those belong to the loop
// that will later wrap it (DESIGN.md "The pipeline"). The three stages are
// injected as pre-bound thunks so the sequencing is unit-testable without Docker
// or Linear.
package pipeline

import (
	"fmt"
	"time"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/stages"
)

// Narrator is the slice of *runlog.Logger the orchestration needs: one-line
// console narration mirrored to run.jsonl, plus Structured for events that also
// feed the global loop.jsonl the viewer tails (ADR-0005). An interface keeps Run
// testable.
type Narrator interface {
	Event(string)
	Structured(loopstream.Record)
}

// Stage is a pre-bound stage invocation — the caller captures cfg/log/runID/args
// so Run only has to decide ordering and read each Result's OK verdict.
type Stage func() stages.Result

// Deps are the orchestration's injectable dependencies: the once-at-the-top
// origin/main fast-forward, the three stages, and the narrator.
type Deps struct {
	FetchMain      func() error
	Implementation Stage
	Review         Stage
	Retrospective  Stage
	Log            Narrator
}

// Run executes the pipeline. The order and skip rules (DESIGN.md):
//
//   - fetch + fast-forward origin/main once, at the top (warn-only on failure —
//     the stages still run; a stale main only risks a noisier merge-base check);
//   - run implementation; if it fails, skip review (nothing to review);
//   - run review only if implementation succeeded;
//   - run retrospective ALWAYS, even after a prior-stage failure — a failed slice
//     is exactly the run worth mining for findings (the one deliberate divergence
//     from the loop's "if not OK -> skip rest").
//
// It returns the slice's folded [stages.Result] (not a bare exit code) so the
// autonomous loop can read the run's Disposition — did the ticket ship, was it
// cap-aborted — without parsing logs or inferring it from an exit code. OK is the
// whole slice's verdict, so Result.ExitCode is the process exit code the standalone
// wrapper exits with.
func Run(d Deps) stages.Result {
	// Fast-forward main once for the whole run: the three stages run seconds apart,
	// so main won't meaningfully move mid-pipeline (DESIGN.md). A fetch failure is
	// not fatal — keep going and let the stages' own checks surface any staleness.
	if err := d.FetchMain(); err != nil {
		d.Log.Event("pipeline … warning: could not fast-forward origin/main: " + err.Error())
	}

	impl := d.Implementation()
	narrateErr(d.Log, "implementation", impl)

	// Re-attempt the implementation stage once when the first attempt crashed
	// environmentally with nothing to salvage (no worktree, no commit — Retryable)
	// rather than running to completion and producing no diff. Such a crash lands
	// at the worktree-creation step's heavy host I/O and is often transient, so a
	// fresh attempt may get further; without this the whole slice is discarded with
	// no commit and no re-queue (BEH-543). A genuine empty-diff/verification failure
	// is not Retryable and is never re-attempted. Bounded to one extra attempt so a
	// persistently sick host (e.g. full disk) can't spin the slice.
	if !impl.OK && impl.Retryable {
		d.Log.Event("pipeline ↻ implementation crashed environmentally with no commit — re-attempting the stage once (BEH-543)")
		impl = d.Implementation()
		narrateErr(d.Log, "implementation", impl)
	}

	reviewRan := false
	var review stages.Result
	if impl.OK {
		review = d.Review()
		reviewRan = true
		narrateErr(d.Log, "review", review)
	} else {
		d.Log.Event("pipeline — implementation failed; skipping review (nothing to review). Retrospective still runs.")
	}

	// Retrospective runs unconditionally: its value is highest on a failed slice.
	retro := d.Retrospective()
	narrateErr(d.Log, "retrospective", retro)

	// One folded Result for the whole slice: OK iff every stage that *ran* did its
	// job, and one Disposition for the loop to act on.
	return stages.Result{
		OK:          impl.OK && (!reviewRan || review.OK) && retro.OK,
		Disposition: fold(impl, review, retro),
		CapResetAt:  firstResetTime(impl, review, retro),
	}
}

// fold reduces the three stage dispositions to the run's one, in strict
// precedence order. Only review can ship or recommend-close and only
// implementation can preflight-abort, so at most one stage carries any given
// value; the order is what decides between *different* stages disagreeing — a
// retrospective that cap-aborts after a review that already shipped (or already
// concluded the branch is a no-op) does not undo the review's verdict, while a cap
// abort anywhere else is the run's story. PreflightAborted sits last of the
// signals because a preflight abort skips review entirely, so it only ever
// competes with a retrospective abort, which the loop has always let win.
func fold(stagesInOrder ...stages.Result) stages.Disposition {
	for _, want := range []stages.Disposition{
		stages.Shipped, stages.RecommendClose, stages.CapAborted, stages.PreflightAborted,
	} {
		for _, s := range stagesInOrder {
			if s.Disposition == want {
				return want
			}
		}
	}
	return stages.NoPR
}

// firstResetTime returns the reset instant of the first stage (in run order) that
// carried one, so the loop's backoff can wait until the cap actually clears (BEH-708).
// Zero when no stage parsed a reset time — the loop then falls back to its fixed backoff.
func firstResetTime(stagesInOrder ...stages.Result) time.Time {
	for _, s := range stagesInOrder {
		if !s.CapResetAt.IsZero() {
			return s.CapResetAt
		}
	}
	return time.Time{}
}

// narrateErr surfaces a stage's hard setup/IO error (Linear fetch, Docker
// preflight, MoveToInProgress, …) to the runlog. The standalone cmd wrappers
// print such an Err to stderr before exiting; the pipeline only sequences on OK,
// so without this a hard failure would fold into "not OK" with no root cause in
// run.jsonl — the opposite of "per-stage verdicts logged loudly".
func narrateErr(log Narrator, stage string, res stages.Result) {
	if res.Err != nil {
		log.Event(fmt.Sprintf("pipeline — %s hard error: %v", stage, res.Err))
	}
}
