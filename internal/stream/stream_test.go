package stream

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
)

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(b)
}

func TestNarratesToolUse(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "tool_use", "name": "Bash", "input": map[string]any{"command": "pnpm test"}},
			},
		},
	})

	out, ok := Narrate(line)
	if !ok {
		t.Fatal("expected a narration")
	}
	if !strings.Contains(out, "Bash") {
		t.Errorf("narration %q does not contain Bash", out)
	}
}

func TestNarrateMalformedLineSkips(t *testing.T) {
	if _, ok := Narrate("{not json"); ok {
		t.Error("malformed line should be skipped")
	}
	if _, ok := Narrate(""); ok {
		t.Error("empty line should be skipped")
	}
}

func TestNarrateSkipsUninterestingEvents(t *testing.T) {
	line := mustJSON(t, map[string]any{"type": "system", "subtype": "init"})
	if _, ok := Narrate(line); ok {
		t.Error("system/init should not be narrated")
	}
}

func TestNarratesResultWithDuration(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type":        "result",
		"subtype":     "success",
		"is_error":    false,
		"duration_ms": 12345,
	})

	out, ok := Narrate(line)
	if !ok {
		t.Fatal("expected a narration")
	}
	if !regexp.MustCompile(`(?i)result|done|✓`).MatchString(out) {
		t.Errorf("result narration %q lacks a success marker", out)
	}
	if !strings.Contains(out, "(12s)") {
		t.Errorf("result narration %q lacks rounded duration", out)
	}
}
