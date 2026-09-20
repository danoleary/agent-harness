// Package viewer is the read-only consumer side of the loop stream (ADR-0005): it
// parses loopstream.Records and renders each as one plain line surfacing the
// current ticket and stage. It is deliberately the plain-text path the later
// animated dashboard falls back to on a non-TTY — no ANSI, no animation — so it is
// safe to pipe or redirect.
//
// The View is stateful: ticket-selected and stage-start records set the current
// ticket/stage, and later events (tool-use, session-result, …) that don't carry
// them are still rendered against the tracked values. That statefulness is the
// whole point — a single global stream lets the viewer always know "what ticket,
// which stage" without scanning per-ticket directories.
package viewer

import (
	"bufio"
	"io"
	"strings"

	"github.com/danoleary/agent-harness/internal/loopstream"
)

// NotRunningMessage is shown when the stream file is absent — no daemon or
// single-shot run has written it. It is a clear status line, not an error.
const NotRunningMessage = "loop not running — no logs/loop.jsonl yet (start it with `agent-harness start`, or run `agent-harness pipeline --next`)"

// describe maps a Kind to a short, fixed label used in the rendered line. It is
// the closed-enum switch the viewer drives off (ADR-0005): every loopstream.Kind
// must have a case, asserted by TestEveryKindIsHandled, so a new kind can never
// fall through to a silent default. ok=false means the kind is unmapped.
func describe(k loopstream.Kind) (label string, ok bool) {
	switch k {
	case loopstream.KindTicketSelected:
		return "selected", true
	case loopstream.KindStageStart:
		return "stage", true
	case loopstream.KindSandboxLaunch:
		return "sandbox", true
	case loopstream.KindToolUse:
		return "tool", true
	case loopstream.KindSessionResult:
		return "result", true
	case loopstream.KindPROpened:
		return "pr", true
	case loopstream.KindTicketReleased:
		return "released", true
	case loopstream.KindRecommendClose:
		return "recommend-close", true
	case loopstream.KindCapAbort:
		return "cap-abort", true
	case loopstream.KindCapBackoff:
		return "cap-backoff", true
	case loopstream.KindBreakerTrip:
		return "breaker", true
	case loopstream.KindIdle:
		return "idle", true
	case loopstream.KindLoopStopped:
		return "stopped", true
	default:
		return "", false
	}
}

// View tracks the current ticket and stage as records stream in.
type View struct {
	ticket string
	stage  string
}

// New returns a fresh View with no current ticket or stage.
func New() *View { return &View{} }

// Observe folds one record into the view's state and returns the plain line to
// print for it. ok=false means the record's kind is unmapped (an unknown kind from
// a newer daemon) and the caller may skip it. State rule: a record's non-empty
// Ticket/Stage update the tracked values; ticket-selected additionally clears the
// stage, since a freshly-selected ticket has not entered a stage yet.
func (v *View) Observe(r loopstream.Record) (string, bool) {
	label, ok := describe(r.Kind)
	if !ok {
		return "", false
	}
	if r.Kind == loopstream.KindTicketSelected {
		v.stage = ""
	}
	if r.Ticket != "" {
		v.ticket = r.Ticket
	}
	if r.Stage != "" {
		v.stage = r.Stage
	}
	return v.render(label, r), true
}

// render formats one line: "<ts>  [<ticket> · <stage>] (<label>) <message>", with
// the bracketed context omitted when no ticket is known yet.
func (v *View) render(label string, r loopstream.Record) string {
	var b strings.Builder
	if r.TS != "" {
		b.WriteString(r.TS)
		b.WriteString("  ")
	}
	if v.ticket != "" {
		b.WriteString("[")
		b.WriteString(v.ticket)
		if v.stage != "" {
			b.WriteString(" · ")
			b.WriteString(v.stage)
		}
		b.WriteString("] ")
	}
	b.WriteString("(")
	b.WriteString(label)
	b.WriteString(") ")
	b.WriteString(r.Message)
	return b.String()
}

// Replay reads JSONL records from r and writes one rendered line per valid record
// to out. Blank lines and malformed (non-JSON or unmapped-kind) lines are skipped
// so a single bad chunk never aborts the tail. It returns only a read error.
func (v *View) Replay(r io.Reader, out io.Writer) error {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64*1024), 1024*1024)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		rendered, ok := v.RenderLine(line)
		if !ok {
			continue
		}
		if _, err := io.WriteString(out, rendered+"\n"); err != nil {
			return err
		}
	}
	return sc.Err()
}

// RenderLine parses one JSONL line into a record and renders it. ok=false for a
// blank, malformed, or unmapped-kind line.
func (v *View) RenderLine(line string) (string, bool) {
	r, ok := parse(line)
	if !ok {
		return "", false
	}
	return v.Observe(r)
}
