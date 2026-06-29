package viewer

import (
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// The tracer: after a ticket is selected and the implementation stage starts, the
// dashboard frame surfaces the ticket id and the stage indicator as "1 of 3".
func TestDashboardSurfacesTicketAndStageIndicator(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{Kind: loopstream.KindTicketSelected, Ticket: "BEH-7", Message: "selected BEH-7 (Urgent) — claimed → In Progress"})
	d.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "implementation", Message: "run X — implementation BEH-7"})

	frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	if !strings.Contains(frame, "BEH-7") {
		t.Fatalf("expected ticket id in frame, got:\n%s", frame)
	}
	if !strings.Contains(frame, "implementation") {
		t.Fatalf("expected stage name in frame, got:\n%s", frame)
	}
	if !strings.Contains(frame, "1 of 3") {
		t.Fatalf("expected stage indicator '1 of 3' in frame, got:\n%s", frame)
	}
}

// AC6: the layout render for a sequence of events. Drives a realistic stream
// through the model and asserts every panel is present, in order, reflecting the
// latest state — the integration counterpart to the per-field tests above.
func TestDashboardRendersFullLayoutForEventSequence(t *testing.T) {
	d := NewDashboard()
	for _, r := range []loopstream.Record{
		{TS: "2026-06-28T12:00:00Z", Kind: loopstream.KindTicketSelected, Ticket: "BEH-7", Message: "selected BEH-7 (Urgent) — claimed → In Progress"},
		{TS: "2026-06-28T12:00:01Z", Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "implementation", Message: "run X — implementation BEH-7"},
		{TS: "2026-06-28T12:00:02Z", Kind: loopstream.KindSandboxLaunch, Ticket: "BEH-7", Stage: "implementation", Message: "launching sandbox (cap 30 min)"},
		{TS: "2026-06-28T12:00:03Z", Kind: loopstream.KindToolUse, Message: "⚒ Read"},
		{TS: "2026-06-28T12:00:04Z", Kind: loopstream.KindToolUse, Message: "⚒ Bash"},
		{TS: "2026-06-28T12:00:05Z", Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "review", Message: "run X — review BEH-7"},
	} {
		d.Observe(r)
	}
	now, _ := time.Parse(time.RFC3339, "2026-06-28T12:00:07Z")
	frame := d.Render(now, DaemonStatus{Alive: true, StreamPresent: true}, 0, false)

	// Each panel header is present, in top-to-bottom order.
	mustOrder(t, frame, "ticket:", "stage:", "step:", "health:", "recent:")
	// Panels reflect the latest state: review stage (2 of 3), priority in the panel,
	// a running daemon with the 2s-old last event, and the tool counter reset for the
	// new stage (no tool-use in review yet → 0).
	for _, want := range []string{"BEH-7", "Urgent", "review", "2 of 3", "running", "2s"} {
		if !strings.Contains(frame, want) {
			t.Fatalf("expected %q in the layout, got:\n%s", want, frame)
		}
	}
}

// mustOrder asserts each substring appears, and appears after the previous one.
func mustOrder(t *testing.T, s string, parts ...string) {
	t.Helper()
	prev := 0
	for _, p := range parts {
		i := strings.Index(s[prev:], p)
		if i < 0 {
			t.Fatalf("expected %q after offset %d, not found in:\n%s", p, prev, s)
		}
		prev += i + len(p)
	}
}

func TestDashboardStageIndicatorCountsAllThreeStages(t *testing.T) {
	for _, tc := range []struct {
		stage string
		want  string
	}{
		{"implementation", "1 of 3"},
		{"review", "2 of 3"},
		{"retrospective", "3 of 3"},
	} {
		d := NewDashboard()
		d.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-1", Stage: tc.stage, Message: "m"})
		frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
		if !strings.Contains(frame, tc.want) {
			t.Fatalf("stage %q: expected %q in frame, got:\n%s", tc.stage, tc.want, frame)
		}
	}
}

func TestDashboardTracksCurrentStepAndToolCounter(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "implementation", Message: "m"})
	d.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Read"})
	d.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"})

	frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	if !strings.Contains(frame, "⚒ Bash") {
		t.Fatalf("expected the latest tool-use as the current step, got:\n%s", frame)
	}
	if !strings.Contains(frame, "2") {
		t.Fatalf("expected a tool-call counter of 2, got:\n%s", frame)
	}

	// A new stage resets the per-stage tool counter.
	d.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "review", Message: "m"})
	d.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Grep"})
	frame = d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	if !strings.Contains(frame, "⚒ Grep") {
		t.Fatalf("expected the new stage's tool-use as current step, got:\n%s", frame)
	}
	if strings.Contains(frame, "2 tool") || !strings.Contains(frame, "1 tool") {
		t.Fatalf("expected the tool counter to reset to 1 on the new stage, got:\n%s", frame)
	}
}

// Frame wraps a body with the clear-screen + cursor-home escape so the ticker
// redraws in place; the body content is unchanged (no ANSI baked into Render).
func TestFrameWrapsBodyWithClearScreenEscape(t *testing.T) {
	body := "ticket:  BEH-7\n"
	framed := Frame(body)
	if !strings.HasPrefix(framed, "\x1b[2J\x1b[H") {
		t.Fatalf("expected a clear-screen + home prefix, got %q", framed)
	}
	if !strings.Contains(framed, body) {
		t.Fatalf("the body must be preserved inside the frame, got %q", framed)
	}
}

// The dashboard keeps a bounded scrollback tail of recent events: the newest event
// is shown, and an old event past the tail bound has scrolled off.
func TestDashboardScrollbackTailIsBoundedToRecentEvents(t *testing.T) {
	d := NewDashboard()
	for i := 0; i < tailMax+5; i++ {
		d.Observe(loopstream.Record{Kind: loopstream.KindIdle, Message: "event-" + strconv.Itoa(i)})
	}
	frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	newest := "event-" + strconv.Itoa(tailMax+4)
	oldest := "event-0"
	if !strings.Contains(frame, newest) {
		t.Fatalf("expected the newest event %q in the tail, got:\n%s", newest, frame)
	}
	if strings.Contains(frame, oldest) {
		t.Fatalf("expected the oldest event %q to have scrolled off, got:\n%s", oldest, frame)
	}
}

// AC4: the daemon being down is a clear rendered state, never a crash.
func TestDashboardShowsDaemonDownState(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "implementation", Message: "m"})
	frame := d.Render(time.Unix(100, 0), DaemonStatus{Alive: false, StreamPresent: true}, 0, false)
	if !strings.Contains(strings.ToLower(frame), "down") {
		t.Fatalf("a down daemon must render a clear down state, got:\n%s", frame)
	}
	if strings.Contains(strings.ToLower(frame), "running") {
		t.Fatalf("a down daemon must not render as running, got:\n%s", frame)
	}
}

// A live daemon renders as running with the age of the last observed event, so the
// operator can see staleness (ADR-0005 — health is liveness + last-event age).
func TestDashboardShowsRunningWithLastEventAge(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{TS: "2026-06-28T12:00:00Z", Kind: loopstream.KindToolUse, Ticket: "BEH-7", Stage: "implementation", Message: "⚒ Bash"})
	now, _ := time.Parse(time.RFC3339, "2026-06-28T12:00:05Z")
	frame := d.Render(now, DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	if !strings.Contains(strings.ToLower(frame), "running") {
		t.Fatalf("a live daemon must render as running, got:\n%s", frame)
	}
	if !strings.Contains(frame, "5s") {
		t.Fatalf("expected a 5s last-event age, got:\n%s", frame)
	}
}

// A live daemon whose stream file does not exist at all is distinct from one that
// is up but has produced no events yet: the absent stream means the daemon is not
// emitting the structured stream (e.g. an old build predating it), which the health
// line must call out rather than the misleading "no events yet". Both differ from
// the empty-but-present stream, which is honestly "no events yet".
func TestDashboardDistinguishesMissingStreamFromNoEvents(t *testing.T) {
	now := time.Unix(100, 0)
	d := NewDashboard()

	// Stream present but empty (truncated at startup, nothing written yet).
	noEvents := d.Render(now, DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	if !strings.Contains(noEvents, "no events yet") {
		t.Fatalf("a present-but-empty stream must read as 'no events yet', got:\n%s", noEvents)
	}

	// Stream file absent — the daemon is up but emitting no stream.
	missing := d.Render(now, DaemonStatus{Alive: true, StreamPresent: false}, 0, false)
	if strings.Contains(missing, "no events yet") {
		t.Fatalf("an absent stream must NOT read as the benign 'no events yet', got:\n%s", missing)
	}
	if !strings.Contains(strings.ToLower(missing), "no event stream") {
		t.Fatalf("an absent stream must be called out as no event stream, got:\n%s", missing)
	}
}

// A live daemon with the STOP sentinel present is winding down, not idle — the
// health line must surface that the operator requested a stop, the single most
// relevant fact about the loop's state. STOP is the headline: it wins over the
// last-event age, so a busy-but-stopping daemon reads as stopping.
func TestDashboardShowsStopRequestedState(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{TS: "2026-06-28T12:00:00Z", Kind: loopstream.KindToolUse, Message: "⚒ Bash"})
	now, _ := time.Parse(time.RFC3339, "2026-06-28T12:00:05Z")
	frame := d.Render(now, DaemonStatus{Alive: true, StopRequested: true, StreamPresent: true}, 0, false)
	low := strings.ToLower(frame)
	if !strings.Contains(low, "stop") {
		t.Fatalf("a pending STOP must be surfaced in the health line, got:\n%s", frame)
	}
	if !strings.Contains(low, "winding down") {
		t.Fatalf("a pending STOP must read as winding down, got:\n%s", frame)
	}
}

// Selecting a new ticket clears the previous ticket's stage/step/counter, so the
// panel shows the new ticket as waiting rather than the prior ticket's final stage
// until its first stage-start (ticket-selected carries no Stage — see next.go).
func TestDashboardClearsStaleStageOnNewTicket(t *testing.T) {
	d := NewDashboard()
	// Prior ticket ran all the way through retrospective with some activity.
	d.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "retrospective", Message: "m"})
	d.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"})
	// A new ticket is selected (no Stage on the record).
	d.Observe(loopstream.Record{Kind: loopstream.KindTicketSelected, Ticket: "BEH-8", Message: "selected BEH-8 (Urgent) — claimed → In Progress"})

	frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	// Assert on the header panels only — the scrollback tail legitimately preserves
	// the prior ticket's historical lines (retrospective, ⚒ Bash).
	header := frame[:strings.Index(frame, "recent:")]
	if !strings.Contains(header, "BEH-8") {
		t.Fatalf("expected the new ticket id in the header, got:\n%s", header)
	}
	if strings.Contains(header, "retrospective") || strings.Contains(header, "3 of 3") {
		t.Fatalf("a newly-selected ticket must not show the prior ticket's stage, got:\n%s", header)
	}
	if strings.Contains(header, "⚒ Bash") || strings.Contains(header, "1 tool") {
		t.Fatalf("a newly-selected ticket must not show the prior ticket's step/counter, got:\n%s", header)
	}
}

// AC1: the pig's state is selected from the most recent event and updates live as
// events arrive — and before any event it sleeps (queue empty).
func TestDashboardPigReflectsMostRecentEvent(t *testing.T) {
	d := NewDashboard()
	// No events yet → sleeping.
	if frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false); !strings.Contains(frame, "status: sleeping") {
		t.Fatalf("a fresh dashboard with no events should show a sleeping mascot, got:\n%s", frame)
	}
	// A tool-use makes it work; a later pr-opened makes it celebrate (most recent wins).
	d.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"})
	if frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false); !strings.Contains(frame, "status: working") {
		t.Fatalf("a recent tool-use should show a working mascot, got:\n%s", frame)
	}
	d.Observe(loopstream.Record{Kind: loopstream.KindPROpened, Ticket: "BEH-7", Stage: "review", Message: "review ✓ PR opened: http://x"})
	if frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false); !strings.Contains(frame, "status: celebrating") {
		t.Fatalf("a recent pr-opened should show a celebrating mascot, got:\n%s", frame)
	}
}

// A pending STOP overrides the event-derived mascot mood: winding down is the
// headline, so even a mascot that was working (a recent tool-use) shows "stopping".
func TestDashboardMascotShowsStoppingWhenStopRequested(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"})
	// Without STOP the most recent event wins → working.
	if frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false); !strings.Contains(frame, "status: working") {
		t.Fatalf("without STOP a recent tool-use should show working, got:\n%s", frame)
	}
	// With STOP the mascot shows stopping regardless of the underlying event mood.
	frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StopRequested: true, StreamPresent: true}, 0, false)
	if !strings.Contains(frame, "status: stopping") {
		t.Fatalf("a pending STOP must show the stopping mascot, got:\n%s", frame)
	}
	if strings.Contains(frame, "status: working") {
		t.Fatalf("a pending STOP must override the working mood, got:\n%s", frame)
	}
}

// BEH-613: once the loop has emitted its terminal KindLoopStopped record, the
// "stopped" mascot is the definitive headline and must win even while the STOP
// sentinel is still present (it is only cleared at the NEXT startup). Otherwise a
// STOP-stopped daemon would freeze on the still-alive "stopping" wave forever,
// defeating the whole point of a definitive stopped signal in the animation.
func TestDashboardTerminalStoppedWinsOverStopRequested(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{Kind: loopstream.KindLoopStopped, Message: "loop — stopped: stop requested"})
	frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: false, StopRequested: true, StreamPresent: true}, 0, false)
	if !strings.Contains(frame, "status: stopped") {
		t.Fatalf("a terminal loop-stopped record must show the stopped mascot even with STOP still pending, got:\n%s", frame)
	}
	if strings.Contains(frame, "status: stopping") {
		t.Fatalf("the terminal stopped state must win over the stopping override, got:\n%s", frame)
	}
}

// AC2 at the dashboard layer: with animation on, the pig frame advances with the
// tick; with animation off, the rendered pig is identical across ticks.
func TestDashboardPigAnimatesOnlyWhenEnabled(t *testing.T) {
	d := NewDashboard()
	d.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"})

	if a, b := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, true), d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 1, true); a == b {
		t.Fatalf("animating: the pig must differ between ticks 0 and 1")
	}
	if a, b := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false), d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 1, false); a != b {
		t.Fatalf("static (--no-animation): the pig must be identical across ticks")
	}
}

// Late attach (AC2): a viewer that attaches mid-ticket reads a record carrying the
// structured ticket/stage fields and renders the panel immediately — without ever
// having seen the ticket-selected event that started the ticket.
func TestDashboardLateAttachRendersFromStructuredFields(t *testing.T) {
	d := NewDashboard()
	// First and only record the late viewer sees: a stage-start mid-ticket.
	d.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-42", Stage: "review", Message: "run X — review BEH-42"})
	frame := d.Render(time.Unix(0, 0), DaemonStatus{Alive: true, StreamPresent: true}, 0, false)
	if !strings.Contains(frame, "BEH-42") || !strings.Contains(frame, "review") || !strings.Contains(frame, "2 of 3") {
		t.Fatalf("late attach should render ticket+stage immediately, got:\n%s", frame)
	}
}
