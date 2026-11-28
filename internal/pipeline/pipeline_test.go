package pipeline

import (
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/gittest"
	"github.com/danoleary/agent-harness/internal/hostio"
	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/runlog/runlogtest"
	"github.com/danoleary/agent-harness/internal/session"
	"github.com/danoleary/agent-harness/internal/stages"
	"github.com/danoleary/agent-harness/internal/ticket"
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
	if code := out.ExitCode(); code != 0 {
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
	if code := out.ExitCode(); code != 1 {
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
	if code := out.ExitCode(); code != 0 {
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
	if code := out.ExitCode(); code != 1 {
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
	if code := out.ExitCode(); code != 1 {
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
	if code := out.ExitCode(); code != 1 {
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
	if code := out.ExitCode(); code != 1 {
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
	if code := out.ExitCode(); code != 1 {
		t.Errorf("exit code = %d, want 1 (a stage hard error fails the run)", code)
	}
	if !r.saw("linear fetch failed") {
		t.Errorf("expected the stage's hard error to be narrated to the runlog; events = %v", r.events)
	}
}

// A review that failed hard (#444: one timed-out tracker read) left a finished
// handoff unreviewed and unpushed; the narration tells the operator so, and how
// to re-run review alone, instead of only the bare error.
func TestRunNarratesHowToRerunAReviewThatFailedHard(t *testing.T) {
	r := &recorder{}
	Run(Deps{
		Ticket:         "444",
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{Err: errors.New("read tcp: i/o timeout")}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if !r.saw("review hard error: read tcp: i/o timeout") {
		t.Errorf("expected the review's hard error narrated; events = %v", r.events)
	}
	if !r.saw("agent-harness review 444") {
		t.Errorf("expected the narration to name `agent-harness review 444`; events = %v", r.events)
	}
}

// Only review gets the re-run hint: an implementation hard error has no handoff.
func TestRunDoesNotOfferAReviewRerunForAnImplementationHardError(t *testing.T) {
	r := &recorder{}
	Run(Deps{
		Ticket:         "444",
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{Err: errors.New("fetch failed")}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if r.saw("agent-harness review") {
		t.Errorf("an implementation hard error offered a review re-run; events = %v", r.events)
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
	if code := out.ExitCode(); code != 0 {
		t.Errorf("exit code = %d, want 0 (a fetch failure must not abort the pipeline)", code)
	}
	if len(r.order) != 3 {
		t.Errorf("stages run = %v, want all three to run despite the fetch failure", r.order)
	}
	if !r.saw("origin/main") {
		t.Errorf("expected a warning naming origin/main on fetch failure; events = %v", r.events)
	}
}

// The review stage's Shipped disposition must propagate to the run even when the
// run as a whole fails — a PR that shipped but went CI-red after the auto-fix budget
// is exit-1 yet Shipped, the exact case the loop breaker must read as a success (no
// increment), NOT infer from the exit code.
func TestRunPropagatesShippedFromReviewEvenOnFailure(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: false, Disposition: stages.Shipped}), // PR shipped, CI red after budget
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if out.ExitCode() != 1 {
		t.Errorf("exit code = %d, want 1 (a not-OK review fails the run)", out.ExitCode())
	}
	if out.Disposition != stages.Shipped {
		t.Errorf("Disposition = %s, want shipped — the review pushed a PR even though the run failed", out.Disposition)
	}
}

// BEH-603: the review stage's RecommendClose disposition must propagate to the run
// so the loop closes the ticket as a duplicate/superseded rather than releasing it
// to Todo. The run is not-OK (nothing shipped) but it is not a failure to be
// re-attempted.
func TestRunPropagatesRecommendCloseFromReview(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: false, Disposition: stages.RecommendClose}), // zero-net-diff branch
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if out.Disposition != stages.RecommendClose {
		t.Errorf("Disposition = %s, want recommend-close (it must propagate from the review stage)", out.Disposition)
	}
}

// A spending-cap abort in ANY stage propagates to the run so the loop reads
// retry-after-reset and keeps the breaker blind to it.
func TestRunPropagatesSpendingCapAbortFromAnyStage(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false, Disposition: stages.CapAborted}), // capped before any PR
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if out.Disposition != stages.CapAborted {
		t.Errorf("Disposition = %s, want cap-aborted when a stage cap-aborted", out.Disposition)
	}
}

// #407: a cap that aborted implementation is still in force seconds later, so a
// retrospective launched into it dies the same way and mines nothing. The slice
// ends at implementation and the loop backs off until the reset; the capped
// transcript stays under the ticket's logs for the next run's retrospective.
func TestRunSkipsTheRetrospectiveAfterACapAbortedImplementation(t *testing.T) {
	r := &recorder{}
	reset := time.Date(2026, 11, 22, 3, 0, 0, 0, time.UTC)
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false, Disposition: stages.CapAborted, CapResetAt: reset}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if want := []string{"impl"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (a cap-aborted implementation ends the slice)", r.order, want)
	}
	if out.Disposition != stages.CapAborted || !out.CapResetAt.Equal(reset) {
		t.Errorf("out = %+v, want CapAborted resetting at %s", out, reset)
	}
	if !r.saw("skipping the rest of the slice, retrospective included (#407)") {
		t.Errorf("no cap-abort skip narration; events = %v", r.events)
	}
}

// The implementation stage's PreflightAborted (the Docker sandbox couldn't launch:
// full disk / daemon down) propagates to the run so the loop reclaims disk +
// backs off and the breaker stays blind to it. The abort is not Retryable, so the
// pipeline does NOT re-attempt implementation, and review is skipped (impl not OK).
func TestRunPropagatesPreflightAbortFromImplementation(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false, Disposition: stages.PreflightAborted}), // preflight refused before any work
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if out.Disposition != stages.PreflightAborted {
		t.Errorf("Disposition = %s, want preflight-aborted when the implementation stage's preflight aborted", out.Disposition)
	}
	// A preflight abort is not Retryable, so implementation runs once (no re-attempt) and
	// review is skipped (impl not OK); retrospective still runs.
	if want := []string{"impl", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (preflight abort: no impl re-attempt, review skipped, retro runs)", r.order, want)
	}
}

// The exact reset time a cap-aborting stage parsed from its abort message propagates to
// the run so the loop can back off until the cap clears rather than a fixed guess
// (BEH-708). It rides alongside the CapAborted disposition of whichever stage was capped.
func TestRunPropagatesSpendingCapResetTimeFromAbortingStage(t *testing.T) {
	r := &recorder{}
	reset := time.Date(2026, 7, 5, 8, 40, 0, 0, time.UTC)
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false, Disposition: stages.CapAborted, CapResetAt: reset}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if !out.CapResetAt.Equal(reset) {
		t.Errorf("CapResetAt = %s, want %s (the aborting stage's reset time)", out.CapResetAt, reset)
	}
}

// The fold's precedence, at the only place two stages can disagree: the review
// reaches a verdict and the retrospective is then cap-aborted. The review's verdict
// is the run's — a ship stays a ship (the loop must not release a shipped ticket)
// and a zero-net-diff branch stays a recommend-close (re-running it can only reach
// the same conclusion) — while a cap abort with no such verdict is the run's story,
// outranking the implementation's preflight abort exactly as the loop's branch order
// always has.
func TestRunFoldsTheDispositionInPrecedenceOrder(t *testing.T) {
	cases := []struct {
		name                string
		impl, review, retro stages.Disposition
		want                stages.Disposition
	}{
		{"a ship outranks a later cap abort", stages.NoPR, stages.Shipped, stages.CapAborted, stages.Shipped},
		{"a recommend-close outranks a later cap abort", stages.NoPR, stages.RecommendClose, stages.CapAborted, stages.RecommendClose},
		{"a cap abort outranks a preflight abort", stages.PreflightAborted, stages.NoPR, stages.CapAborted, stages.CapAborted},
		{"nothing to report is a no-PR run", stages.NoPR, stages.NoPR, stages.NoPR, stages.NoPR},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			r := &recorder{}
			out := Run(Deps{
				FetchMain:      func() error { return nil },
				Implementation: r.stage("impl", stages.Result{OK: true, Disposition: c.impl}),
				Review:         r.stage("review", stages.Result{OK: true, Disposition: c.review}),
				Retrospective:  r.stage("retro", stages.Result{OK: true, Disposition: c.retro}),
				Log:            r,
			})
			if out.Disposition != c.want {
				t.Errorf("Disposition = %s, want %s", out.Disposition, c.want)
			}
		})
	}
}

// A loop without the retro step leaves Retrospective nil: the run ends after
// review, and its verdict is the two Stages that ran.
func TestRunWithoutARetrospectiveEndsAfterReview(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true, Disposition: stages.Shipped}),
		Log:            r,
	})
	if want := []string{"impl", "review"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v", r.order, want)
	}
	if !out.OK || out.Disposition != stages.Shipped {
		t.Errorf("Run = %+v, want an OK shipped run", out)
	}
}

// ForTicket is the one composition root of a ticket's pipeline (#29): cmd/pipeline
// and the ticket loop both bind all three Stages to one Host through it, so the
// two can never wire a slice differently. Building it launches nothing.
func TestForTicketBindsEveryStageToOneSlice(t *testing.T) {
	log := runlogtest.New(t, "PROJ-1")
	d := ForTicket(config.Config{}, log, "run", stages.Args{Identifier: "PROJ-1"})
	if d.FetchMain == nil || d.Implementation == nil || d.Review == nil || d.Retrospective == nil || d.Vacate == nil {
		t.Fatalf("ForTicket must bind the fetch, all three Stages and the vacate (#481), got %+v", d)
	}
	if d.Log != log {
		t.Errorf("the pipeline narrates to the slice's own runlog, got %v", d.Log)
	}
	if d.Ticket != "PROJ-1" {
		t.Errorf("Ticket = %q, want the slice's identifier for the review re-run hint (#444)", d.Ticket)
	}
}

// interruptedBy143 is the Result of a Stage whose session a SIGTERM killed (exit
// 143) — the run cut short from outside, not a broken slice (#99).
func interruptedBy143(t *testing.T) stages.Result {
	t.Helper()
	res, ok := stages.InterruptedResult(session.Outcome{ExitCode: 143})
	if !ok {
		t.Fatal("exit 143 was not classified as interrupted")
	}
	return res
}

// A signal-killed implementation is not a failed one (#99): there is nothing to
// review, and a retrospective would only mine a stub transcript inside a process
// that is already going down. Neither runs, the implementation is not re-attempted,
// and the slice reports Interrupted so the loop leaves the claim alone.
func TestRunInterruptedImplementationLaunchesNeitherReviewNorRetrospective(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", interruptedBy143(t)),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if want := []string{"impl"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (no review, no retrospective after an interruption)", r.order, want)
	}
	if out.Disposition != stages.Interrupted {
		t.Errorf("disposition = %v, want %v", out.Disposition, stages.Interrupted)
	}
	if out.ExitCode() == 0 {
		t.Error("an interrupted slice exited 0; it did not do its job")
	}
	if !r.saw("implementation interrupted by signal 15") {
		t.Errorf("no 'interrupted by signal 15' narration; events = %v", r.events)
	}
	if r.saw("skipping review (nothing to review)") {
		t.Error("an interruption was narrated as an implementation failure")
	}
}

// The same holds for a review the signal cut short: the retrospective is skipped.
func TestRunInterruptedReviewSkipsRetrospective(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", interruptedBy143(t)),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if want := []string{"impl", "review"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v", r.order, want)
	}
	if out.Disposition != stages.Interrupted {
		t.Errorf("disposition = %v, want %v", out.Disposition, stages.Interrupted)
	}
}

// A retrospective interrupted after the review shipped does not undo the ship.
func TestRunInterruptedRetrospectiveKeepsTheShip(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true, Disposition: stages.Shipped}),
		Retrospective:  r.stage("retro", interruptedBy143(t)),
		Log:            r,
	})
	if out.Disposition != stages.Shipped {
		t.Errorf("disposition = %v, want %v", out.Disposition, stages.Shipped)
	}
}

// A retrospective interrupted after a failed implementation does not turn the
// failure into an interruption: the slice is still NoPR, so the loop releases the
// claim and the breaker counts it.
func TestRunInterruptedRetrospectiveKeepsTheFailure(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", interruptedBy143(t)),
		Log:            r,
	})
	if out.Disposition != stages.NoPR {
		t.Errorf("disposition = %v, want %v", out.Disposition, stages.NoPR)
	}
	if !r.saw("retrospective interrupted by signal 15") {
		t.Errorf("no retrospective interruption narration; events = %v", r.events)
	}
}

// A ticket held on a red base launched no session (#125), so the slice ends at
// implementation: no review, no retrospective over a transcript that does not exist,
// and the BaseRed disposition reaches the loop with the base it held on and the
// verdict's expiry, which the loop idles on (#190).
func TestRunEndsTheSliceAtARedBaseHold(t *testing.T) {
	r := &recorder{}
	until := time.Date(2026, 11, 6, 14, 32, 0, 0, time.UTC)
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false, Disposition: stages.BaseRed, HeldBase: "3f9c2e7a", HeldUntil: until}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if out.Disposition != stages.BaseRed {
		t.Errorf("Disposition = %s, want base-red", out.Disposition)
	}
	if out.HeldBase != "3f9c2e7a" || !out.HeldUntil.Equal(until) {
		t.Errorf("held on %q until %v, want the implementation stage's 3f9c2e7a until %v", out.HeldBase, out.HeldUntil, until)
	}
	if want := []string{"impl"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (a red-base hold ends the slice)", r.order, want)
	}
}

// #331: an implementation that skipped the ticket because a commit on main already
// names it launched no session: review would fail over a worktree that was never
// made and the retrospective would mine nothing, so the slice ends there. It exits
// 0 and carries AlreadyOnMain and the commit to the loop.
func TestRunEndsTheSliceAtATicketAlreadyMergedOnMain(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true, Disposition: stages.AlreadyOnMain, ResolvedBy: "abc123", ResolvedWhy: "PROJ-4: the fix"}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if want := []string{"impl"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (no session ran: no review, no retrospective)", r.order, want)
	}
	if out.ExitCode() != 0 || out.Disposition != stages.AlreadyOnMain || out.ResolvedBy != "abc123" {
		t.Errorf("out = %+v, want an exit-0 AlreadyOnMain carrying the commit", out)
	}
}

// #329: an implementation that found the ticket already resolved on main handed
// nothing off, so review is skipped, but the slice did its job: it exits 0 and
// carries ResolvedOnMain, with the commit and reason, to the loop.
func TestRunImplementationResolvedOnMainSkipsReviewAndIsNoFailure(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{Disposition: stages.ResolvedOnMain, ResolvedBy: "abc123", ResolvedWhy: "PROJ-99 fixed it"}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if want := []string{"impl", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (nothing to review; retro still runs)", r.order, want)
	}
	if out.ExitCode() != 0 || out.Disposition != stages.ResolvedOnMain || out.ResolvedBy != "abc123" || out.ResolvedWhy != "PROJ-99 fixed it" {
		t.Errorf("out = %+v, want an exit-0 ResolvedOnMain carrying the commit and reason", out)
	}
	if r.saw("implementation failed") || !r.saw("already resolved on main") {
		t.Errorf("events = %v, want the resolved stop narrated, not a failure", r.events)
	}
}

// #311: the retrospective learns the base its run's implementation stage found
// green, and a re-attempt's verdict replaces the first attempt's.
func TestRetrospectiveGetsTheGreenBaseImplementationFound(t *testing.T) {
	r := &recorder{}
	var got []string
	impl, retro := withGreenBase(
		r.stageSeq("impl",
			stages.Result{Retryable: true, GreenBase: "aaa"},
			stages.Result{OK: true, GreenBase: "bbb"},
		),
		func(greenBase string) stages.Result {
			got = append(got, greenBase)
			return stages.Result{OK: true}
		},
	)

	Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: impl,
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  retro,
		Log:            r,
	})

	if len(got) != 1 || got[0] != "bbb" {
		t.Errorf("retrospective got green bases %v, want the re-attempt's [bbb]", got)
	}
}

// #368: an implementation whose session stopped blocked on a human handed
// nothing off, so review must not run, even over a branch the session committed
// to. The slice did its job: it exits 0 and carries SessionBlocked, with the
// reason and questions, to the loop.
func TestRunImplementationSessionBlockedSkipsReviewAndIsNoFailure(t *testing.T) {
	r := &recorder{}
	out := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{Disposition: stages.SessionBlocked, BlockedWhy: "premise is false", BlockedQuestions: []string{"keep it?"}}),
		Review:         r.stage("review", stages.Result{OK: true, Disposition: stages.Shipped}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if want := []string{"impl", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (nothing handed off to review; retro still runs)", r.order, want)
	}
	if out.ExitCode() != 0 || out.Disposition != stages.SessionBlocked || out.BlockedWhy != "premise is false" || !reflect.DeepEqual(out.BlockedQuestions, []string{"keep it?"}) {
		t.Errorf("out = %+v, want an exit-0 SessionBlocked carrying the reason and questions", out)
	}
	if r.saw("implementation failed") || !r.saw("blocked on a human") {
		t.Errorf("events = %v, want the block narrated, not a failure", r.events)
	}
}

// #481: the ticket run occupies its worktree while its sessions run (ADR-0076),
// so a run that ends with the worktree kept must not leave it locked: not after
// an early return, not without a retrospective, not after a reap the
// retrospective refused. Each run binds a real Host over a temp repo, as
// ForTicket binds the real one.
func TestRunLeavesNoOccupancyLockOnAKeptWorktree(t *testing.T) {
	for _, tc := range []struct {
		name    string
		session func(h *hostio.Fake, repo *gittest.Repo) func(int, hostio.AgentRun) session.Outcome
		retro   bool
	}{
		{"an interrupted implementation", func(*hostio.Fake, *gittest.Repo) func(int, hostio.AgentRun) session.Outcome {
			return func(int, hostio.AgentRun) session.Outcome { return session.Outcome{ExitCode: 143} }
		}, true},
		{"a loop without the retro step", handsOff, false},
		{"a retrospective that keeps the unshipped worktree", handsOff, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			repo := gittest.New(t)
			h := hostio.NewFake(repo.Checkout(), ticket.Slug("PROJ-1"))
			h.AgentFn = tc.session(h, repo)
			// No push reaches a PR, so a retrospective keeps the worktree.
			h.PushErr, h.ForcePushErr, h.PRThere = errors.New("push refused"), errors.New("push refused"), false
			log := runlogtest.New(t, "PROJ-1")
			d := forHost(h, sliceCfg(), log, stages.Args{Identifier: "PROJ-1"})
			if !tc.retro {
				d.Retrospective = nil
			}
			var occupied bool
			impl := d.Implementation
			d.Implementation = func() stages.Result {
				res := impl()
				occupied = repo.Occupied(h.Worktree())
				return res
			}

			Run(d)

			if !occupied {
				t.Fatal("the run must occupy the worktree it provisioned while its sessions run")
			}
			if !h.Worktree().Exists() {
				t.Fatal("the run was meant to keep its worktree")
			}
			if repo.Occupied(h.Worktree()) {
				t.Error("a run that keeps its worktree must lift its occupancy lock when it ends")
			}
		})
	}
}

// handsOff is a /tdd session that commits its handoff.
func handsOff(h *hostio.Fake, repo *gittest.Repo) func(int, hostio.AgentRun) session.Outcome {
	return func(_ int, run hostio.AgentRun) session.Outcome {
		if run.Label == "implementation" {
			repo.Commit(h.Worktree(), "fix.txt", "the fix\n", gittest.HandoffSubject)
		}
		return session.Outcome{}
	}
}

// sliceCfg is a minimal Consumer config a slice runs every stage over.
func sliceCfg() config.Config {
	return config.Config{
		Host: config.Host{
			ProjectPath: "/fake/checkout", TddTimeout: 30 * time.Minute, ReviewTimeout: 30 * time.Minute,
			RetrospectiveTimeout: 20 * time.Minute, SessionIdleTimeout: 5 * time.Minute,
		},
		Project: config.Project{
			BranchPrefix: "feat", Image: "example-agent-harness:latest",
			Gates:   []config.Gate{{Name: "check", Command: "pnpm run check"}},
			Prompts: config.PromptBodies{Implement: "implement the ticket", Review: "review the worktree", Retro: "retrospect the run"},
		},
	}
}

// #481: Run lifts the run's occupancy lock on every return, the early ones
// included, and before the BEH-543 second attempt at implementation provisions
// again.
func TestRunVacatesTheWorktreeOnEveryReturn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		impl  []stages.Result
		retro bool
		want  []string
	}{
		{"a full slice", []stages.Result{{OK: true}}, true, []string{"impl", "review", "retro", "vacate"}},
		{"no retro step", []stages.Result{{OK: true}}, false, []string{"impl", "review", "vacate"}},
		{"a red base", []stages.Result{{Disposition: stages.BaseRed}}, true, []string{"impl", "vacate"}},
		{"already on main", []stages.Result{{Disposition: stages.AlreadyOnMain}}, true, []string{"impl", "vacate"}},
		{"a cap abort", []stages.Result{{Disposition: stages.CapAborted}}, true, []string{"impl", "vacate"}},
		{"an interruption", []stages.Result{interruptedBy143(t)}, true, []string{"impl", "vacate"}},
		{"a re-attempt", []stages.Result{{Retryable: true}, {OK: true}}, true, []string{"impl", "vacate", "impl", "review", "retro", "vacate"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &recorder{}
			d := Deps{
				FetchMain:      func() error { return nil },
				Implementation: r.stageSeq("impl", tc.impl...),
				Review:         r.stage("review", stages.Result{OK: true}),
				Retrospective:  r.stage("retro", stages.Result{OK: true}),
				Vacate:         func() { r.order = append(r.order, "vacate") },
				Log:            r,
			}
			if !tc.retro {
				d.Retrospective = nil
			}

			Run(d)

			if !reflect.DeepEqual(r.order, tc.want) {
				t.Errorf("order = %v, want %v", r.order, tc.want)
			}
		})
	}
}
