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
// ever-growing buffer.
const tailMax = 8

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

// Dashboard is the live full-screen model: it folds the structured loop stream
// into the handful of fields the redraw-on-a-ticker view shows — the current
// ticket, which stage is running, the current step, and a scrollback tail — and
// renders them to an ANSI frame. It is deliberately separate from View (the plain
// fallback): the model holds structured state so a late-attaching viewer renders
// the current ticket/stage immediately, without waiting for the next event.
type Dashboard struct {
	ticket     string // current ticket id
	ticketLine string // the ticket-selected message verbatim (carries priority)
	stage      string // current stage name
	step       string // latest tool-use message (the current step)
	toolCount  int    // tool-use count within the current stage
	lastTS     string // RFC3339 timestamp of the most recent observed event

	// view renders each observed record into a plain scrollback line, reused for the
	// tail so the dashboard's scrollback matches the non-TTY fallback exactly.
	view *View
	tail []string // ring buffer of the most recent rendered lines (≤ tailMax)
}

// NewDashboard returns a fresh Dashboard with no current ticket or stage.
func NewDashboard() *Dashboard { return &Dashboard{view: New()} }

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
	if line, ok := d.view.Observe(r); ok {
		d.tail = append(d.tail, line)
		if len(d.tail) > tailMax {
			d.tail = d.tail[len(d.tail)-tailMax:]
		}
	}
}

// Render returns the dashboard frame body (no clear-screen escape — see Frame) for
// the model's current state. now and alive are injected so the render is pure and
// testable: alive is the daemon's liveness, now anchors the "age of last event".
func (d *Dashboard) Render(now time.Time, alive bool) string {
	var b strings.Builder
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
	b.WriteString(d.renderHealth(now, alive))
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
func (d *Dashboard) renderHealth(now time.Time, alive bool) string {
	if !alive {
		return "○ daemon down"
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
