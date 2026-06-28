package runlog

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// Console is the loop-level narrator used before any ticket-keyed Logger exists
// (selection, idle, breaker). Structured mirrors a loop-level event into the same
// global loop.jsonl the per-ticket loggers feed, so the viewer sees one ordered
// stream across selection and the stages.
func TestConsoleStructuredAppendsToGlobalStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	c := NewConsole(loopstream.NewStream(path))

	c.Structured(loopstream.Record{Kind: loopstream.KindIdle, Message: "loop — queue empty; idling"})

	var r loopstream.Record
	if err := json.Unmarshal([]byte(strings.TrimSpace(readFile(t, path))), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if r.Kind != loopstream.KindIdle || r.Message != "loop — queue empty; idling" {
		t.Fatalf("unexpected record: %+v", r)
	}
	if r.TS == "" {
		t.Fatalf("expected a stamped timestamp")
	}
}

// A Console with no stream wired (or a hand-built zero value) must never panic —
// it degrades to console-only narration.
func TestConsoleWithNilStreamDoesNotPanic(t *testing.T) {
	c := NewConsole(nil)
	c.Event("hello")
	c.Structured(loopstream.Record{Kind: loopstream.KindBreakerTrip, Message: "tripped"})
}
