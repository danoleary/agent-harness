package loop

import (
	"reflect"
	"strings"
	"testing"
	"time"
)

// recorder captures the order of injected calls and the narration emitted, so a
// test can assert WHAT the loop did and IN WHAT ORDER without Docker, Linear, or
// real signals — the same seam the pipeline tests use.
type recorder struct {
	order  []string
	events []string
}

func (r *recorder) Event(msg string) { r.events = append(r.events, msg) }

func (r *recorder) saw(substr string) bool {
	for _, e := range r.events {
		if strings.Contains(e, substr) {
			return true
		}
	}
	return false
}

// stopAfter returns a StopRequested predicate that reports "not yet" for the
// first n checks and "stop" thereafter, modelling a SIGINT/sentinel that lands
// after a fixed number of checkpoints.
func stopAfter(n int) func() bool {
	var calls int
	return func() bool {
		calls++
		return calls > n
	}
}

// TestStartupClearsStaleStopThenFetchesMain is the tracer bullet: before the loop
// body runs, startup must (1) clear any stale STOP sentinel left by a prior run,
// then (2) fetch+fast-forward origin/main — in that order. With stop already
// requested at the first checkpoint, the loop exits cleanly (code 0) without
// selecting a ticket.
func TestStartupClearsStaleStopThenFetchesMain(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		ClearStopFile: func() error { r.order = append(r.order, "clear-stop"); return nil },
		FetchMain:     func() error { r.order = append(r.order, "fetch-main"); return nil },
		StopRequested: func() bool { return true }, // stop already requested at the first checkpoint
		ResolveNext:   func() (string, bool) { r.order = append(r.order, "resolve"); return "", false },
		RunPipeline:   func(string) int { r.order = append(r.order, "pipeline"); return 0 },
		Sleep:         func(time.Duration) { r.order = append(r.order, "sleep") },
		PollInterval:  time.Minute,
		TickInterval:  2 * time.Second,
		Log:           r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a deliberate stop is a clean exit)", code)
	}
	if want := []string{"clear-stop", "fetch-main"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("startup order = %v, want %v (clear stale STOP, then fetch main; no ticket selected)", r.order, want)
	}
}

// TestEmptyQueueIdlesThenRePollsNotExit is the defining daemon behaviour: an
// empty queue must NOT exit (that's `pipeline --next`) — it idles for PollInterval
// then re-polls. Here selection returns empty on the first turn (→ idle + re-poll)
// and the test arranges a stop to land at the SECOND checkpoint so the loop
// terminates after one idle cycle, proving it looped rather than exited.
func TestEmptyQueueIdlesThenRePollsNotExit(t *testing.T) {
	r := &recorder{}
	var slept []time.Duration
	// The idle wait is broken into TickInterval chunks (see the tick-responsiveness
	// test); with no mid-idle stop, a full idle is PollInterval/TickInterval ticks.
	// 10s/2s = 5 ticks. Stop is arranged to land at the SECOND between-ticket
	// checkpoint, after one full idle cycle, proving the loop re-polled (didn't exit).
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stopAfter(6), // 1 checkpoint + 5 idle-tick checks pass, then stop at the next checkpoint
		ResolveNext:   func() (string, bool) { r.order = append(r.order, "resolve"); return "", false },
		RunPipeline:   func(string) int { r.order = append(r.order, "pipeline"); return 0 },
		Sleep:         func(d time.Duration) { r.order = append(r.order, "sleep"); slept = append(slept, d) },
		PollInterval:  10 * time.Second,
		TickInterval:  2 * time.Second,
		Log:           r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (an empty queue then stop is a clean exit)", code)
	}
	if r.order[0] != "resolve" {
		t.Errorf("first action = %q, want a resolve (select before idling)", r.order[0])
	}
	for _, step := range r.order {
		if step == "pipeline" {
			t.Errorf("RunPipeline ran on an empty queue; order = %v", r.order)
		}
	}
	// A full 10s idle in 2s ticks is 5 ticks; each tick is one TickInterval Sleep.
	if len(slept) != 5 {
		t.Errorf("idle slept %d ticks, want 5 (PollInterval/TickInterval = 10s/2s)", len(slept))
	}
	for i, d := range slept {
		if d != 2*time.Second {
			t.Errorf("idle tick %d slept %v, want one TickInterval (2s)", i, d)
		}
	}
	if !r.saw("queue empty") {
		t.Errorf("expected a 'queue empty — idling' narration; events = %v", r.events)
	}
}

// TestTicketRunsPipelineThenFetchesMainAndContinues proves the happy path: a
// selected ticket is run through RunPipeline (with its identifier), origin/main
// is fetched again after the session ("pull main after every session"), and the
// loop CONTINUES to the next turn rather than exiting — where the arranged stop
// lands. The startup fetch plus the after-ticket fetch is two fetches total.
func TestTicketRunsPipelineThenFetchesMainAndContinues(t *testing.T) {
	r := &recorder{}
	var ranWith string
	var fetches int
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { fetches++; r.order = append(r.order, "fetch"); return nil },
		StopRequested: stopAfter(1), // run one ticket, then stop at the 2nd checkpoint
		ResolveNext:   func() (string, bool) { r.order = append(r.order, "resolve"); return "BEH-100", true },
		RunPipeline: func(id string) int {
			r.order = append(r.order, "pipeline")
			ranWith = id
			return 0
		},
		Sleep:        func(time.Duration) { r.order = append(r.order, "sleep") },
		PollInterval: time.Minute,
		TickInterval: 2 * time.Second,
		Log:          r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a ticket then a stop is a clean exit)", code)
	}
	if ranWith != "BEH-100" {
		t.Errorf("RunPipeline ran with %q, want the selected ticket %q", ranWith, "BEH-100")
	}
	// startup fetch, then resolve → pipeline → after-ticket fetch, then stop.
	if want := []string{"fetch", "resolve", "pipeline", "fetch"}; !reflect.DeepEqual(r.order, want) {
		t.Errorf("order = %v, want %v (startup fetch, then select→run→fetch, then loop continues to the stop)", r.order, want)
	}
	if fetches != 2 {
		t.Errorf("FetchMain called %d times, want 2 (startup + after the ticket)", fetches)
	}
	if r.saw("queue empty") {
		t.Errorf("a non-empty queue must not narrate 'queue empty'; events = %v", r.events)
	}
}

// TestGracefulStopBetweenTicketsRunsTwoThenStops proves the stop is a per-ticket
// checkpoint: with a stop arranged to land at the THIRD checkpoint, the loop runs
// two whole tickets and stops cleanly between the second and a (never-attempted)
// third — never mid-ticket, and the stop narration fires exactly once.
func TestGracefulStopBetweenTicketsRunsTwoThenStops(t *testing.T) {
	r := &recorder{}
	var ran int
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stopAfter(2), // two tickets, stop at the 3rd checkpoint
		ResolveNext:   func() (string, bool) { return "BEH-1", true },
		RunPipeline:   func(string) int { ran++; return 0 },
		Sleep:         func(time.Duration) {},
		PollInterval:  time.Minute,
		TickInterval:  2 * time.Second,
		Log:           r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if ran != 2 {
		t.Errorf("RunPipeline ran %d tickets, want 2 (stop lands between the 2nd and 3rd, never mid-ticket)", ran)
	}
	if !r.saw("stop requested") {
		t.Errorf("expected a 'stop requested' wind-down narration; events = %v", r.events)
	}
}

// TestIdleSleepIsBrokenIntoTicksAndStopsEarly proves the idle wait is responsive:
// instead of one opaque PollInterval sleep, the loop sleeps in TickInterval chunks
// and re-checks stop between them, so a stop landing mid-idle is observed within
// one tick — not a whole poll interval later. With PollInterval=10*Tick and a stop
// arranged to land after a few idle ticks, the loop must wake early: far fewer than
// the 10 ticks a full idle would take.
func TestIdleSleepIsBrokenIntoTicksAndStopsEarly(t *testing.T) {
	r := &recorder{}
	tick := 10 * time.Millisecond
	poll := 10 * tick // a full idle would be 10 ticks

	// Stop is checked at the top of each loop turn (the between-ticket checkpoint)
	// AND on every idle tick. Let the first checkpoint pass (so we enter the idle),
	// then report stop once we've slept a few ticks — modelling a SIGINT mid-idle.
	var stopChecks int
	stop := func() bool {
		stopChecks++
		return stopChecks > 4 // pass the 1st (checkpoint) + a few idle-tick checks, then stop
	}

	var ticks int
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stop,
		ResolveNext:   func() (string, bool) { return "", false }, // always empty → idle
		RunPipeline:   func(string) int { t.Fatal("RunPipeline must not run on an empty queue"); return 0 },
		Sleep:         func(d time.Duration) { ticks++ },
		PollInterval:  poll,
		TickInterval:  tick,
		Log:           r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	// Each Sleep call is one tick. Stop is observed after a few idle ticks, so the
	// loop must wake well before the 10 ticks a full PollInterval idle would take.
	if ticks == 0 {
		t.Errorf("idle slept 0 ticks, want a few (the idle must actually wait in TickInterval chunks)")
	}
	if ticks >= 10 {
		t.Errorf("idle slept %d ticks, want fewer than a full %d-tick poll (a mid-idle stop must wake early)", ticks, 10)
	}
}
