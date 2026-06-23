// Package stream turns claude's stream-json output lines into concise console
// narration.
package stream

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
)

type event struct {
	Type       string   `json:"type"`
	Subtype    string   `json:"subtype"`
	IsError    bool     `json:"is_error"`
	Result     string   `json:"result"`
	DurationMS *float64 `json:"duration_ms"`
	Message    struct {
		Content []struct {
			Type string `json:"type"`
			Name string `json:"name"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"message"`
}

// usagePolicyRefusalMarker is the stable core of Claude Code's usage-policy
// refusal message. Matched as a substring so the surrounding remediation text
// (which varies — it suggests a model to switch to) doesn't have to be exact.
const usagePolicyRefusalMarker = "unable to respond to this request, which appears to violate our Usage Policy"

// IsUsagePolicyRefusal reports whether a stream-json line is the terminal
// usage-policy refusal *result* event (BEH-389): an `is_error` result whose
// `result` text is the classifier's "unable to respond … violate our Usage
// Policy" message. This is a known intermittent false-positive that
// disproportionately strikes long agentic sessions; the harness treats it as
// retryable rather than a real failure. Only the terminal result event counts —
// the same text appearing in an earlier assistant turn is not a refusal — and a
// malformed line is never a refusal (it just returns false).
func IsUsagePolicyRefusal(line string) bool {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return false
	}
	return e.Type == "result" && e.IsError && strings.Contains(e.Result, usagePolicyRefusalMarker)
}

// spendingCapAbortMarker is the stable core of the billing/usage spending-cap
// abort result ("Spending cap reached resets 8:20am"). Matched as a substring so
// the trailing reset time (which varies) doesn't have to be exact.
const spendingCapAbortMarker = "Spending cap reached"

// IsSpendingCapAbort reports whether a stream-json line is the terminal
// spending-cap abort *result* event (BEH-494): an `is_error` result whose
// `result` text is the billing/usage-cap message. The session is killed before
// doing any real work, so — like a usage-policy refusal — the harness treats it
// as a distinct, retry-after-reset class rather than the generic "never ran"
// failure. Only the terminal result event counts; a malformed line is never a
// cap abort (it just returns false).
func IsSpendingCapAbort(line string) bool {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return false
	}
	return e.Type == "result" && e.IsError && strings.Contains(e.Result, spendingCapAbortMarker)
}

// reviewVerdictMarker is the stable lead of the /review-worktree report (step 5 of
// the skill: "## Review: <branch> …"). The qualitative seven-lens review ends in
// this report, so its presence in an assistant turn is the signal that the review
// actually ran — keyed off the header prefix so the variable branch/intent/diff
// stats that follow don't have to match.
const reviewVerdictMarker = "## Review:"

// IsReviewVerdict reports whether a stream-json line is the /review-worktree
// session emitting its verdict — an assistant turn carrying a text block led by
// the "## Review:" report header (BEH-525). The harness uses this to tell whether
// the qualitative review completed: a session OOM-killed mid-gate (exit 137) emits
// no report, so the harness must not let a green host-side gate re-run masquerade
// as a full review. Only an assistant text block counts (the agent's own output,
// not a result event or tool call); a malformed line is never a verdict.
func IsReviewVerdict(line string) bool {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return false
	}
	if e.Type != "assistant" {
		return false
	}
	for _, block := range e.Message.Content {
		if block.Type == "text" && strings.Contains(block.Text, reviewVerdictMarker) {
			return true
		}
	}
	return false
}

// Narrate turns one line of claude's `--output-format stream-json` into a concise
// console narration string, returning ok=false to skip it. The full raw stream
// is teed to the per-run jsonl regardless; this is only the human-friendly
// summary shown when the harness is not running --verbose. Never panics — a
// malformed line is simply skipped so a single bad chunk can't kill the run.
func Narrate(line string) (string, bool) {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return "", false
	}

	if e.Type == "result" {
		mark := "✓"
		if e.IsError {
			mark = "✗"
		}
		secs := ""
		if e.DurationMS != nil {
			secs = fmt.Sprintf(" (%ds)", int(math.Round(*e.DurationMS/1000)))
		}
		subtype := e.Subtype
		if subtype == "" {
			subtype = "ended"
		}
		// An is_error result that still carries subtype "success" (the cap/limit
		// aborts ship exactly this shape — BEH-495) would narrate the
		// contradictory "✗ session success". Coerce the word so it agrees with the
		// ✗ mark.
		if e.IsError && subtype == "success" {
			subtype = "error"
		}
		return fmt.Sprintf("%s session %s%s", mark, subtype, secs), true
	}

	if e.Type == "assistant" {
		for _, block := range e.Message.Content {
			if block.Type == "tool_use" && block.Name != "" {
				return "⚒ " + block.Name, true
			}
		}
	}

	return "", false
}
