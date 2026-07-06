// Package stream turns claude's stream-json output lines into concise console
// narration.
package stream

import (
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/loopstream"
)

type event struct {
	Type       string   `json:"type"`
	Subtype    string   `json:"subtype"`
	IsError    bool     `json:"is_error"`
	Result     string   `json:"result"`
	NumTurns   int      `json:"num_turns"`
	DurationMS *float64 `json:"duration_ms"`
	Message    struct {
		// Model is the model that produced the turn. A spending-cap abort ships a
		// "<synthetic>" turn the API injects rather than a real model response.
		Model   string `json:"model"`
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

// syntheticModel is the model id the API stamps on a turn it injected itself
// rather than a real model response. A spending-cap abort ships its
// "Spending cap reached" notice as a `model:"<synthetic>"` assistant turn.
const syntheticModel = "<synthetic>"

// IsSpendingCapAbort reports whether a stream-json line is a terminal
// spending-cap abort (BEH-494) — either the `is_error` result whose `result`
// text is the billing/usage-cap message, OR the `model:"<synthetic>"` assistant
// turn carrying that message (BEH-568). The cap can strike at session start and
// ship only the synthetic turn, before any terminal result event, so keying off
// the result alone risks mislabelling it "never ran"; recognising both shapes
// makes the retry-after-reset classification robust. The session is killed before
// doing any real work, so — like a usage-policy refusal — the harness treats it as
// a distinct, retry-after-reset class. The synthetic gate keeps a real assistant
// turn that merely quotes the cap text from counting; a malformed line is never a
// cap abort (it just returns false).
func IsSpendingCapAbort(line string) bool {
	_, ok := capAbortText(line)
	return ok
}

// capAbortText returns the spending-cap abort message text carried by a stream-json
// line — from the terminal `is_error` result OR the `model:"<synthetic>"` assistant
// turn (the two shapes IsSpendingCapAbort recognises) — and ok=false when the line is
// not a cap abort. It is the single source of truth for "is this a cap abort, and what
// did it say", so IsSpendingCapAbort and SpendingCapResetTime can't drift apart.
func capAbortText(line string) (string, bool) {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return "", false
	}
	if e.Type == "result" && e.IsError && strings.Contains(e.Result, spendingCapAbortMarker) {
		return e.Result, true
	}
	if e.Type == "assistant" && e.Message.Model == syntheticModel {
		for _, block := range e.Message.Content {
			if block.Type == "text" && strings.Contains(block.Text, spendingCapAbortMarker) {
				return block.Text, true
			}
		}
	}
	return "", false
}

// capResetRe matches the reset clock-time the cap message carries ("resets 8:40am",
// "resets 11:05 PM"). The minutes are optional so a bare "resets 8am" still parses;
// the meridiem is required (an unqualified hour is too ambiguous to trust).
var capResetRe = regexp.MustCompile(`(?i)resets\s+(\d{1,2})(?::(\d{2}))?\s*(am|pm)`)

// SpendingCapResetTime extracts the reset clock-time from a spending-cap abort line and
// resolves it to the next occurrence of that wall-clock time strictly after now, in
// now's location (BEH-708). The abort message names the exact reset ("Spending cap
// reached resets 8:40am"), which the harness historically discarded — parsing it lets
// the cap backoff wait until the cap actually clears instead of a fixed guess. ok=false
// when the line is not a cap abort or carries no parseable reset time, in which case the
// caller keeps its fixed fallback backoff. The timezone is assumed to be now's location
// (the message carries none); the caller applies a margin and a sanity ceiling so a
// timezone-skewed or wrapped-to-tomorrow resolution degrades to the fixed backoff rather
// than an absurd wait.
func SpendingCapResetTime(line string, now time.Time) (time.Time, bool) {
	text, ok := capAbortText(line)
	if !ok {
		return time.Time{}, false
	}
	m := capResetRe.FindStringSubmatch(text)
	if m == nil {
		return time.Time{}, false
	}
	hour, err := strconv.Atoi(m[1])
	if err != nil || hour < 1 || hour > 12 {
		return time.Time{}, false
	}
	minute := 0
	if m[2] != "" {
		if minute, err = strconv.Atoi(m[2]); err != nil || minute > 59 {
			return time.Time{}, false
		}
	}
	// 12-hour → 24-hour: 12am is midnight (00:00), 12pm is noon (12:00).
	switch strings.ToLower(m[3]) {
	case "am":
		if hour == 12 {
			hour = 0
		}
	case "pm":
		if hour != 12 {
			hour += 12
		}
	}
	reset := time.Date(now.Year(), now.Month(), now.Day(), hour, minute, 0, 0, now.Location())
	if !reset.After(now) {
		reset = reset.Add(24 * time.Hour) // the named time already passed today → next occurrence
	}
	return reset, true
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

// reviewBlockedMarker is the disposition the /review-worktree report carries when
// it could NOT autonomously resolve a Blocker/Important finding and a human
// decision is required (BEH-580). The autonomous pipeline has no approver, so the
// reviewer self-resolves what it can (applies a safe fix, or accepts-and-documents
// a deliberate change) and emits "Disposition: clear"; only a genuinely unresolved
// finding emits "Disposition: blocked". Matched as a substring so the trailing
// reason text doesn't have to be exact. The "clear" disposition deliberately does
// NOT contain this marker, so a clear verdict never trips it.
const reviewBlockedMarker = "Disposition: blocked"

// IsReviewBlocked reports whether a stream-json line is a /review-worktree verdict
// declaring a blocked disposition (BEH-580): an assistant text block carrying BOTH
// the "## Review:" report header AND "Disposition: blocked". That pairing means the
// review ran and found a Blocker/Important finding it could not autonomously
// resolve — so the harness must fail the push closed and keep the worktree rather
// than open a PR with the finding unaddressed (the BEH-439 leak). Both markers are
// required so a "Disposition: blocked" mentioned outside a review report can't trip
// it; a malformed line is never blocked.
func IsReviewBlocked(line string) bool {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return false
	}
	if e.Type != "assistant" {
		return false
	}
	for _, block := range e.Message.Content {
		if block.Type == "text" &&
			strings.Contains(block.Text, reviewVerdictMarker) &&
			strings.Contains(block.Text, reviewBlockedMarker) {
			return true
		}
	}
	return false
}

// turnZeroNoOpMaxTurns is the num_turns ceiling below which a subtype="success",
// non-error result that produced NO final output text is read as a degenerate
// turn-0 no-op (BEH-709) rather than a real success. The crash the pinned CLI
// ships when its `!`+backtick inline-bash directive errors reports exactly
// num_turns=3 with an empty result; a session that did any real work reports both
// a non-empty final result and many more turns, so this window cannot catch one.
const turnZeroNoOpMaxTurns = 3

// isTurnZeroNoOp reports whether a result event is the pinned CLI's turn-0 no-op:
// a subtype="success", non-error terminal result that produced NO final output
// text (an empty `result`) in a handful of turns. The `!`+backtick directive in
// the prompt runs host-of-sandbox, errors, and the session degenerates to a
// zero-work run that still exits 0 with is_error=false — so keying off is_error /
// exit code alone reads it as success and masks the crash (BEH-709). Requiring a
// POSITIVE num_turns <= turnZeroNoOpMaxTurns keeps a minimal result event that
// simply omits num_turns (num_turns=0) from being mislabelled: only a reported,
// genuinely low turn count paired with an empty result is the crash signature.
func isTurnZeroNoOp(e event) bool {
	return e.Type == "result" && e.Subtype == "success" && !e.IsError &&
		strings.TrimSpace(e.Result) == "" &&
		e.NumTurns >= 1 && e.NumTurns <= turnZeroNoOpMaxTurns
}

// IsTurnZeroNoOp reports whether a stream-json line is the pinned CLI's turn-0
// no-op result (BEH-709), so the session layer can surface it onto its Outcome the
// way it does a usage-policy refusal or spending-cap abort. It is the terminal
// masked-success shape: a subtype="success", non-error result that produced no
// output in a handful of turns because the prompt's `!`+backtick directive errored
// host-of-sandbox and the session degenerated to zero real work while still
// exiting 0. A malformed line is never a no-op (it just returns false).
func IsTurnZeroNoOp(line string) bool {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return false
	}
	return isTurnZeroNoOp(e)
}

// Narrate turns one line of claude's `--output-format stream-json` into a concise
// console narration string, returning ok=false to skip it. The full raw stream
// is teed to the per-run jsonl regardless; this is only the human-friendly
// summary shown when the harness is not running --verbose. Never panics — a
// malformed line is simply skipped so a single bad chunk can't kill the run. It is
// a thin wrapper over NarrateRecord that drops the structured kind, so existing
// console-only callers are unaffected.
func Narrate(line string) (string, bool) {
	msg, _, ok := NarrateRecord(line)
	return msg, ok
}

// NarrateRecord is Narrate plus the structured loopstream.Kind for the line, so a
// caller mirroring narration into the global event stream (ADR-0005) can classify
// it without re-parsing the prose: a tool_use turn is KindToolUse, a terminal
// result is KindSessionResult. ok=false (and an empty kind) for an
// uninteresting/malformed line, exactly as Narrate.
func NarrateRecord(line string) (string, loopstream.Kind, bool) {
	var e event
	if err := json.Unmarshal([]byte(line), &e); err != nil {
		return "", "", false
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
		// A non-error "success" result that produced no output in <=3 turns is the
		// turn-0 no-op (BEH-709), not a real success. Surface it with the ✗ mark and a
		// distinct word so it is never read as the harness's success signal — the
		// stage's own ground-truth check (e.g. an absent findings dropbox) then routes
		// it to the retry/never-ran class instead of a masked "✓ session success".
		if isTurnZeroNoOp(e) {
			mark = "✗"
			subtype = "no-op (turn-0 crash — no output)"
		}
		return fmt.Sprintf("%s session %s%s", mark, subtype, secs), loopstream.KindSessionResult, true
	}

	if e.Type == "assistant" {
		for _, block := range e.Message.Content {
			if block.Type == "tool_use" && block.Name != "" {
				return "⚒ " + block.Name, loopstream.KindToolUse, true
			}
		}
	}

	return "", "", false
}
