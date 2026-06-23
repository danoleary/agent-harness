package pipeline

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/stages"
)

// recorder captures the order stages ran in and the narration emitted.
type recorder struct {
	order  []string
	events []string
}

func (r *recorder) Event(msg string) { r.events = append(r.events, msg) }

// stage returns a Stage that records it ran and yields the given Result.
func (r *recorder) stage(name string, res stages.Result) Stage {
	return func() stages.Result {
		r.order = append(r.order, name)
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
	code := Run(Deps{
		FetchMain:      func() error { fetched++; return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code != 0 {
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
	code := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: false}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if want := []string{"impl", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v (review must be skipped, retro must still run)", r.order, want)
	}
}

func TestRunReviewFailureStillRunsRetroAndFails(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: false}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1", code)
	}
	if want := []string{"impl", "review", "retro"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("stage order = %v, want %v", r.order, want)
	}
}

func TestRunRetrospectiveFailureFailsTheRun(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: false}),
		Log:            r,
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (a failed retrospective fails the run)", code)
	}
}

func TestRunNarratesStageHardError(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		FetchMain:      func() error { return nil },
		Implementation: r.stage("impl", stages.Result{Err: errors.New("linear fetch failed")}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code != 1 {
		t.Errorf("exit code = %d, want 1 (a stage hard error fails the run)", code)
	}
	if !r.saw("linear fetch failed") {
		t.Errorf("expected the stage's hard error to be narrated to the runlog; events = %v", r.events)
	}
}

func TestRunFetchMainFailureIsWarnOnlyAndStagesStillRun(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		FetchMain:      func() error { return errors.New("network down") },
		Implementation: r.stage("impl", stages.Result{OK: true}),
		Review:         r.stage("review", stages.Result{OK: true}),
		Retrospective:  r.stage("retro", stages.Result{OK: true}),
		Log:            r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a fetch failure must not abort the pipeline)", code)
	}
	if len(r.order) != 3 {
		t.Errorf("stages run = %v, want all three to run despite the fetch failure", r.order)
	}
	if !r.saw("origin/main") {
		t.Errorf("expected a warning naming origin/main on fetch failure; events = %v", r.events)
	}
}
