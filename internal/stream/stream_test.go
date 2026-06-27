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

// The terminal usage-policy refusal (BEH-389): an is_error result whose text is
// the classifier's "unable to respond … violate our Usage Policy" message. This
// is a known intermittent false-positive on long sessions, so the harness treats
// it as retryable rather than a real failure.
func TestIsUsagePolicyRefusalDetectsTheRefusalResult(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type":     "result",
		"subtype":  "success",
		"is_error": true,
		"result":   "API Error: Claude Code is unable to respond to this request, which appears to violate our Usage Policy (https://www.anthropic.com/legal/aup). Please double press esc to edit your last message or start a new session for Claude Code to assist with a different task. If you are seeing this refusal repeatedly, try running /model claude-sonnet-4-20250514 to switch models.",
	})
	if !IsUsagePolicyRefusal(line) {
		t.Error("expected the usage-policy refusal result to be detected")
	}
}

// A genuine error result (is_error, but not the usage-policy refusal) must NOT be
// treated as retryable — only the policy-refusal false-positive is.
func TestIsUsagePolicyRefusalIgnoresOtherErrors(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type":     "result",
		"subtype":  "error_during_execution",
		"is_error": true,
		"result":   "Error: command failed with exit code 1",
	})
	if IsUsagePolicyRefusal(line) {
		t.Error("a non-refusal error result must not be treated as a usage-policy refusal")
	}
}

// A successful result, a non-result event that merely mentions the policy text,
// and a malformed line are all non-refusals.
func TestIsUsagePolicyRefusalRejectsNonRefusals(t *testing.T) {
	success := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "done",
	})
	// The refusal text also appears in an assistant text block earlier in the
	// stream; only the terminal result event counts, never an assistant turn.
	assistantEcho := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "unable to respond to this request, which appears to violate our Usage Policy"},
			},
		},
	})
	for name, line := range map[string]string{
		"success":        success,
		"assistant echo": assistantEcho,
		"malformed":      "{not json",
		"empty":          "",
	} {
		if IsUsagePolicyRefusal(line) {
			t.Errorf("%s should not be a usage-policy refusal", name)
		}
	}
}

// The terminal spending-cap abort (BEH-494): an is_error result whose text is
// the billing/usage-cap message ("Spending cap reached resets 8:20am"). The
// session is killed before doing any real work, so the harness must surface it
// as a distinct retry-after-reset class — not the generic "never ran" failure.
func TestIsSpendingCapAbortDetectsTheCapResult(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type":     "result",
		"subtype":  "success",
		"is_error": true,
		"result":   "Spending cap reached resets 8:20am",
	})
	if !IsSpendingCapAbort(line) {
		t.Error("expected the spending-cap abort result to be detected")
	}
}

// The cap detector must not fire on a non-cap error, on the sibling usage-policy
// refusal (the two retryable classes stay distinct), on a success, or on a
// malformed line.
func TestIsSpendingCapAbortRejectsNonCaps(t *testing.T) {
	otherError := mustJSON(t, map[string]any{
		"type": "result", "subtype": "error_during_execution", "is_error": true,
		"result": "Error: command failed with exit code 1",
	})
	usagePolicyRefusal := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"result": "API Error: Claude Code is unable to respond to this request, which appears to violate our Usage Policy.",
	})
	success := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "done",
	})
	for name, line := range map[string]string{
		"other error":          otherError,
		"usage-policy refusal": usagePolicyRefusal,
		"success":              success,
		"malformed":            "{not json",
		"empty":                "",
	} {
		if IsSpendingCapAbort(line) {
			t.Errorf("%s should not be a spending-cap abort", name)
		}
	}
}

// A spending-cap abort that strikes at session start can ship ONLY the synthetic
// assistant turn — `model:"<synthetic>"` carrying "Spending cap reached" — and not
// the terminal is_error result the result-based detector keys off (BEH-568). The
// detector must recognise that shape too, so classification doesn't depend on the
// result event always being present.
func TestIsSpendingCapAbortDetectsTheSyntheticAssistantTurn(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model":   "<synthetic>",
			"content": []any{map[string]any{"type": "text", "text": "Spending cap reached resets 7:30am"}},
		},
	})
	if !IsSpendingCapAbort(line) {
		t.Error("expected the synthetic spending-cap assistant turn to be detected")
	}
}

// The synthetic gate matters: a real assistant turn that merely quotes "Spending
// cap reached" (e.g. an agent discussing this very ticket) is not a cap abort. Only
// the `model:"<synthetic>"` turn is.
func TestIsSpendingCapAbortIgnoresNonSyntheticCapText(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model":   "claude-opus-4-8",
			"content": []any{map[string]any{"type": "text", "text": "The finding says 'Spending cap reached' is the marker."}},
		},
	})
	if IsSpendingCapAbort(line) {
		t.Error("a real assistant turn quoting the cap text must not be a cap abort")
	}
}

// The review verdict (BEH-525): the /review-worktree session emits its seven-lens
// report as an assistant text block led by the "## Review:" header. The harness
// keys off that header to tell whether the qualitative review actually ran — so a
// session OOM-killed mid-gate (which emits no report) isn't silently treated as a
// full review once the host-side gates pass.
func TestIsReviewVerdictDetectsTheReportHeader(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "## Review: feat/beh-499  (BEH-499 — FormField aria-describedby)   3 files, +40/-2\n\nGates: lint ✓"},
			},
		},
	})
	if !IsReviewVerdict(line) {
		t.Error("expected the review report header to be detected as a verdict")
	}
}

// A session that never reached the report — tool_use, a bare result, a non-review
// assistant turn, or a malformed line — has emitted no verdict.
func TestIsReviewVerdictRejectsNonVerdicts(t *testing.T) {
	toolUse := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "tool_use", "name": "Bash"}},
		},
	})
	otherText := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "Running pnpm run lint first."}},
		},
	})
	result := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "done",
	})
	for name, line := range map[string]string{
		"tool use":   toolUse,
		"other text": otherText,
		"result":     result,
		"malformed":  "{not json",
		"empty":      "",
	} {
		if IsReviewVerdict(line) {
			t.Errorf("%s should not be a review verdict", name)
		}
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

// An is_error result whose subtype is "success" (the shape the spending-cap and
// other limit aborts ship — BEH-495) must not narrate the contradictory
// "✗ session success": the cross says fail, the word says success. The mark and
// the text have to agree, so the success word is coerced to an error word.
func TestNarratesErrorResultWithSuccessSubtypeIsNotContradictory(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type":        "result",
		"subtype":     "success",
		"is_error":    true,
		"duration_ms": 2000,
	})

	out, ok := Narrate(line)
	if !ok {
		t.Fatal("expected a narration")
	}
	if !strings.Contains(out, "✗") {
		t.Errorf("error result narration %q lacks the ✗ error mark", out)
	}
	if strings.Contains(out, "success") {
		t.Errorf("error result narration %q echoes the contradictory \"success\" subtype", out)
	}
	if !strings.Contains(out, "(2s)") {
		t.Errorf("error result narration %q lacks rounded duration", out)
	}
}
