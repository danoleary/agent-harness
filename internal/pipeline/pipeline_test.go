package pipeline

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
	"github.com/beherd/agent-harness/internal/stages"
)

// recorder captures the order stages ran in and the narration emitted.
type recorder struct {
	order   []string
	events  []string
	records []loopstream.Record
}

func (r *recorder) Event(msg string) { r.events = append(r.events, msg) }

func (r *recorder) Structured(rec loopstream.Record) {
	r.events = append(r.events, rec.Message)
	r.records = append(r.records, rec)
}

// stage returns a Stage that records it ran and yields the given Result.
func (r *recorder) stage(name string, res stages.Result) Stage {
	return func() stages.Result {
		r.order = append(r.order, name)
		return res
	}
}

// stageSeq returns a Stage that records each run and yields the next Result in
// the sequence (clamping to the last once exhausted), so a test can model a
// stage that a re-attempt invokes more than once with a different outcome.
func (r *recorder) stageSeq(name string, results ...stages.Result) Stage {
	var calls int
	return func() stages.Result {
		r.order = append(r.order, name)
		res := results[calls]
		if calls < len(results)-1 {
			calls++
		}
		return res
	}
}

func (r *recorder) saw(event string) bool {
	for _, e := range r.events {
		if strings.Contains(e, event) {
			return true
		}
	}
	return false
}

func TestRunAllStagesSucceedExitsZeroInOrder(t *testing.T) {
	r := &recorder{}
	var fetched int
	out := Run(Deps{
		FetchMain:      func() error { fetched++; return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code := out.ExitCode; code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if want := []string{"impl", "review", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v", r.order, want)
	}
	if fetched != 1 {
		t.Errorf("FetchMain called %d times, want exactly 1", fetched)
	}
}

func TestRunImplementationFailureSkipsReviewButRunsRetro(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code := out.ExitCode; code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if want := []string{"impl", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (review must be skipped, retro must still run)", r.order, want)
	}
}

func TestRunRetriesImplementationOnEnvCrashThenSucceeds(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain: func() error { return nil },
		// First attempt crashed environmentally with no commit (no worktree) —
		// Retryable; the re-attempt lands on a healthy host and succeeds.
		Implementation: r.stageSeq("impl",
			stages.Result{OK: false, Retryable: true},
			stages.Result{OK: true},
		),
		Review:        r.stage("review", stages.Result{OK: true}),
		Retrospective: r.stage("retro", stages.Result{OK: true}),
		Log:           r,
	})
	if code := out.ExitCode; code != 0 {
		t.Errorf("exit code = %d, want 0 (a retried env-crash that then succeeds passes)", code)
	}
	if want := []string{"impl", "impl", "review", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (impl re-attempted, then review+retro run)", r.order, want)
	}
	if !r.saw("re-attempt") {
		t.Errorf("expected a narration that implementation was re-attempted; events = %v", r.events)
	}
}

func TestRunRetriesImplementationOnlyOnceThenGivesUp(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain: func() error { return nil },
		// Every attempt crashes environmentally (e.g. a persistently full disk).
		Implementation: r.stageSeq("impl",
			stages.Result{OK: false, Retryable: true},
		),
		Review:        r.stage("review", stages.Result{OK: true}),
		Retrospective: r.stage("retro", stages.Result{OK: true}),
		Log:           r,
	})
	if code := out.ExitCode; code != 1 {
		t.Errorf("exit code = %d, want 1 (both attempts failed)", code)
	}
	if want := []string{"impl", "impl", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (impl re-attempted exactly once, review skipped, retro runs)", r.order, want)
	}
}

func TestRunDoesNotRetryGenuineImplementationFailure(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain: func() error { return nil },
		// Ran to completion but produced no handoff diff — a real verification
		// failure, not an environmental crash. Must NOT be re-attempted.
		Implementation: r.stageSeq("impl",
			stages.Result{OK: false, Retryable: false},
			stages.Result{OK: true}, // would be consumed only on an (incorrect) retry
		),
		Review:        r.stage("review", stages.Result{OK: true}),
		Retrospective: r.stage("retro", stages.Result{OK: true}),
		Log:           r,
	})
	if code := out.ExitCode; code != 1 {
		t.Errorf("exit code = %d, want 1 (a non-retryable failure fails the run)", code)
	}
	if want := []string{"impl", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (impl runs once, no re-attempt, review skipped)", r.order, want)
	}
	if r.saw("re-attempt") {
		t.Errorf("a non-retryable failure must not narrate a re-attempt; events = %v", r.events)
	}
}

func TestRunReviewFailureStillRunsRetroAndFails(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: false}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code := out.ExitCode; code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if want := []string{"impl", "review", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v", r.order, want)
	}
}

func TestRunRetrospectiveFailureFailsTheRun(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: false}),
		Log:            r,
	})
	if code := out.ExitCode; code != 1 {
		t.Errorf("exit code = %d, want 1 (a failed retrospective fails the run)", code)
	}
}

func TestRunNarratesStageHardError(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{Err: errors.New("linear fetch failed")}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code := out.ExitCode; code != 1 {
		t.Errorf("exit code = %d, want 1 (a stage hard error fails the run)", code)
	}
	if !r.saw("linear fetch failed") {
		t.Errorf("expected the stage's hard error to be narrated to the runlog; events = %v", r.events)
	}
}

func TestRunFetchMainFailureIsWarnOnlyAndStagesStillRun(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return errors.New("network down") },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code := out.ExitCode; code != 0 {
		t.Errorf("exit code = %d, want 0 (a fetch failure must not abort the pipeline)", code)
	}
	if len(r.order) != 3 {
		t.Errorf("stages run = %v, want all three to run despite the fetch failure", r.order)
	}
	if !r.saw("origin/main") {
		t.Errorf("expected a warning naming origin/main on fetch failure; events = %v", r.events)
	}
}

// The review stage's ReachedPushedPR must propagate to the Outcome even when the
// run as a whole fails — a PR that shipped but went CI-red after the auto-fix budget
// is exit-1 yet "reached a pushed PR", the exact case the loop breaker must read as
// a success (no increment), NOT infer from the exit code.
func TestRunPropagatesReachedPushedPRFromReviewEvenOnFailure(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: false, ReachedPushedPR: true}), // PR shipped, CI red after budget
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if out.ExitCode != 1 {
		t.Errorf("exit code = %d, want 1 (a not-OK review fails the run)", out.ExitCode)
	}
	if !out.ReachedPushedPR {
		t.Error("Outcome.ReachedPushedPR must be true — the review pushed a PR even though the run failed")
	}
}

// BEH-603: the review stage's RecommendClose disposition must propagate to the
// Outcome so the loop keeps the ticket In Progress for a human to close rather than
// releasing it to Todo. The run is not-OK (nothing shipped) but it is not a failure
// to be re-attempted.
func TestRunPropagatesRecommendCloseFromReview(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: false, RecommendClose: true}), // zero-net-diff branch
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if !out.RecommendClose {
		t.Error("Outcome.RecommendClose must propagate from the review stage")
	}
	if out.ReachedPushedPR {
		t.Error("a recommend-close pushed nothing — ReachedPushedPR must be false")
	}
}

// A spending-cap abort in ANY stage propagates to the Outcome so the loop reads
// retry-after-reset and keeps the breaker blind to it.
func TestRunPropagatesSpendingCapAbortFromAnyStage(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false, SpendingCapAbort: true}), // capped before any PR
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if !out.SpendingCapAbort {
		t.Error("Outcome.SpendingCapAbort must be true when a stage cap-aborted")
	}
	if out.ReachedPushedPR {
		t.Error("a cap abort before review pushed nothing — ReachedPushedPR must be false")
	}
}

// The exact reset time a cap-aborting stage parsed from its abort message propagates to
// the Outcome so the loop can back off until the cap clears rather than a fixed guess
// (BEH-708). It rides alongside SpendingCapAbort from whichever stage was capped.
func TestRunPropagatesSpendingCapResetTimeFromAbortingStage(t *testing.T) {
	r := &recorder{}
	reset := time.Date(2026, 7, 5, 8, 40, 0, 0, time.UTC)
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false, SpendingCapAbort: true, SpendingCapResetTime: reset}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if !out.SpendingCapResetTime.Equal(reset) {
		t.Errorf("Outcome.SpendingCapResetTime = %s, want %s (the aborting stage's reset time)", out.SpendingCapResetTime, reset)
	}
}
