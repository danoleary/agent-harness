package loop

import (
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// recorder captures the order of injected calls and the narration emitted, so a
// test can assert WHAT the loop did and IN WHAT ORDER without Docker, Linear, or
// real signals — the same seam the pipeline tests use.
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

// kindFor returns the structured kind recorded for the first record whose message
// contains substr, so a test can assert the loop tagged an event correctly.
func (r *recorder) kindFor(substr string) (loopstream.Kind, bool) {
	for _, rec := range r.records {
		if strings.Contains(rec.Message, substr) {
			return rec.Kind, true
		}
	}
	return "", false
}

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
		RunPipeline: func(string) TicketOutcome {
			r.order = append(r.order, "pipeline")
			return TicketOutcome{ReachedPushedPR: true}
		},
		Sleep:        func(time.Duration) { r.order = append(r.order, "sleep") },
		PollInterval: time.Minute,
		TickInterval: 2 * time.Second,
		Log:          r,
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
		RunPipeline: func(string) TicketOutcome {
			r.order = append(r.order, "pipeline")
			return TicketOutcome{ReachedPushedPR: true}
		},
		Sleep:        func(d time.Duration) { r.order = append(r.order, "sleep"); slept = append(slept, d) },
		PollInterval: 10 * time.Second,
		TickInterval: 2 * time.Second,
		Log:          r,
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
	// The idle narration is tagged for the viewer's global stream (ADR-0005).
	if k, ok := r.kindFor("queue empty"); !ok || k != loopstream.KindIdle {
		t.Errorf("idle event kind = %q (found=%v), want %q", k, ok, loopstream.KindIdle)
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
		RunPipeline: func(id string) TicketOutcome {
			r.order = append(r.order, "pipeline")
			ranWith = id
			return TicketOutcome{ReachedPushedPR: true}
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
		RunPipeline:   func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: true} },
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

// TestBreakerTripsAndExitsAfterThreeNoPRTickets proves the circuit breaker keys on
// "did the ticket reach a pushed PR?" — NOT the exit code — and trips the daemon
// down the graceful wind-down path (exit 0) after three consecutive no-PR tickets.
// No stop is ever requested here: the ONLY thing that ends this otherwise-infinite
// loop is the breaker. When all three failures share one ticket id (a poison
// top-of-queue ticket, re-selected after each crash), the loud report names it.
func TestBreakerTripsAndExitsAfterThreeNoPRTickets(t *testing.T) {
	r := &recorder{}
	var ran int
	var released []string
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          func() bool { return false }, // never stopped — only the breaker can end this
		ResolveNext:            func() (string, bool) { return "BEH-99", true },
		RunPipeline:            func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: false} },
		ReleaseTicket:          func(id string) error { released = append(released, id); return nil },
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 3,
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a breaker trip is a deliberate wind-down, same path as STOP)", code)
	}
	if ran != 3 {
		t.Errorf("RunPipeline ran %d tickets, want exactly 3 (the breaker must stop consuming the queue at the threshold)", ran)
	}
	// Each no-PR run releases the ticket back to Todo (BEH-590), even on the way to a
	// breaker trip — so a tripped daemon leaves the offender in Todo, not stranded.
	if want := []string{"BEH-99", "BEH-99", "BEH-99"}; !reflect.DeepEqual(released, want) {
		t.Errorf("released = %v, want %v (every no-PR run releases the ticket, including the trip-causing ones)", released, want)
	}
	if !r.saw("circuit breaker") {
		t.Errorf("expected a loud 'circuit breaker' wind-down report; events = %v", r.events)
	}
	if !r.saw("BEH-99") {
		t.Errorf("the trip report must name the repeat offender BEH-99; events = %v", r.events)
	}
	// The trip report and the no-PR releases are tagged for the viewer (ADR-0005).
	if k, ok := r.kindFor("circuit breaker"); !ok || k != loopstream.KindBreakerTrip {
		t.Errorf("breaker-trip event kind = %q (found=%v), want %q", k, ok, loopstream.KindBreakerTrip)
	}
	if k, ok := r.kindFor("produced no PR"); !ok || k != loopstream.KindTicketReleased {
		t.Errorf("no-PR release event kind = %q (found=%v), want %q", k, ok, loopstream.KindTicketReleased)
	}
}

// TestNoPRRunReleasesTicketBackToTodo is the tracer bullet for BEH-590: a run that
// ends WITHOUT reaching a pushed PR — and is NOT a spending-cap abort (OOM, sandbox
// crash, review crash, empty-diff verification failure) — must release the ticket
// back to Todo rather than strand it In Progress. The dispatch claimed it on select,
// so when the run dies without a PR the claim has to be undone or the board reads as
// "in flight" forever and the dispatch guard never re-grabs it. A single failure
// with a generous breaker threshold isolates the release from the breaker trip.
func TestNoPRRunReleasesTicketBackToTodo(t *testing.T) {
	r := &recorder{}
	var released []string
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stopAfter(1), // one ticket, then stop at the 2nd checkpoint
		ResolveNext:   func() (string, bool) { return "BEH-42", true },
		RunPipeline: func(string) TicketOutcome {
			return TicketOutcome{ReachedPushedPR: false} // ran, but no PR (not a cap abort)
		},
		ReleaseTicket:          func(id string) error { released = append(released, id); return nil },
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 3, // generous — one failure must not trip it
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a no-PR run then stop is a clean exit)", code)
	}
	if want := []string{"BEH-42"}; !reflect.DeepEqual(released, want) {
		t.Errorf("released = %v, want %v (a run that produced no PR must be released to Todo, not left In Progress)", released, want)
	}
}

// commentRec records the (identifier, body) of each comment posted, so a test can
// assert the release leaves a visible breadcrumb on the ticket.
type commentRec struct {
	ids    []string
	bodies []string
}

func (c *commentRec) post(id, body string) error {
	c.ids = append(c.ids, id)
	c.bodies = append(c.bodies, body)
	return nil
}

// TestNoPRRunCommentsOnRelease proves the release-on-no-PR path leaves a comment on
// the ticket (BEH-590 acceptance: "leave a comment noting the run died"), so a
// repeatedly-failing ticket is visible on the board rather than silently bouncing
// Todo↔In Progress forever. The comment names the ticket and explains it produced
// no PR.
func TestNoPRRunCommentsOnRelease(t *testing.T) {
	r := &recorder{}
	c := &commentRec{}
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          stopAfter(1),
		ResolveNext:            func() (string, bool) { return "BEH-42", true },
		RunPipeline:            func(string) TicketOutcome { return TicketOutcome{ReachedPushedPR: false} },
		ReleaseTicket:          func(string) error { return nil },
		CommentTicket:          c.post,
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 3,
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if want := []string{"BEH-42"}; !reflect.DeepEqual(c.ids, want) {
		t.Errorf("commented on %v, want %v (the no-PR release must leave a breadcrumb on the ticket)", c.ids, want)
	}
	if len(c.bodies) != 1 || !strings.Contains(c.bodies[0], "no PR") {
		t.Errorf("comment body = %q, want one mentioning 'no PR'", c.bodies)
	}
}

// TestCapAbortReleaseCommentsWithReason proves the spending-cap abort release also
// leaves a breadcrumb (BEH-590 acceptance: note the run died + why), distinct from
// the generic no-PR comment so the board shows a cap abort will auto-retry rather
// than reading as a plain failure. A stop arranged after one backoff cycle ends the
// loop.
func TestCapAbortReleaseCommentsWithReason(t *testing.T) {
	r := &recorder{}
	c := &commentRec{}
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stopAfter(6), // 1 top checkpoint + 5 backoff ticks, then stop
		ResolveNext:   func() (string, bool) { return "BEH-7", true },
		RunPipeline:   func(string) TicketOutcome { return TicketOutcome{SpendingCapAbort: true} },
		ReleaseTicket: func(string) error { return nil },
		CommentTicket: c.post,
		Sleep:         func(time.Duration) {},
		PollInterval:  time.Minute,
		TickInterval:  2 * time.Second,
		CapBackoff:    10 * time.Second,
		Log:           r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if want := []string{"BEH-7"}; !reflect.DeepEqual(c.ids, want) {
		t.Errorf("commented on %v, want %v (a cap-abort release must leave a breadcrumb)", c.ids, want)
	}
	if len(c.bodies) != 1 || !strings.Contains(c.bodies[0], "spending cap") {
		t.Errorf("comment body = %q, want one mentioning 'spending cap'", c.bodies)
	}
}

// TestSpendingCapAbortReleasesTicketBacksOffAndLeavesBreakerNeutral is the
// tracer bullet for the cap-abort control flow (DESIGN.md §Spending-cap abort
// backoff): a run that ends in a spending-cap abort (no PR) must (1) release the
// ticket back to Todo so it isn't stranded In Progress, (2) NOT trip the circuit
// breaker — proven here with the most aggressive possible threshold of 1, which a
// single counted failure would trip — and (3) enter a long interruptible backoff
// (slept in TickInterval chunks) before re-polling. A stop arranged to land after
// one full backoff cycle ends the otherwise-infinite loop.
func TestSpendingCapAbortReleasesTicketBacksOffAndLeavesBreakerNeutral(t *testing.T) {
	r := &recorder{}
	var released []string
	var ran int
	var slept []time.Duration
	// CapBackoff/TickInterval = 10s/2s = 5 ticks for a full, uninterrupted backoff.
	// One top checkpoint (pass) + 5 backoff-tick stop-checks pass, then stop at the
	// next top checkpoint: stopAfter(6).
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stopAfter(6),
		ResolveNext:   func() (string, bool) { return "BEH-7", true },
		RunPipeline: func(string) TicketOutcome {
			ran++
			return TicketOutcome{SpendingCapAbort: true}
		},
		ReleaseTicket: func(id string) error { released = append(released, id); return nil },
		Sleep:         func(d time.Duration) { slept = append(slept, d) },
		PollInterval:  time.Minute,
		TickInterval:  2 * time.Second,
		CapBackoff:    10 * time.Second,
		// Threshold 1: a single counted failure would trip immediately. It must NOT.
		MaxConsecutiveFailures: 1,
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a cap abort then stop is a clean exit)", code)
	}
	if ran != 1 {
		t.Errorf("RunPipeline ran %d times, want 1", ran)
	}
	if want := []string{"BEH-7"}; !reflect.DeepEqual(released, want) {
		t.Errorf("released = %v, want %v (a cap abort must release the ticket to Todo)", released, want)
	}
	if r.saw("circuit breaker") {
		t.Errorf("a cap abort must NOT trip the breaker (threshold 1); events = %v", r.events)
	}
	// All sleeps here are backoff ticks (the queue is never empty). A full 10s backoff
	// in 2s ticks is 5 ticks — proving the loop actually entered the backoff wait.
	if len(slept) != 5 {
		t.Errorf("backoff slept %d ticks, want 5 (CapBackoff/TickInterval = 10s/2s)", len(slept))
	}
	for i, d := range slept {
		if d != 2*time.Second {
			t.Errorf("backoff tick %d slept %v, want one TickInterval (2s)", i, d)
		}
	}
	if !r.saw("spending-cap") {
		t.Errorf("expected a 'spending-cap abort — backing off' narration; events = %v", r.events)
	}
}

// TestCapBackoffNarratesEntryAndWake proves the cap-abort backoff is observable
// (BEH-605): instead of one silent ~45-minute sleep, the loop narrates a structured
// KindCapBackoff record on entry (naming the backoff duration so a watcher sees how
// long the wait is) and another when the backoff elapses and it re-polls, so a long
// backoff is never indistinguishable from a dead daemon. No stop lands mid-backoff:
// the backoff elapses naturally (5 ticks for 10s/2s), then a stop at the next top
// checkpoint ends the loop.
func TestCapBackoffNarratesEntryAndWake(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stopAfter(6), // 1 top checkpoint + 5 backoff ticks elapse, then stop
		ResolveNext:   func() (string, bool) { return "BEH-7", true },
		RunPipeline:   func(string) TicketOutcome { return TicketOutcome{SpendingCapAbort: true} },
		ReleaseTicket: func(string) error { return nil },
		Sleep:         func(time.Duration) {},
		PollInterval:  time.Minute,
		TickInterval:  2 * time.Second,
		CapBackoff:    10 * time.Second,
		Log:           r,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	// Entry: a KindCapBackoff record announcing the wait and naming the duration.
	entryKind, ok := r.kindFor("backing off 10s")
	if !ok {
		t.Fatalf("no cap-backoff entry narration naming the duration; events = %v", r.events)
	}
	if entryKind != loopstream.KindCapBackoff {
		t.Errorf("entry kind = %q, want %q", entryKind, loopstream.KindCapBackoff)
	}
	// Wake: a KindCapBackoff record when the backoff elapses and the loop re-polls,
	// so each backoff cycle bookends cleanly in the log.
	wakeKind, ok := r.kindFor("backoff elapsed")
	if !ok {
		t.Fatalf("no cap-backoff wake narration on re-poll; events = %v", r.events)
	}
	if wakeKind != loopstream.KindCapBackoff {
		t.Errorf("wake kind = %q, want %q", wakeKind, loopstream.KindCapBackoff)
	}
}

// TestCapBackoffEmitsPeriodicHeartbeat proves the backoff narrates a periodic
// "still capped" heartbeat while it waits (BEH-605) so a watcher can see the daemon
// is alive and counting down — the whole point at the 45m default. With a 10s
// backoff ticking every 2s and a 4s heartbeat cadence, a heartbeat falls due twice
// (at ~4s and ~8s elapsed) before the wait ends; the final tick is suppressed so it
// doesn't double up with the wake record.
func TestCapBackoffEmitsPeriodicHeartbeat(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		ClearStopFile:       func() error { return nil },
		FetchMain:           func() error { return nil },
		StopRequested:       stopAfter(6), // 1 top checkpoint + 5 backoff ticks elapse, then stop
		ResolveNext:         func() (string, bool) { return "BEH-7", true },
		RunPipeline:         func(string) TicketOutcome { return TicketOutcome{SpendingCapAbort: true} },
		ReleaseTicket:       func(string) error { return nil },
		Sleep:               func(time.Duration) {},
		PollInterval:        time.Minute,
		TickInterval:        2 * time.Second,
		CapBackoff:          10 * time.Second,
		CapBackoffHeartbeat: 4 * time.Second,
		Log:                 r,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	var beats int
	for _, rec := range r.records {
		if rec.Kind == loopstream.KindCapBackoff && strings.Contains(rec.Message, "still capped") {
			beats++
		}
	}
	if beats < 2 {
		t.Fatalf("want >=2 cap-backoff heartbeat records during the wait, got %d; events = %v", beats, r.events)
	}
}

// TestCapBackoffEmitsNoHeartbeatWhenCadenceUnset proves the heartbeat is opt-in: with
// CapBackoffHeartbeat left zero, the wait still bookends (entry + wake) but emits no
// intermediate "still capped" spam — the contract the existing callers rely on.
func TestCapBackoffEmitsNoHeartbeatWhenCadenceUnset(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stopAfter(6),
		ResolveNext:   func() (string, bool) { return "BEH-7", true },
		RunPipeline:   func(string) TicketOutcome { return TicketOutcome{SpendingCapAbort: true} },
		ReleaseTicket: func(string) error { return nil },
		Sleep:         func(time.Duration) {},
		PollInterval:  time.Minute,
		TickInterval:  2 * time.Second,
		CapBackoff:    10 * time.Second,
		// CapBackoffHeartbeat unset (zero) → no intermediate heartbeats.
		Log: r,
	})
	if code != 0 {
		t.Fatalf("exit code = %d, want 0", code)
	}
	if r.saw("still capped") {
		t.Fatalf("a zero heartbeat cadence must emit no intermediate heartbeat; events = %v", r.events)
	}
}

// TestCapBackoffIsInterruptibleByStop proves the cap-abort backoff is responsive to
// STOP/SIGINT (same short-tick mechanism as the idle wait): instead of one opaque
// ~45-minute sleep, the backoff sleeps in TickInterval chunks and re-checks stop
// between them, so a stop landing mid-backoff is observed within one tick. With
// CapBackoff=10*Tick and a stop arranged to land after a few backoff ticks, the
// loop must wake well before the 10 ticks a full backoff would take.
func TestCapBackoffIsInterruptibleByStop(t *testing.T) {
	r := &recorder{}
	tick := 10 * time.Millisecond
	backoff := 10 * tick // a full backoff would be 10 ticks

	// Pass the first (top) checkpoint so we run the ticket and enter the backoff,
	// then report stop once we've slept a few backoff ticks — modelling a mid-backoff SIGINT.
	var stopChecks int
	stop := func() bool {
		stopChecks++
		return stopChecks > 4 // 1 top checkpoint + a few backoff-tick checks, then stop
	}

	var ticks int
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { return nil },
		StopRequested: stop,
		ResolveNext:   func() (string, bool) { return "BEH-7", true },
		RunPipeline:   func(string) TicketOutcome { return TicketOutcome{SpendingCapAbort: true} },
		ReleaseTicket: func(string) error { return nil },
		Sleep:         func(time.Duration) { ticks++ },
		PollInterval:  time.Minute,
		TickInterval:  tick,
		CapBackoff:    backoff,
		Log:           r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if ticks == 0 {
		t.Errorf("backoff slept 0 ticks, want a few (the backoff must actually wait in TickInterval chunks)")
	}
	if ticks >= 10 {
		t.Errorf("backoff slept %d ticks, want fewer than a full %d-tick backoff (a mid-backoff stop must wake early)", ticks, 10)
	}
	// A stop landing mid-backoff suppresses the wake record (the top-of-loop
	// wind-down narrates the exit instead) — so the backoff must NOT bookend.
	if r.saw("backoff elapsed") {
		t.Errorf("a stop mid-backoff must suppress the wake record, but a 'backoff elapsed' record was emitted; events = %v", r.events)
	}
}

// TestCapAbortThenNormalRunAutoResumes proves auto-resume (DESIGN.md §Spending-cap
// abort backoff): after a cap abort releases its ticket and the daemon backs off,
// the loop re-polls and a subsequent non-capped run behaves exactly as before — it
// runs the next ticket, fetches main after the session, and resets the breaker. The
// cap-aborted run skips the after-ticket fetch (no work shipped), so only startup +
// the normal run fetch main: two fetches total.
func TestCapAbortThenNormalRunAutoResumes(t *testing.T) {
	r := &recorder{}
	var released []string
	var ranWith []string
	var fetches int
	var run int
	code := Run(Deps{
		ClearStopFile: func() error { return nil },
		FetchMain:     func() error { fetches++; return nil },
		StopRequested: stopAfter(4), // top, 2 backoff ticks, top → run the 2nd ticket, then stop at the 3rd top checkpoint
		ResolveNext: func() (string, bool) {
			if run == 0 {
				return "BEH-A", true
			}
			return "BEH-B", true
		},
		RunPipeline: func(id string) TicketOutcome {
			run++
			ranWith = append(ranWith, id)
			if id == "BEH-A" {
				return TicketOutcome{SpendingCapAbort: true} // first run: capped
			}
			return TicketOutcome{ReachedPushedPR: true} // auto-resumed run: ships normally
		},
		ReleaseTicket:          func(id string) error { released = append(released, id); return nil },
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		CapBackoff:             4 * time.Second, // 2 ticks
		MaxConsecutiveFailures: 3,
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if want := []string{"BEH-A", "BEH-B"}; !reflect.DeepEqual(ranWith, want) {
		t.Errorf("ran tickets %v, want %v (cap abort, back off, then auto-resume the next ticket)", ranWith, want)
	}
	if want := []string{"BEH-A"}; !reflect.DeepEqual(released, want) {
		t.Errorf("released = %v, want %v (only the capped ticket is released; the shipped one is not)", released, want)
	}
	if fetches != 2 {
		t.Errorf("FetchMain called %d times, want 2 (startup + after the normal run; the cap-aborted run skips the fetch)", fetches)
	}
	if r.saw("circuit breaker") {
		t.Errorf("a cap abort then a shipped ticket must not trip the breaker; events = %v", r.events)
	}
}

// TestBreakerDoesNotTripWhenTicketsKeepShipping proves a healthy daemon never
// trips: a long run of pushed-PR tickets keeps the counter at 0, so the loop only
// ends on the arranged stop, not the breaker.
func TestBreakerDoesNotTripWhenTicketsKeepShipping(t *testing.T) {
	r := &recorder{}
	var ran int
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          stopAfter(5), // five shipped tickets, then stop
		ResolveNext:            func() (string, bool) { return "BEH-1", true },
		RunPipeline:            func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 3,
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if ran != 5 {
		t.Errorf("RunPipeline ran %d tickets, want 5 (shipping tickets never trip the breaker)", ran)
	}
	if r.saw("circuit breaker") {
		t.Errorf("a daemon shipping every ticket must NOT trip the breaker; events = %v", r.events)
	}
}

// TestMaxTicketsStopsCleanlyAfterCeiling proves the optional attempted-ticket
// ceiling: with MaxTickets=2 and no stop ever requested, the loop runs exactly two
// tickets then winds down on the clean stop path (exit 0), narrating the ceiling.
// It's the AFK safety valve — a non-zero ceiling bounds an overnight run.
func TestMaxTicketsStopsCleanlyAfterCeiling(t *testing.T) {
	r := &recorder{}
	var ran int
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          func() bool { return false }, // never stopped — only the ceiling ends this
		ResolveNext:            func() (string, bool) { return "BEH-1", true },
		RunPipeline:            func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 3,
		MaxTickets:             2,
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a ceiling is a deliberate wind-down, like STOP)", code)
	}
	if ran != 2 {
		t.Errorf("RunPipeline ran %d tickets, want exactly 2 (the ceiling must stop consuming the queue)", ran)
	}
	if !r.saw("max tickets") {
		t.Errorf("expected a 'max tickets reached' wind-down narration; events = %v", r.events)
	}
}

// TestMaxTicketsZeroIsUnlimited proves the default ceiling of 0 imposes no bound:
// with MaxTickets=0 the loop runs well past any small N and only ends on the
// arranged stop, exactly as the long-running daemon should.
func TestMaxTicketsZeroIsUnlimited(t *testing.T) {
	r := &recorder{}
	var ran int
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          stopAfter(5), // five tickets, then stop — not the ceiling
		ResolveNext:            func() (string, bool) { return "BEH-1", true },
		RunPipeline:            func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 10,
		MaxTickets:             0, // unlimited
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if ran != 5 {
		t.Errorf("RunPipeline ran %d tickets, want 5 (MaxTickets=0 imposes no ceiling)", ran)
	}
	if r.saw("max tickets") {
		t.Errorf("MaxTickets=0 must never narrate a ceiling; events = %v", r.events)
	}
}

// clockFrom returns a Now func that reports start on its first call and advances by
// step on each subsequent call — a deterministic fake clock so the runtime-ceiling
// test needs no real time.
func clockFrom(start time.Time, step time.Duration) func() time.Time {
	t := start
	first := true
	return func() time.Time {
		if first {
			first = false
			return t
		}
		t = t.Add(step)
		return t
	}
}

// TestMaxRuntimeStopsCleanlyAfterCeiling proves the optional wall-clock ceiling:
// with the clock advancing 1m per between-ticket check and MaxRuntime=90s, the loop
// runs one ticket (elapsed 1m < 90s) and on the next checkpoint (elapsed 2m ≥ 90s)
// winds down on the clean stop path (exit 0), narrating the ceiling. No stop is ever
// requested — only the runtime ceiling ends this.
func TestMaxRuntimeStopsCleanlyAfterCeiling(t *testing.T) {
	r := &recorder{}
	var ran int
	origin := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          func() bool { return false },
		ResolveNext:            func() (string, bool) { return "BEH-1", true },
		RunPipeline:            func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                  func(time.Duration) {},
		Now:                    clockFrom(origin, time.Minute),
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 10,
		MaxRuntime:             90 * time.Second,
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a runtime ceiling is a deliberate wind-down)", code)
	}
	if ran != 1 {
		t.Errorf("RunPipeline ran %d tickets, want 1 (elapsed crosses 90s before the 2nd ticket)", ran)
	}
	if !r.saw("max runtime") {
		t.Errorf("expected a 'max runtime reached' wind-down narration; events = %v", r.events)
	}
}

// TestMaxRuntimeZeroIsUnlimited proves the default of 0 imposes no wall-clock bound:
// even with the clock leaping an hour per check, the loop only ends on the arranged
// stop, never on runtime.
func TestMaxRuntimeZeroIsUnlimited(t *testing.T) {
	r := &recorder{}
	var ran int
	origin := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          stopAfter(3),
		ResolveNext:            func() (string, bool) { return "BEH-1", true },
		RunPipeline:            func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                  func(time.Duration) {},
		Now:                    clockFrom(origin, time.Hour), // leaps an hour per check
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 10,
		MaxRuntime:             0, // unlimited
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if ran != 3 {
		t.Errorf("RunPipeline ran %d tickets, want 3 (MaxRuntime=0 imposes no ceiling)", ran)
	}
	if r.saw("max runtime") {
		t.Errorf("MaxRuntime=0 must never narrate a ceiling; events = %v", r.events)
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
		RunPipeline: func(string) TicketOutcome {
			t.Fatal("RunPipeline must not run on an empty queue")
			return TicketOutcome{}
		},
		Sleep:        func(d time.Duration) { ticks++ },
		PollInterval: poll,
		TickInterval: tick,
		Log:          r,
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

// gib is a readable byte-count for the disk-reclaim tests.
const gib = 1 << 30

// freeDiskSeq returns a FreeDisk stub that yields the given free-byte readings in
// order, repeating the last value once exhausted — so a test can model the disk
// changing across the reclaim step's successive statfs calls (gate → after-prune →
// after-store-prune) without real I/O.
func freeDiskSeq(vals ...uint64) func() (uint64, error) {
	var i int
	return func() (uint64, error) {
		v := vals[i]
		if i < len(vals)-1 {
			i++
		}
		return v, nil
	}
}

// TestDiskReclaimPrunesBelowThresholdBeforeSelecting is the tracer bullet for
// ADR-0005: at the top of an iteration, when free disk is below the soft
// DiskReclaimThreshold, the loop prunes merged worktrees BEFORE selecting the next
// ticket. Here the first statfs reads below the threshold (→ prune) and the
// re-check after the prune reads above it (→ no secondary store prune). Prune must
// run, and it must run before ResolveNext.
func TestDiskReclaimPrunesBelowThresholdBeforeSelecting(t *testing.T) {
	r := &recorder{}
	var storePruned bool
	code := Run(Deps{
		ClearStopFile:        func() error { return nil },
		FetchMain:            func() error { return nil },
		StopRequested:        stopAfter(1), // one iteration, then stop
		DiskReclaimThreshold: 8 * gib,
		FreeDisk:             freeDiskSeq(1*gib, 9*gib), // below at the gate, healthy after the prune
		PruneMergedWorktrees: func() (int, error) { r.order = append(r.order, "prune"); return 2, nil },
		StorePrune:           func() error { storePruned = true; return nil },
		ResolveNext:          func() (string, bool) { r.order = append(r.order, "resolve"); return "", false },
		RunPipeline:          func(string) TicketOutcome { return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                func(time.Duration) {},
		PollInterval:         time.Minute,
		TickInterval:         2 * time.Second,
		Log:                  r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	pruneIdx, resolveIdx := indexOf(r.order, "prune"), indexOf(r.order, "resolve")
	if pruneIdx < 0 {
		t.Fatalf("PruneMergedWorktrees never ran below the threshold; order = %v", r.order)
	}
	if resolveIdx < 0 || pruneIdx > resolveIdx {
		t.Errorf("prune must run before ticket selection; order = %v", r.order)
	}
	if storePruned {
		t.Errorf("store prune must NOT run when the worktree prune already brought free disk above the threshold")
	}
	if !r.saw("reclaimed 2 merged worktree(s)") {
		t.Errorf("expected a 'reclaimed 2 merged worktree(s)' narration; events = %v", r.events)
	}
}

// indexOf returns the first index of want in xs, or -1.
func indexOf(xs []string, want string) int {
	for i, x := range xs {
		if x == want {
			return i
		}
	}
	return -1
}

// TestDiskReclaimSilentAtOrAboveThreshold proves the statfs gate: when free disk is
// at or above the threshold the loop makes NO prune / store-prune calls and emits no
// reclaim narration — disk pressure self-heals only when there actually is pressure.
func TestDiskReclaimSilentAtOrAboveThreshold(t *testing.T) {
	r := &recorder{}
	var pruned, storePruned bool
	var statfsCalls int
	code := Run(Deps{
		ClearStopFile:        func() error { return nil },
		FetchMain:            func() error { return nil },
		StopRequested:        stopAfter(1),
		DiskReclaimThreshold: 8 * gib,
		FreeDisk:             func() (uint64, error) { statfsCalls++; return 8 * gib, nil }, // exactly at the floor — healthy
		PruneMergedWorktrees: func() (int, error) { pruned = true; return 0, nil },
		StorePrune:           func() error { storePruned = true; return nil },
		ResolveNext:          func() (string, bool) { return "", false },
		RunPipeline:          func(string) TicketOutcome { return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                func(time.Duration) {},
		PollInterval:         time.Minute,
		TickInterval:         2 * time.Second,
		Log:                  r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if pruned {
		t.Errorf("PruneMergedWorktrees ran with free disk at the threshold — the statfs gate must suppress all shell-out when disk is healthy")
	}
	if storePruned {
		t.Errorf("StorePrune ran with free disk at the threshold")
	}
	if statfsCalls != 1 {
		t.Errorf("FreeDisk called %d times, want exactly 1 (the cheap gate check, nothing more) at/above the threshold", statfsCalls)
	}
	if r.saw("reclaimed") {
		t.Errorf("a healthy-disk iteration must be silent; events = %v", r.events)
	}
}

// TestDiskReclaimZeroThresholdDisables proves the 0-disables contract: with the
// threshold zeroed, the loop never even statfs's — reclaim is fully off.
func TestDiskReclaimZeroThresholdDisables(t *testing.T) {
	r := &recorder{}
	var statfsCalls int
	code := Run(Deps{
		ClearStopFile:        func() error { return nil },
		FetchMain:            func() error { return nil },
		StopRequested:        stopAfter(1),
		DiskReclaimThreshold: 0, // disabled
		FreeDisk:             func() (uint64, error) { statfsCalls++; return 0, nil },
		PruneMergedWorktrees: func() (int, error) { t.Fatal("prune must not run when reclaim is disabled"); return 0, nil },
		ResolveNext:          func() (string, bool) { return "", false },
		RunPipeline:          func(string) TicketOutcome { return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                func(time.Duration) {},
		PollInterval:         time.Minute,
		TickInterval:         2 * time.Second,
		Log:                  r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	if statfsCalls != 0 {
		t.Errorf("FreeDisk called %d times, want 0 (a zero threshold disables reclaim entirely — not even a statfs)", statfsCalls)
	}
}

// TestDiskReclaimRunsStorePruneWhenStillBelowAfterWorktreePrune proves the secondary
// reclaim is conditional: when the worktree prune leaves free disk STILL below the
// threshold, the loop runs `pnpm store prune` as a cheap follow-up.
func TestDiskReclaimRunsStorePruneWhenStillBelowAfterWorktreePrune(t *testing.T) {
	r := &recorder{}
	code := Run(Deps{
		ClearStopFile:        func() error { return nil },
		FetchMain:            func() error { return nil },
		StopRequested:        stopAfter(1),
		DiskReclaimThreshold: 8 * gib,
		// Below at the gate, and STILL below after the worktree prune → store prune runs.
		FreeDisk:             freeDiskSeq(1*gib, 2*gib, 3*gib),
		PruneMergedWorktrees: func() (int, error) { r.order = append(r.order, "prune"); return 1, nil },
		StorePrune:           func() error { r.order = append(r.order, "store-prune"); return nil },
		ResolveNext:          func() (string, bool) { return "", false },
		RunPipeline:          func(string) TicketOutcome { return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                func(time.Duration) {},
		PollInterval:         time.Minute,
		TickInterval:         2 * time.Second,
		Log:                  r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0", code)
	}
	pruneIdx, storeIdx := indexOf(r.order, "prune"), indexOf(r.order, "store-prune")
	if storeIdx < 0 {
		t.Fatalf("StorePrune never ran though free disk stayed below the threshold after the worktree prune; order = %v", r.order)
	}
	if pruneIdx < 0 || pruneIdx > storeIdx {
		t.Errorf("store prune must run AFTER the worktree prune; order = %v", r.order)
	}
}

// stubErr is a sentinel error for the prune-failure test.
type stubErr string

func (e stubErr) Error() string { return string(e) }

// TestDiskReclaimPruneFailureIsSwallowedAndBreakerUntouched proves reclaim is never a
// ticket outcome (ADR-0005): a failing prune is narrated as a warning, the loop keeps
// running the queue, and the circuit breaker is never touched. With the most
// aggressive threshold of 1, a reclaim failure that wrongly counted as a ticket
// failure would trip the breaker after a single iteration — it must not. Two
// successful tickets run despite the prune failing on every iteration; the breaker
// (which keys only on "reached a pushed PR?") stays reset and the loop never trips.
func TestDiskReclaimPruneFailureIsSwallowedAndBreakerUntouched(t *testing.T) {
	r := &recorder{}
	var ran int
	code := Run(Deps{
		ClearStopFile:          func() error { return nil },
		FetchMain:              func() error { return nil },
		StopRequested:          stopAfter(2), // two tickets, then stop at the 3rd checkpoint
		DiskReclaimThreshold:   8 * gib,
		FreeDisk:               func() (uint64, error) { return 1 * gib, nil }, // always under pressure
		PruneMergedWorktrees:   func() (int, error) { return 0, stubErr("gh unreachable") },
		StorePrune:             func() error { return nil },
		ResolveNext:            func() (string, bool) { return "BEH-1", true },
		RunPipeline:            func(string) TicketOutcome { ran++; return TicketOutcome{ReachedPushedPR: true} },
		Sleep:                  func(time.Duration) {},
		PollInterval:           time.Minute,
		TickInterval:           2 * time.Second,
		MaxConsecutiveFailures: 1, // a single counted failure would trip — reclaim failure must NOT count
		Log:                    r,
	})
	if code != 0 {
		t.Errorf("exit code = %d, want 0 (a failed prune must not end the loop)", code)
	}
	if ran != 2 {
		t.Errorf("RunPipeline ran %d tickets, want 2 (a failed prune must not stop the loop consuming the queue)", ran)
	}
	if r.saw("circuit breaker") {
		t.Errorf("a reclaim failure must NOT trip the breaker (threshold 1); events = %v", r.events)
	}
	if !r.saw("pruning merged worktrees failed") {
		t.Errorf("a failed prune must be logged as a warning; events = %v", r.events)
	}
	if r.saw("reclaimed") {
		t.Errorf("a prune that removed nothing must not narrate a reclaim; events = %v", r.events)
	}
}
