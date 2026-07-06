package github

import (
	"testing"
	"time"
)

// claimsPath is the list-issues request ListInProgressClaims issues: the repo's
// open, in-progress-labelled issues (the agent-claimed set).
const claimsPath = "GET /repos/acme/widgets/issues?labels=in-progress&per_page=100&state=open"

// TestListInProgressClaimsDerivesFromTimeline pins the reaper read: each claimed
// issue's StartedAt comes from when the in-progress label was applied, and
// HasLinkedPR is true when a pull request cross-references the issue. Both are read
// from the issue timeline, so a claim stranded by a dead agent (no PR past the TTL)
// can be released back to Todo (BEH-677).
func TestListInProgressClaimsDerivesFromTimeline(t *testing.T) {
	claimedAt := "2026-07-05T10:00:00Z"
	tr, _ := fakeTransport(map[string]any{
		claimsPath: []any{
			map[string]any{"number": 9},
			map[string]any{"number": 10},
		},
		"GET /repos/acme/widgets/issues/9/timeline?per_page=100": []any{
			map[string]any{"event": "labeled", "label": map[string]any{"name": "in-progress"}, "created_at": claimedAt},
			map[string]any{"event": "cross-referenced", "source": map[string]any{
				"type": "issue", "issue": map[string]any{"number": 200, "pull_request": map[string]any{"url": "u"}},
			}},
		},
		"GET /repos/acme/widgets/issues/10/timeline?per_page=100": []any{
			map[string]any{"event": "labeled", "label": map[string]any{"name": "in-progress"}, "created_at": claimedAt},
		},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	claims, err := c.ListInProgressClaims()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("expected 2 claims, got %d: %+v", len(claims), claims)
	}
	want, _ := time.Parse(time.RFC3339, claimedAt)
	if claims[0].Identifier != "#9" || !claims[0].StartedAt.Equal(want) || !claims[0].HasLinkedPR {
		t.Errorf("claim[0] = %+v, want #9 startedAt=%v HasLinkedPR=true", claims[0], want)
	}
	if claims[1].Identifier != "#10" || claims[1].HasLinkedPR {
		t.Errorf("claim[1] = %+v, want #10 HasLinkedPR=false", claims[1])
	}
}
