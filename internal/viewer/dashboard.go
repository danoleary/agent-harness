package viewer

import (
	"strconv"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// pipelineStages is the fixed three-stage pipeline, in order, that the stage
// indicator counts against ("n of 3"). It is the viewer's local copy of the
// daemon's stage vocabulary (ADR-0005) — a stage name the daemon emits that is not
// here renders as an unknown stage rather than a crash.
var pipelineStages = []string{"implementation", "review", "retrospective"}

// stageCount is the denominator of the "n of 3" indicator.
const stageCount = 3

// tailMax bounds the scrollback tail to the most recent events, so the dashboard
// stays a fixed-height frame the ticker can redraw in place rather than an
// ever-growing buffer. Kept short (the last few events) — the operator wants the
// current activity, not a full log; loop.log is the full record.
const tailMax = 3

// stageIndex returns the 1-based position of stage in the pipeline, or 0 when the
// stage is empty or unrecognised.
func stageIndex(stage string) int {
	for i, s := range pipelineStages {
		if s == stage {
			return i + 1
		}
	}
	return 0
}

// clearHome is the only ANSI the viewer emits: clear the whole screen (\x1b[2J)
// then move the cursor home (\x1b[H), so each ticker redraw overwrites the prior
// frame in place rather than scrolling. It is hand-rolled (ADR-0005 — no TUI
// dependency) and confined to Frame, never the body, so the rendered content stays
// plain and the non-TTY fallback shares it without leaking escapes into pipes.
const clearHome = "\x1b[2J\x1b[H"

// Frame wraps a rendered dashboard body in the clear-screen + cursor-home escape
// for in-place redraw. Only the dashboard (TTY) path calls it; the fallback prints
// bodies without it.
func Frame(body string) string { return clearHome + body }

// DaemonStatus is the environmental state the viewer probes each tick, external to
// the event stream and injected into Render so the render stays pure and testable.
// The three facts are independent of the folded events: whether the daemon process
// is alive (pidfile liveness), whether a STOP sentinel has been requested (the
// operator asked the loop to wind down — ADR-0005), and whether the stream file
// exists at all (so a daemon that is up but emitting no stream — e.g. an old build —
// is distinguishable from one that is running but has produced no events yet).
type DaemonStatus struct {
	Alive         bool
	StopRequested bool
	StreamPresent bool
}

// Dashboard is the live full-screen model: it folds the structured loop stream
// into the handful of fields the redraw-on-a-ticker view shows — the current
// ticket, which stage is running, the current step, and a scrollback tail — and
// renders them to an ANSI frame. It is deliberately separate from View (the plain
// fallback): the model holds structured state so a late-attaching viewer renders
// the current ticket/stage immediately, without waiting for the next event.
type Dashboard struct {
	ticket     string   // current ticket id
	ticketLine string   // the ticket-selected message verbatim (carries priority)
	stage      string   // current stage name
	step       string   // latest tool-use message (the current step)
	toolCount  int      // tool-use count within the current stage
	lastTS     string   // RFC3339 timestamp of the most recent observed event
	pig        pigState // mascot mood, selected from the most recent event's kind

	// view renders each observed record into a plain scrollback line, reused for the
	// tail so the dashboard's scrollback matches the non-TTY fallback exactly.
	view *View
	tail []string // ring buffer of the most recent rendered lines (≤ tailMax)
}

// NewDashboard returns a fresh Dashboard with no current ticket or stage. Before
// any event the mascot sleeps — the queue-empty resting state.
func NewDashboard() *Dashboard { return &Dashboard{view: New(), pig: pigSleeping} }

// Observe folds one record into the model state. Non-empty Ticket/Stage update the
// tracked values; a ticket-selected record additionally captures its message for
// the ticket panel.
func (d *Dashboard) Observe(r loopstream.Record) {
	switch r.Kind {
	case loopstream.KindTicketSelected:
		// A freshly-selected ticket has not entered a stage yet, so the prior ticket's
		// stage/step/activity no longer describe it (ticket-selected carries no Stage).
		// Clear them — mirroring View.Observe — so the panel shows the new ticket as
		// waiting rather than the previous ticket's final stage until its first
		// stage-start arrives, and the header stays consistent with the tail's bracket.
		d.ticketLine = r.Message
		d.stage = ""
		d.step = ""
		d.toolCount = 0
	case loopstream.KindStageStart:
		// A fresh stage zeroes the per-stage activity, so the counter is honest
		// about being scoped to the running stage (ADR-0005), not cumulative.
		d.toolCount = 0
		d.step = ""
	case loopstream.KindToolUse:
		d.toolCount++
		d.step = r.Message
	}
	if r.Ticket != "" {
		d.ticket = r.Ticket
	}
	if r.Stage != "" {
		d.stage = r.Stage
	}
	if r.TS != "" {
		d.lastTS = r.TS
	}
	// The mascot reflects the most recent event, whatever its kind (AC1).
	d.pig = pigStateFor(r)
	if line, ok := d.view.Observe(r); ok {
		d.tail = append(d.tail, line)
		if len(d.tail) > tailMax {
			d.tail = d.tail[len(d.tail)-tailMax:]
		}
	}
}

// Render returns the dashboard frame body (no clear-screen escape — see Frame) for
// the model's current state. now and status are injected so the render is pure and
// testable: status carries the daemon's liveness / STOP / stream-present facts, and
// now anchors the "age of last event". frame is the redraw tick (advanced by the
// command) the mascot animates on; animate is false under --no-animation, freezing
// the mascot to its single static frame. A pending STOP overrides the event-derived
// mood — winding down is the headline, so the mascot waves goodbye whatever it was up
// to — except once the loop has emitted its terminal loop-stopped record, when the
// definitive "stopped" mascot wins over the still-alive "stopping" wave (see below).
func (d *Dashboard) Render(now time.Time, status DaemonStatus, frame int, animate bool) string {
	var b strings.Builder
	mood := d.pig
	// A pending STOP makes "winding down" the headline — but once the loop has emitted
	// its terminal loop-stopped record (mood == pigStopped) it has actually exited, so
	// the definitive "stopped" mascot wins over the still-alive "stopping" wave. The
	// STOP sentinel persists past the exit (cleared only at the next startup), so
	// without this a STOP-stopped daemon would freeze on "stopping" forever (BEH-613).
	if status.StopRequested && mood != pigStopped {
		mood = pigStopping
	}
	b.WriteString(renderPig(mood, frame, animate))
	b.WriteString("\n")
	b.WriteString("ticket:  ")
	b.WriteString(d.ticket)
	b.WriteString("\n")
	if d.ticketLine != "" {
		b.WriteString("         ")
		b.WriteString(d.ticketLine)
		b.WriteString("\n")
	}
	b.WriteString("stage:   ")
	b.WriteString(d.renderStage())
	b.WriteString("\n")
	b.WriteString("step:    ")
	b.WriteString(d.renderStep())
	b.WriteString("\n")
	b.WriteString("health:  ")
	b.WriteString(d.renderHealth(now, status))
	b.WriteString("\n")
	b.WriteString("recent:\n")
	for _, line := range d.tail {
		b.WriteString("  ")
		b.WriteString(line)
		b.WriteString("\n")
	}
	return b.String()
}

// renderHealth formats the daemon-health line from liveness and the age of the
// last observed event (ADR-0005). A down daemon is a clear state, not a crash; a
// live one shows how stale its last event is so a wedged-but-alive loop is visible.
func (d *Dashboard) renderHealth(now time.Time, status DaemonStatus) string {
	if !status.Alive {
		return "○ daemon down"
	}
	// A pending STOP is the headline: the operator asked the loop to wind down, so
	// say so regardless of event age — a busy-but-stopping daemon reads as stopping.
	if status.StopRequested {
		return "● running — STOP requested, winding down"
	}
	// The stream file being absent is distinct from it being present-but-empty: an
	// absent stream means the live daemon is not writing the structured stream at all
	// (e.g. a build predating it), so point at that rather than the benign "no events
	// yet" — which would otherwise mislead the operator into waiting for events that
	// will never come.
	if !status.StreamPresent {
		return "● running — no event stream (daemon may predate it — rebuild & relaunch)"
	}
	if d.lastTS == "" {
		return "● running — no events yet"
	}
	ts, err := time.Parse(time.RFC3339, d.lastTS)
	if err != nil {
		return "● running"
	}
	return "● running — last event " + formatAge(now.Sub(ts)) + " ago"
}

// formatAge renders a coarse, human age for the last-event line: seconds under a
// minute, then minutes, then hours. Negative skew (a future timestamp from clock
// drift) is clamped to 0s rather than shown as a negative age.
func formatAge(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	switch {
	case d < time.Minute:
		return strconv.Itoa(int(d.Seconds())) + "s"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	default:
		return strconv.Itoa(int(d.Hours())) + "h"
	}
}

// renderStep formats the current-step line: the latest tool-use message and the
// per-stage tool-call counter. With no tool-use yet it is an honest "(waiting)".
func (d *Dashboard) renderStep() string {
	calls := strconv.Itoa(d.toolCount) + " tool calls"
	if d.step == "" {
		return "(waiting)  " + calls
	}
	return d.step + "  (" + calls + ")"
}

// renderStage formats the stage line: the stage name and the "n of 3" indicator,
// or a waiting state when no stage has started yet.
func (d *Dashboard) renderStage() string {
	idx := stageIndex(d.stage)
	if idx == 0 {
		return "(waiting)"
	}
	return d.stage + "  [" + strconv.Itoa(idx) + " of " + strconv.Itoa(stageCount) + "]"
}
