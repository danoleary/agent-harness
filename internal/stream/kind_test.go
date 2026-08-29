package stream

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/loopstream"
)

func TestNarrateRecordClassifiesToolUse(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Bash"},
			},
		},
	})
	msg, kind, ok := NarrateRecord(line)
	if !ok {
		t.Fatal("expected a narration")
	}
	if kind != loopstream.KindToolUse {
		t.Fatalf("expected tool-use kind, got %q", kind)
	}
	if msg == "" {
		t.Fatal("expected a non-empty message")
	}
}

func TestNarrateRecordClassifiesSessionResult(t *testing.T) {
	line := mustJSON(t, map[string]any{"type": "result", "subtype": "success", "duration_ms": 1000})
	_, kind, ok := NarrateRecord(line)
	if !ok {
		t.Fatal("expected a narration")
	}
	if kind != loopstream.KindSessionResult {
		t.Fatalf("expected session-result kind, got %q", kind)
	}
}

func TestNarrateRecordSkipsUninteresting(t *testing.T) {
	line := mustJSON(t, map[string]any{"type": "system", "subtype": "init"})
	if _, _, ok := NarrateRecord(line); ok {
		t.Fatal("system/init should not be narrated")
	}
}

// Narrate stays a thin wrapper so existing callers are unaffected.
func TestNarrateDelegatesToNarrateRecord(t *testing.T) {
	line := mustJSON(t, map[string]any{"type": "result", "subtype": "success"})
	msg, ok := Narrate(line)
	if !ok || msg == "" {
		t.Fatalf("Narrate should still return the message, got %q ok=%v", msg, ok)
	}
}
