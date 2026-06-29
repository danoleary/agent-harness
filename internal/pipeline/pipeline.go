// Package pipeline orchestrates the single-ticket chain — implementation →
// review → retrospective — over one hand-passed ticket, then exits. It has no
// ticket selection, stop control, or circuit breaker; those belong to the loop
// that will later wrap it (DESIGN.md "The pipeline"). The three stages are
// injected as pre-bound thunks so the sequencing is unit-testable without Docker
// or Linear.
package pipeline

import (
	"fmt"

	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/stages"
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

// Outcome is the pipeline's typed result. It carries the process ExitCode (what
// the standalone cmd wrapper exits with) plus the two signals the autonomous loop's
// circuit breaker keys on — ReachedPushedPR (the ticket shipped) and
// SpendingCapAbort (a retry-after-reset control signal) — so the loop never has to
// infer "did this ticket ship?" from the opaque exit code (DESIGN.md §Circuit
// breaker).
type Outcome struct {
	// ExitCode is 0 iff every stage that ran succeeded, 1 otherwise.
	ExitCode int
	// ReachedPushedPR is true iff the review stage pushed the branch and opened a
	// PR — the breaker's success signal, decoupled from ExitCode (a PR can exist on
	// a non-zero exit, e.g. CI red after the auto-fix budget).
	ReachedPushedPR bool
	// SpendingCapAbort is true iff any stage was aborted by an external spending cap
	// before finishing — the breaker stays blind to it (retry after the cap resets).
	SpendingCapAbort bool
	// RecommendClose is true iff the review stage concluded the branch makes zero net
	// change and the ticket should be closed as a duplicate/superseded rather than
	// shipped (BEH-603). The loop keeps it In Progress for a human; breaker-neutral.
	RecommendClose bool
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
//
// It returns a typed Outcome (not a bare exit code) so the autonomous loop can read
// the breaker signals — did the ticket reach a pushed PR, was it cap-aborted —
// without parsing logs or inferring them from ExitCode.
func Run(d Deps) Outcome {
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
	exit := 1
	if ok {
		exit = 0
	}
	// Breaker signals, decoupled from the exit code: only review pushes, so
	// ReachedPushedPR comes from it alone; a cap abort anywhere in the chain is
	// retry-after-reset.
	return Outcome{
		ExitCode:         exit,
		ReachedPushedPR:  review.ReachedPushedPR,
		SpendingCapAbort: impl.SpendingCapAbort || review.SpendingCapAbort || retro.SpendingCapAbort,
		RecommendClose:   review.RecommendClose,
	}
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
