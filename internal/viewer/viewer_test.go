package viewer

import (
	"bytes"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/loopstream"
)

func TestRenderSurfacesCurrentTicketAndStage(t *testing.T) {
	v := New()

	line, ok := v.Observe(loopstream.Record{Kind: loopstream.KindTicketSelected, Ticket: "BEH-7", Message: "selected BEH-7 — claimed"})
	if !ok {
		t.Fatalf("expected a rendered line for ticket-selected")
	}
	if !strings.Contains(line, "BEH-7") {
		t.Fatalf("expected ticket in line, got %q", line)
	}

	line, _ = v.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-7", Stage: "implementation", Message: "run X — implementation BEH-7"})
	if !strings.Contains(line, "BEH-7") || !strings.Contains(line, "implementation") {
		t.Fatalf("expected ticket+stage in line, got %q", line)
	}

	// A later event that does not itself carry ticket/stage still surfaces the
	// current ones the viewer is tracking.
	line, _ = v.Observe(loopstream.Record{Kind: loopstream.KindToolUse, Message: "⚒ Bash"})
	if !strings.Contains(line, "BEH-7") || !strings.Contains(line, "implementation") {
		t.Fatalf("expected carried-forward ticket+stage, got %q", line)
	}
	if !strings.Contains(line, "⚒ Bash") {
		t.Fatalf("expected the event message in the line, got %q", line)
	}
}

func TestTicketSelectedResetsStage(t *testing.T) {
	v := New()
	v.Observe(loopstream.Record{Kind: loopstream.KindStageStart, Ticket: "BEH-1", Stage: "review", Message: "m"})
	line, _ := v.Observe(loopstream.Record{Kind: loopstream.KindTicketSelected, Ticket: "BEH-2", Message: "selected BEH-2"})
	if strings.Contains(line, "review") {
		t.Fatalf("a new ticket should clear the prior stage, got %q", line)
	}
	if !strings.Contains(line, "BEH-2") {
		t.Fatalf("expected new ticket, got %q", line)
	}
}

func TestReplayRendersEveryLineToOutput(t *testing.T) {
	in := strings.NewReader(strings.Join([]string{
		`{"ts":"t1","kind":"ticket-selected","ticket":"BEH-9","message":"selected BEH-9"}`,
		`{"ts":"t2","kind":"stage-start","ticket":"BEH-9","stage":"implementation","message":"run X — implementation BEH-9"}`,
		``, // blank lines are skipped, not errors
		`{"ts":"t3","kind":"idle","message":"queue empty; idling"}`,
	}, "\n"))
	var out bytes.Buffer
	v := New()
	if err := v.Replay(in, &out); err != nil {
		t.Fatalf("replay: %v", err)
	}
	got := out.String()
	if c := strings.Count(got, "\n"); c != 3 {
		t.Fatalf("expected 3 rendered lines, got %d:\n%s", c, got)
	}
	if !strings.Contains(got, "BEH-9") || !strings.Contains(got, "implementation") {
		t.Fatalf("expected ticket/stage in output:\n%s", got)
	}
}

func TestReplaySkipsMalformedLinesWithoutError(t *testing.T) {
	in := strings.NewReader("not json\n" + `{"ts":"t","kind":"idle","message":"idle"}` + "\n")
	var out bytes.Buffer
	v := New()
	if err := v.Replay(in, &out); err != nil {
		t.Fatalf("a malformed line must be skipped, not fatal: %v", err)
	}
	if strings.Contains(out.String(), "not json") {
		t.Fatalf("malformed line should not render: %q", out.String())
	}
	if !strings.Contains(out.String(), "idle") {
		t.Fatalf("the valid line after a malformed one should still render: %q", out.String())
	}
}

// TestEveryKindIsHandled is the AC3 guard: the closed enum and the viewer's
// mapping must stay in lockstep. A Kind added to loopstream without a viewer
// label fails here rather than silently rendering a default.
func TestEveryKindIsHandled(t *testing.T) {
	for _, k := range loopstream.AllKinds() {
		label, ok := describe(k)
		if !ok {
			t.Fatalf("kind %q has no viewer mapping — add it to describe()", k)
		}
		if label == "" {
			t.Fatalf("kind %q mapped to an empty label", k)
		}
	}
}

func TestDescribeRejectsUnmappedKind(t *testing.T) {
	if _, ok := describe(loopstream.Kind("totally-unknown")); ok {
		t.Fatalf("an unmapped kind must report ok=false, not a silent default")
	}
}

func TestNotRunningMessageIsClear(t *testing.T) {
	if !strings.Contains(strings.ToLower(NotRunningMessage), "not running") {
		t.Fatalf("expected a clear not-running message, got %q", NotRunningMessage)
	}
}
