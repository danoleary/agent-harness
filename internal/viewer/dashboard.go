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

// defaultStallAfter is how long a live daemon may go without emitting any event
// before the dashboard escalates to a "stalled" suspicion (ADR-0006). Generous on
// purpose — comfortably above a slow sandbox spin-up or a single long-running tool
// call, both of which legitimately go quiet for minutes — so it flags a genuine
// wedge, not normal sparse activity. Overridable per-process (SetStallThreshold).
const defaultStallAfter = 10 * time.Minute

// bannerRule is the fixed-width horizontal rule that brackets a state banner. The
// viewer is stdlib-only (ADR-0005) with no terminal-size syscall, so the bar is a
// fixed width rather than a true full-width fill — wide enough to read as a headline
// above the mascot.
const bannerRule = "━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━"

// ANSI SGR colours for the state banner. Confined to the banner (and only ever on
// the dashboard path, which a non-TTY / NO_COLOR run never reaches — that falls back
// to the plain View). Colour is reinforcement, never the sole signal: the banner's
// words carry the state on their own (ADR-0006, "never by colour alone").
const (
	ansiReset = "\x1b[0m"
	ansiRed   = "\x1b[91m" // bright red — stopped (process gone)
	ansiAmber = "\x1b[93m" // bright yellow — stalled (alive but wedged)
)

// alertState is the dashboard's headline classification, derived once per render
// from liveness + last-event age and used to drive the mascot mood, the banner, the
// animation freeze, and the transition bell (ADR-0006). It is independent of the
// event-derived pig mood: a dead daemon is "stopped" however it died.
type alertState int

const (
	alertNone    alertState = iota // alive and recently active — normal operation
	alertStalled                   // alive but no event past the stall threshold
	alertStopped                   // process not alive (clean exit or unclean death)
)

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

	// sawTerminal records whether a KindLoopStopped record was ever observed, and
	// stopReason carries its reason. Under ADR-0006 liveness (not this record) drives
	// the stopped mascot, so these only distinguish a clean exit (terminal record seen
	// → show its reason) from an unclean death (process gone, no record → "exited
	// without clean shutdown").
	sawTerminal bool
	stopReason  string

	// stallAfter is the live-but-no-events age past which the dashboard escalates to a
	// "stalled" suspicion. Zero disables stall detection.
	stallAfter time.Duration

	// view renders each observed record into a plain scrollback line, reused for the
	// tail so the dashboard's scrollback matches the non-TTY fallback exactly.
	view *View
	tail []string // ring buffer of the most recent rendered lines (≤ tailMax)
}

// NewDashboard returns a fresh Dashboard with no current ticket or stage. Before
// any event the mascot sleeps — the queue-empty resting state.
func NewDashboard() *Dashboard {
	return &Dashboard{view: New(), pig: pigSleeping, stallAfter: defaultStallAfter}
}

// SetStallThreshold overrides the live-but-no-events age past which the dashboard
// flags "stalled" (ADR-0006). A non-positive value disables stall detection. Set
// once at startup by cmd/watch from its flag/env; the default is defaultStallAfter.
func (d *Dashboard) SetStallThreshold(after time.Duration) { d.stallAfter = after }

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
	case loopstream.KindLoopStopped:
		// The loop wound down cleanly and narrated why. Liveness still drives the
		// mascot (ADR-0006); this only marks the stop as clean and captures its reason.
		d.sawTerminal = true
		d.stopReason = stopReasonFromMessage(r.Message)
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
	alert := d.alertStateFor(now, status)
	mood := d.pig
	switch {
	case alert == alertStopped:
		// Liveness is the authority for "stopped" (ADR-0006): the process is gone —
		// clean exit or unclean death — so the definitive stopped mascot wins over both
		// the event-derived mood and the still-alive "stopping" wave below.
		mood = pigStopped
	case status.StopRequested && mood != pigStopped:
		// A pending STOP makes "winding down" the headline while the daemon is still
		// alive. (Once it actually exits, the alertStopped branch above takes over.)
		mood = pigStopping
	}
	// Motion means "alive and working": freeze the mascot to a single static frame
	// whenever it is stopped or stalled, so a still screen is itself a signal (ADR-0006).
	if alert != alertNone {
		animate = false
	}
	if banner := d.banner(alert, now); banner != "" {
		b.WriteString(banner)
		b.WriteString("\n")
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

// alertStateFor classifies the loop's headline state from liveness and last-event
// age (ADR-0006). A dead process is alertStopped however it died — liveness, not the
// event stream, is the authority. A live daemon that has gone quiet past the stall
// threshold is alertStalled — but a pending STOP is "winding down", not a wedge, so
// it stays alertNone (the health line carries the stopping headline).
func (d *Dashboard) alertStateFor(now time.Time, status DaemonStatus) alertState {
	if !status.Alive {
		return alertStopped
	}
	if status.StopRequested {
		return alertNone
	}
	if d.stallAfter > 0 && d.lastTS != "" {
		if ts, err := time.Parse(time.RFC3339, d.lastTS); err == nil && now.Sub(ts) >= d.stallAfter {
			return alertStalled
		}
	}
	return alertNone
}

// Alerting reports whether the loop is in a bell-worthy state — stopped (the process
// is gone) or stalled (alive but wedged) — for cmd/watch's transition bell. A pending
// STOP is expected winding-down, not an alarm, so it does not alert (ADR-0006).
func (d *Dashboard) Alerting(now time.Time, status DaemonStatus) bool {
	return d.alertStateFor(now, status) != alertNone
}

// banner renders the loud headline above the mascot for a non-normal state, or ""
// for alertNone. Stopped is red and distinguishes a clean exit (terminal record seen
// → its reason) from an unclean death (no record → "exited without clean shutdown",
// the investigate case). Stalled is amber and worded as a suspicion — the process is
// alive and may resume — never the word "stopped" (ADR-0006).
func (d *Dashboard) banner(alert alertState, now time.Time) string {
	switch alert {
	case alertStopped:
		if d.sawTerminal {
			return colourBanner(ansiRed, "⏹  AGENT STOPPED — "+d.stopReason)
		}
		return colourBanner(ansiRed, "⚠  AGENT STOPPED — exited without clean shutdown")
	case alertStalled:
		return colourBanner(ansiAmber, "⚠  STALLED — no activity for "+d.lastEventAge(now))
	default:
		return ""
	}
}

// lastEventAge is the coarse human age of the most recent event, for the stalled
// banner. Empty last timestamp (no events) reads as "0s" — stall detection needs a
// timestamp to fire, so this is only reached with one present.
func (d *Dashboard) lastEventAge(now time.Time) string {
	ts, err := time.Parse(time.RFC3339, d.lastTS)
	if err != nil {
		return formatAge(0)
	}
	return formatAge(now.Sub(ts))
}

// colourBanner wraps a one-line headline in the bracketing rule and the given SGR
// colour. The words carry the state on their own; colour only reinforces it, and the
// reset closes the colour so it never bleeds into the mascot below.
func colourBanner(colour, line string) string {
	body := bannerRule + "\n  " + line + "\n" + bannerRule
	return colour + body + ansiReset
}

// stopReasonFromMessage extracts the human reason from a KindLoopStopped message
// (e.g. "loop — stopped: max-tickets reached" → "max-tickets reached"). With no
// "stopped:" marker it falls back to the whole trimmed message rather than empty.
func stopReasonFromMessage(message string) string {
	const marker = "stopped: "
	if i := strings.Index(message, marker); i >= 0 {
		return strings.TrimSpace(message[i+len(marker):])
	}
	return strings.TrimSpace(message)
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
