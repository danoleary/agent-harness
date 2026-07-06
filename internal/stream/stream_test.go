package stream

import (
	"encoding/json"
	"regexp"
	"strings"
	"testing"
	"time"
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

// The abort message names the exact reset time ("resets 8:40am"); SpendingCapResetTime
// extracts it and resolves it to the next occurrence of that wall-clock time strictly
// after now, in now's location — so the cap backoff can wait until the cap actually
// clears rather than a fixed guess (BEH-708). A reset time still ahead today resolves
// to today.
func TestSpendingCapResetTimeResolvesLaterToday(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"result": "Spending cap reached resets 8:40am",
	})
	now := time.Date(2026, 7, 5, 7, 30, 0, 0, time.UTC)
	got, ok := SpendingCapResetTime(line, now)
	if !ok {
		t.Fatal("expected a reset time to be parsed from the cap message")
	}
	want := time.Date(2026, 7, 5, 8, 40, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// When the named reset time already passed today (now is past it), the next occurrence
// is that time tomorrow — the wrap-around the "resets 8:40am" wording implies at, say,
// 9am. (The loop's sanity ceiling keeps a genuine ~24h wrap from producing an absurd
// wait; here we just pin the resolution.)
func TestSpendingCapResetTimeWrapsToTomorrowWhenPassed(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"result": "Spending cap reached resets 8:40am",
	})
	now := time.Date(2026, 7, 5, 9, 0, 0, 0, time.UTC)
	got, ok := SpendingCapResetTime(line, now)
	if !ok {
		t.Fatal("expected a reset time to be parsed")
	}
	want := time.Date(2026, 7, 6, 8, 40, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// PM times convert to 24-hour, and the parser reads the reset time out of the synthetic
// assistant turn (the session-start cap shape) too — not just the result event.
func TestSpendingCapResetTimeHandlesPMFromSyntheticTurn(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model":   "<synthetic>",
			"content": []any{map[string]any{"type": "text", "text": "Spending cap reached resets 11:05pm"}},
		},
	})
	now := time.Date(2026, 7, 5, 10, 0, 0, 0, time.UTC)
	got, ok := SpendingCapResetTime(line, now)
	if !ok {
		t.Fatal("expected a reset time to be parsed from the synthetic turn")
	}
	want := time.Date(2026, 7, 5, 23, 5, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Errorf("got %s, want %s", got, want)
	}
}

// The 12-hour boundary: 12am is midnight (00:00 next day, since it's past now), 12pm is
// noon. Off-by-twelve here would send the backoff to the wrong half of the day.
func TestSpendingCapResetTimeHandlesNoonAndMidnight(t *testing.T) {
	now := time.Date(2026, 7, 5, 6, 0, 0, 0, time.UTC)
	cases := map[string]time.Time{
		"resets 12:00pm": time.Date(2026, 7, 5, 12, 0, 0, 0, time.UTC),
		"resets 12:00am": time.Date(2026, 7, 6, 0, 0, 0, 0, time.UTC),
	}
	for text, want := range cases {
		line := mustJSON(t, map[string]any{
			"type": "result", "subtype": "success", "is_error": true,
			"result": "Spending cap reached " + text,
		})
		got, ok := SpendingCapResetTime(line, now)
		if !ok {
			t.Fatalf("%q: expected a reset time", text)
		}
		if !got.Equal(want) {
			t.Errorf("%q: got %s, want %s", text, got, want)
		}
	}
}

// No reset time to extract → ok=false, so the caller keeps its fixed fallback backoff:
// a cap abort with no parseable "resets HH:MMam", a non-cap line, and a malformed line
// all decline rather than guessing.
func TestSpendingCapResetTimeDeclinesWithoutAParseableReset(t *testing.T) {
	capNoTime := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": true,
		"result": "Spending cap reached",
	})
	nonCap := mustJSON(t, map[string]any{
		"type": "result", "subtype": "success", "is_error": false, "result": "done at 8:40am",
	})
	now := time.Date(2026, 7, 5, 7, 0, 0, 0, time.UTC)
	for name, line := range map[string]string{
		"cap abort without a reset time": capNoTime,
		"non-cap line mentioning a time": nonCap,
		"malformed":                      "{not json",
	} {
		if _, ok := SpendingCapResetTime(line, now); ok {
			t.Errorf("%s should not yield a reset time", name)
		}
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

// The blocked disposition (BEH-580): under the autonomous pipeline there is no
// human to answer the review's approval prompt, so the reviewer must declare its
// push decision in the verdict. A "## Review:" report carrying "Disposition:
// blocked" means an unresolved Blocker/Important finding still needs a human
// decision — the harness must NOT push it. Both markers must be present: the
// blocked disposition is only meaningful inside a review verdict.
func TestIsReviewBlockedDetectsTheBlockedDisposition(t *testing.T) {
	line := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": "text", "text": "## Review: feat/beh-439  (BEH-439 — showView refactor)   4 files, +60/-12\n\nDisposition: blocked — showView behaviour divergence needs a human call"},
			},
		},
	})
	if !IsReviewBlocked(line) {
		t.Error("expected a review verdict declaring a blocked disposition to be detected")
	}
}

// A verdict that declares "Disposition: clear" — everything resolved or
// accepted-and-documented — is not blocked, and neither is a verdict with no
// disposition, a bare "Disposition: blocked" outside any review report, a
// non-verdict turn, or a malformed line. Only "## Review:" + "Disposition:
// blocked" together block the push.
func TestIsReviewBlockedRejectsNonBlocked(t *testing.T) {
	clearVerdict := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "## Review: feat/x  (BEH-1 — intent)   2 files, +5/-1\n\nDisposition: clear — all findings resolved"}},
		},
	})
	verdictNoDisposition := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "## Review: feat/x  (BEH-1 — intent)   2 files, +5/-1"}},
		},
	})
	blockedOutsideReport := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "Disposition: blocked — discussing the contract, not a verdict"}},
		},
	})
	otherText := mustJSON(t, map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "Running pnpm run lint first."}},
		},
	})
	for name, line := range map[string]string{
		"clear verdict":          clearVerdict,
		"verdict no disposition": verdictNoDisposition,
		"blocked outside report": blockedOutsideReport,
		"other text":             otherText,
		"malformed":              "{not json",
		"empty":                  "",
	} {
		if IsReviewBlocked(line) {
			t.Errorf("%s should not be a blocked review verdict", name)
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
