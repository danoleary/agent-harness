package semdedup

import (
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/linear"
)

func openSet() []linear.ExistingFinding {
	return []linear.ExistingFinding{
		{Identifier: "BEH-572", Title: "spending-cap-aborted review still pushes a PR", Key: "review-spending-cap-abort"},
		{Identifier: "BEH-547", Title: "Docker container died mid-session (exit 125)"},
	}
}

// The matcher reports the identifier the model picked, resolved against the open
// set — the headline same-class-different-prose match.
func TestMatchFindingReturnsModelPick(t *testing.T) {
	var gotPrompt string
	m := New(func(prompt string) (string, error) {
		gotPrompt = prompt
		return "BEH-572", nil
	})

	id, err := m.MatchFinding(
		findings.Finding{Title: "review aborted on spend cap yet PR still opened", Body: "no verdict"},
		openSet(),
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "BEH-572" {
		t.Errorf("id = %q, want BEH-572", id)
	}
	// The prompt must carry the candidates so the model can match against them.
	if !strings.Contains(gotPrompt, "BEH-572") || !strings.Contains(gotPrompt, "BEH-547") {
		t.Errorf("prompt missing candidate identifiers: %q", gotPrompt)
	}
	if !strings.Contains(gotPrompt, "review aborted on spend cap") {
		t.Errorf("prompt missing the new finding title: %q", gotPrompt)
	}
}

// NONE → no match (empty id), so filing files the finding as new.
func TestMatchFindingNoneMeansNoMatch(t *testing.T) {
	m := New(func(string) (string, error) { return "NONE", nil })
	id, err := m.MatchFinding(findings.Finding{Title: "a fresh class"}, openSet())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if id != "" {
		t.Errorf("id = %q, want empty (no match)", id)
	}
}

// A Complete error propagates so the caller (filing.File) degrades to filing.
func TestMatchFindingPropagatesError(t *testing.T) {
	m := New(func(string) (string, error) { return "", errBoom })
	if _, err := m.MatchFinding(findings.Finding{Title: "x"}, openSet()); err == nil {
		t.Error("expected the Complete error propagated, got nil")
	}
}

// An empty open set is answered without calling the model (nothing to match).
func TestMatchFindingShortCircuitsEmptyOpenSet(t *testing.T) {
	called := false
	m := New(func(string) (string, error) { called = true; return "", nil })
	id, err := m.MatchFinding(findings.Finding{Title: "x"}, nil)
	if err != nil || id != "" {
		t.Fatalf("got (%q, %v), want (\"\", nil)", id, err)
	}
	if called {
		t.Error("expected the model NOT called for an empty open set")
	}
}

// An identifier the model invents that we never offered must NOT be trusted —
// parseVerdict only ever returns an identifier from the candidate set.
func TestParseVerdictRejectsUnknownIdentifier(t *testing.T) {
	if got := parseVerdict("BEH-999", openSet()); got != "" {
		t.Errorf("parseVerdict(unknown) = %q, want empty", got)
	}
}

// The identifier survives surrounding punctuation/prose the model may add.
func TestParseVerdictExtractsIdentifierFromNoisyReply(t *testing.T) {
	if got := parseVerdict("The match is (BEH-547).", openSet()); got != "BEH-547" {
		t.Errorf("parseVerdict(noisy) = %q, want BEH-547", got)
	}
}

// NONE appearing before any identifier is honored as no-match.
func TestParseVerdictNonePrecedence(t *testing.T) {
	if got := parseVerdict("NONE", openSet()); got != "" {
		t.Errorf("parseVerdict(NONE) = %q, want empty", got)
	}
	if got := parseVerdict("", openSet()); got != "" {
		t.Errorf("parseVerdict(empty) = %q, want empty", got)
	}
}

// buildPrompt surfaces a candidate's dedup key when it has one (extra signal).
func TestBuildPromptIncludesKeyWhenPresent(t *testing.T) {
	p := buildPrompt(findings.Finding{Title: "x"}, openSet())
	if !strings.Contains(p, "review-spending-cap-abort") {
		t.Errorf("prompt missing candidate key: %q", p)
	}
	if !strings.Contains(p, "NONE") {
		t.Errorf("prompt missing the NONE escape instruction: %q", p)
	}
}

var errBoom = boomError("boom")

type boomError string

func (e boomError) Error() string { return string(e) }
