package linear

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// inProgressTransport answers the ListInProgressClaims query with the given issue
// nodes, recording the calls it saw.
func inProgressTransport(t *testing.T, nodes ...map[string]any) (Transport, *[]call) {
	t.Helper()
	var calls []call
	tr := func(query string, variables map[string]any) (json.RawMessage, error) {
		calls = append(calls, call{query: query, variables: variables})
		ns := make([]any, 0, len(nodes))
		for _, n := range nodes {
			ns = append(ns, n)
		}
		b, err := json.Marshal(map[string]any{"issues": map[string]any{"nodes": ns}})
		if err != nil {
			t.Fatalf("marshal fake: %v", err)
		}
		return b, nil
	}
	return tr, &calls
}

// claimNode builds one In Progress claim node with the given startedAt and
// attachment URLs.
func claimNode(id, startedAt string, attachmentURLs ...string) map[string]any {
	atts := make([]any, 0, len(attachmentURLs))
	for _, u := range attachmentURLs {
		atts = append(atts, map[string]any{"url": u})
	}
	return map[string]any{
		"identifier":  id,
		"startedAt":   startedAt,
		"attachments": map[string]any{"nodes": atts},
	}
}

// A claim with no attachments parses to no linked PR and a parsed startedAt — the
// raw signal the reaper needs to decide strandedness.
func TestListInProgressClaimsParsesStartedAtAndNoPR(t *testing.T) {
	tr, _ := inProgressTransport(t, claimNode("BEH-451", "2026-07-03T21:34:00Z"))
	claims, err := NewClient(tr, testOptions()).ListInProgressClaims()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claims) != 1 {
		t.Fatalf("got %d claims, want 1", len(claims))
	}
	c := claims[0]
	if c.Identifier != "BEH-451" {
		t.Errorf("Identifier = %q, want BEH-451", c.Identifier)
	}
	want := time.Date(2026, 7, 3, 21, 34, 0, 0, time.UTC)
	if !c.StartedAt.Equal(want) {
		t.Errorf("StartedAt = %v, want %v", c.StartedAt, want)
	}
	if c.HasLinkedPR {
		t.Error("HasLinkedPR = true, want false — a claim with no attachments has no PR")
	}
}

// A GitHub pull-request attachment marks the claim as having a linked PR, so the
// reaper leaves it alone (the work shipped, or is mid-review).
func TestListInProgressClaimsDetectsLinkedPR(t *testing.T) {
	tr, _ := inProgressTransport(t, claimNode("BEH-633", "2026-07-03T20:00:00Z", "https://github.com/example-org/example-repo/pull/733"))
	claims, err := NewClient(tr, testOptions()).ListInProgressClaims()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claims) != 1 || !claims[0].HasLinkedPR {
		t.Errorf("HasLinkedPR = %v, want true — a /pull/ attachment is a linked PR", claims)
	}
}

// A non-PR attachment (e.g. a bare Linear/doc link) does not count as a linked PR.
func TestListInProgressClaimsIgnoresNonPRAttachment(t *testing.T) {
	tr, _ := inProgressTransport(t, claimNode("BEH-451", "2026-07-03T21:34:00Z", "https://linear.app/example-workspace/issue/BEH-451"))
	claims, err := NewClient(tr, testOptions()).ListInProgressClaims()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claims) != 1 || claims[0].HasLinkedPR {
		t.Errorf("HasLinkedPR = %v, want false — a non-PR attachment is not a PR link", claims)
	}
}

// The query must scope to agent-claimed In Progress tickets only: team BEH, the
// started state type, unassigned (a human's In Progress work carries an assignee),
// and ready-for-agent. Reaping must never touch a human's claim.
func TestListInProgressClaimsScopesToAgentClaims(t *testing.T) {
	tr, calls := inProgressTransport(t, claimNode("BEH-451", "2026-07-03T21:34:00Z"))
	if _, err := NewClient(tr, testOptions()).ListInProgressClaims(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected one query, got %d", len(*calls))
	}
	raw, err := json.Marshal((*calls)[0].variables)
	if err != nil {
		t.Fatalf("marshal variables: %v", err)
	}
	vars := string(raw)
	for _, want := range []string{"BEH", "started", "assignee", "null", "ready-for-agent"} {
		if !strings.Contains(vars, want) {
			t.Errorf("query filter missing %q: %s", want, vars)
		}
	}
}
