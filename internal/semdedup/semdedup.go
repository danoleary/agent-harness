// Package semdedup is the host-side semantic dedup matcher (BEH-573): given a new
// harness finding and the set of open agent-harness findings, it asks a cheap
// model whether the finding is the same root-cause class as one of them, so a
// recurrence worded differently than the open issue still dedups against it
// (exact key/title match — internal/filing.matchKey — only catches verbatim
// repeats). It satisfies filing.SemanticMatcher.
//
// It is best-effort by construction (ADR-0001): the model logic (prompt + parse)
// is pure and unit-tested over an injected Complete func; the live Anthropic
// transport is thin and kept out of the unit suite, mirroring linear.NewTransport.
// Any transport error propagates to filing.Router, which degrades to filing the
// finding — the pre-semantic behaviour — never a crash.
package semdedup

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// noMatch is the sentinel the model returns when a finding matches no open issue.
const noMatch = "NONE"

// Complete sends a prompt to a model and returns its text reply. The real
// implementation (NewAnthropicComplete) holds the host-only API key; tests inject
// a fake, exactly as linear.Transport is faked in the linear suite.
type Complete func(prompt string) (string, error)

// Matcher answers filing.SemanticMatcher by composing a Complete with the pure
// prompt/parse logic.
type Matcher struct {
	complete Complete
}

// New builds a Matcher over the given Complete.
func New(complete Complete) *Matcher { return &Matcher{complete: complete} }

// MatchFinding asks the model which open finding f duplicates, returning that
// issue's identifier or "" for none. A Complete error propagates so the caller
// degrades; a reply that names no open identifier resolves to "".
func (m *Matcher) MatchFinding(f findings.Finding, open []tracker.ExistingFinding) (string, error) {
	if len(open) == 0 {
		return "", nil
	}
	reply, err := m.complete(buildPrompt(f, open))
	if err != nil {
		return "", err
	}
	return parseVerdict(reply, open), nil
}

// buildPrompt renders the dedup question: the new finding, the numbered open
// candidates (identifier + title + dedup key), and a strict instruction to reply
// with exactly one open identifier or the NONE sentinel. Titles/keys give the
// model the signal to spot a same-class recurrence under different prose.
func buildPrompt(f findings.Finding, open []tracker.ExistingFinding) string {
	var b strings.Builder
	b.WriteString("You are deduplicating engineering-harness findings.\n")
	b.WriteString("A NEW finding was just surfaced. Decide whether it describes the SAME root-cause class as any of the EXISTING open findings below — same underlying problem, even if worded differently. Different symptoms of the same root cause count as the same class; superficially similar but distinct root causes do NOT.\n\n")
	b.WriteString("NEW finding:\n")
	b.WriteString("Title: " + f.Title + "\n")
	if strings.TrimSpace(f.Body) != "" {
		b.WriteString("Body: " + f.Body + "\n")
	}
	b.WriteString("\nEXISTING open findings:\n")
	for _, e := range open {
		line := e.Identifier + ": " + e.Title
		if strings.TrimSpace(e.Key) != "" {
			line += " [key: " + e.Key + "]"
		}
		b.WriteString(line + "\n")
	}
	b.WriteString("\nReply with EXACTLY ONE of the existing identifiers (e.g. " + open[0].Identifier + ") if the new finding is the same class as it, or the single word " + noMatch + " if it matches none. Reply with only that token, nothing else.")
	return b.String()
}

// parseVerdict extracts the matched identifier from the model reply: the first
// open identifier that appears as a whole token in the reply, or "" (no match)
// for NONE, an empty reply, or any identifier not in the candidate set. Being
// strict here — only ever returning an identifier we actually offered — is what
// lets filing trust the match without re-verifying it.
func parseVerdict(reply string, open []tracker.ExistingFinding) string {
	tokens := strings.FieldsFunc(reply, func(r rune) bool {
		// Split on everything but identifier characters (letters, digits, hyphen),
		// so "BEH-572." or "(BEH-572)" still yields the bare identifier token.
		return !(r == '-' || (r >= '0' && r <= '9') || (r >= 'A' && r <= 'Z') || (r >= 'a' && r <= 'z'))
	})
	for _, tok := range tokens {
		if strings.EqualFold(tok, noMatch) {
			return ""
		}
		for _, e := range open {
			if strings.EqualFold(tok, e.Identifier) {
				return e.Identifier
			}
		}
	}
	return ""
}

const (
	anthropicURL     = "https://api.anthropic.com/v1/messages"
	anthropicVersion = "2023-06-01"
	// maxTokens is tiny on purpose: the reply is a single identifier token.
	maxTokens = 16
)

// NewAnthropicComplete builds the real host-side Complete over the Anthropic
// Messages API. It holds the host-only ANTHROPIC_API_KEY (sent via x-api-key — a
// subscription OAuth token is rejected on that header, BEH-316, so callers only
// wire this when a real API key is present and otherwise leave the matcher unset).
// Kept thin and out of the unit suite; the Matcher logic is exercised with a fake
// Complete.
func NewAnthropicComplete(apiKey, model string) Complete {
	return func(prompt string) (string, error) {
		body, err := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": maxTokens,
			"messages":   []map[string]any{{"role": "user", "content": prompt}},
		})
		if err != nil {
			return "", err
		}
		req, err := http.NewRequest(http.MethodPost, anthropicURL, bytes.NewReader(body))
		if err != nil {
			return "", err
		}
		req.Header.Set("content-type", "application/json")
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", anthropicVersion)

		res, err := http.DefaultClient.Do(req)
		if err != nil {
			return "", err
		}
		defer res.Body.Close()
		payload, err := io.ReadAll(res.Body)
		if err != nil {
			return "", err
		}
		if res.StatusCode < 200 || res.StatusCode >= 300 {
			return "", fmt.Errorf("Anthropic HTTP %d: %s", res.StatusCode, string(payload))
		}
		var parsed struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		}
		if err := json.Unmarshal(payload, &parsed); err != nil {
			return "", err
		}
		var text strings.Builder
		for _, c := range parsed.Content {
			if c.Type == "text" {
				text.WriteString(c.Text)
			}
		}
		return text.String(), nil
	}
}
