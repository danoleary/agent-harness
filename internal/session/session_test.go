package session

import (
	"bytes"
	"strings"
	"testing"
)

type fakeLog struct {
	teed   []string
	events []string
}

func (f *fakeLog) Event(msg string)      { f.events = append(f.events, msg) }
func (f *fakeLog) TeeLine(_, raw string) { f.teed = append(f.teed, strings.TrimSuffix(raw, "\n")) }

const (
	lineSystem  = `{"type":"system","subtype":"init"}`
	lineToolUse = `{"type":"assistant","message":{"content":[{"type":"tool_use","name":"Edit"}]}}`
	lineResult  = `{"type":"result","subtype":"success","duration_ms":1000}`
)

// Non-verbose: every stdout line is teed raw, and narratable lines surface as
// concise events (the default console view).
func TestPumpStdoutTeesAllAndNarrates(t *testing.T) {
	in := strings.Join([]string{lineSystem, lineToolUse, lineResult}, "\n")
	log := &fakeLog{}
	var echo bytes.Buffer

	pumpStdout(strings.NewReader(in), "implementation-x.jsonl", false, log, &echo)

	if len(log.teed) != 3 {
		t.Fatalf("expected all 3 lines teed, got %d: %v", len(log.teed), log.teed)
	}
	joined := strings.Join(log.events, "\n")
	if !strings.Contains(joined, "⚒ Edit") {
		t.Errorf("expected tool_use narration, got: %q", joined)
	}
	if !strings.Contains(joined, "session success") {
		t.Errorf("expected result narration, got: %q", joined)
	}
	if echo.Len() != 0 {
		t.Errorf("non-verbose must not echo raw, got: %q", echo.String())
	}
}

// Verbose: every line is echoed raw to the console and still teed; the concise
// narration is suppressed (the raw stream is the whole point of --verbose).
func TestPumpStdoutVerboseEchoesRawAndSkipsNarration(t *testing.T) {
	in := strings.Join([]string{lineToolUse, lineResult}, "\n")
	log := &fakeLog{}
	var echo bytes.Buffer

	pumpStdout(strings.NewReader(in), "implementation-x.jsonl", true, log, &echo)

	if len(log.teed) != 2 {
		t.Fatalf("expected both lines teed, got %d", len(log.teed))
	}
	if !strings.Contains(echo.String(), lineToolUse) || !strings.Contains(echo.String(), lineResult) {
		t.Errorf("verbose should echo raw lines, got: %q", echo.String())
	}
	if len(log.events) != 0 {
		t.Errorf("verbose must not narrate, got: %v", log.events)
	}
}

// stderr is forensic: every line is teed, and a bounded tail of the last
// non-empty lines is returned so a docker launch failure can be explained.
func TestPumpStderrTeesAllAndReturnsBoundedTail(t *testing.T) {
	var lines []string
	for i := 0; i < 15; i++ {
		lines = append(lines, "err line")
	}
	// Blank lines are teed but excluded from the tail.
	in := strings.Join(lines, "\n") + "\n\n" + "docker: final cause"
	log := &fakeLog{}

	tail := pumpStderr(strings.NewReader(in), "implementation-x.jsonl", log, 10)

	if len(log.teed) != 17 {
		t.Errorf("expected all 17 lines teed (incl. blank), got %d", len(log.teed))
	}
	if len(tail) != 10 {
		t.Fatalf("expected tail bounded to 10, got %d", len(tail))
	}
	if tail[len(tail)-1] != "docker: final cause" {
		t.Errorf("tail should end at the last non-empty line, got: %q", tail[len(tail)-1])
	}
}
