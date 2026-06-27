// Package pipeline orchestrates the single-ticket chain — implementation →
// review → retrospective — over one hand-passed ticket, then exits. It has no
// ticket selection, stop control, or circuit breaker; those belong to the loop
// that will later wrap it (DESIGN.md "The pipeline"). The three stages are
// injected as pre-bound thunks so the sequencing is unit-testable without Docker
// or Linear.
package pipeline

import (
	"fmt"

	"github.com/beherd/agent-harness/internal/stages"
)

// Narrator is the slice of *runlog.Logger the orchestration needs: one-line
// console narration mirrored to run.jsonl. An interface keeps Run testable.
type Narrator interface {
	Event(string)
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

// Run executes the pipeline and returns the process exit code: 0 iff every stage
// that *ran* succeeded, 1 otherwise. The order and skip rules (DESIGN.md):
//
//   - fetch + fast-forward origin/main once, at the top (warn-only on failure —
//     the stages still run; a stale main only risks a noisier merge-base check);
//   - run implementation; if it fails, skip review (nothing to review);
//   - run review only if implementation succeeded;
//   - run retrospective ALWAYS, even after a prior-stage failure — a failed slice
//     is exactly the run worth mining for findings (the one deliberate divergence
//     from the loop's "if not OK -> skip rest").
func Run(d Deps) int {
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

	ok := impl.OK && (!reviewRan || review.OK) && retro.OK
	if ok {
		return 0
	}
	return 1
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
