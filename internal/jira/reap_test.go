package jira

import (
	"testing"
	"time"
)

// claimsSearchPath is the search/jql request ListInProgressClaims issues: the
// in-progress JQL, keyed to the status-change date used as the claim time.
const claimsSearchPath = "GET /rest/api/2/search/jql?fields=statuscategorychangedate&jql=status+%3D+%22In+Progress%22&maxResults=50"

// TestListInProgressClaimsDerivesSignals pins the reaper read: each claimed
// issue's StartedAt comes from its status-category change date, and HasLinkedPR is
// true when a remote link points at a pull request. Both let a claim stranded by a
// dead agent (no PR past the TTL) be released back to Todo (BEH-677).
func TestListInProgressClaimsDerivesSignals(t *testing.T) {
	claimedAt := "2026-07-05T10:00:00.000+0000"
	tr, _ := fakeTransport(map[string]any{
		claimsSearchPath: map[string]any{"issues": []any{
			map[string]any{"key": "PROJ-9", "fields": map[string]any{"statuscategorychangedate": claimedAt}},
			map[string]any{"key": "PROJ-10", "fields": map[string]any{"statuscategorychangedate": claimedAt}},
		}},
		"GET /rest/api/2/issue/PROJ-9/remotelink": []any{
			map[string]any{"object": map[string]any{"url": "https://github.com/acme/widgets/pull/200"}},
		},
		"GET /rest/api/2/issue/PROJ-10/remotelink": []any{},
	})
	c := NewClient(tr, baseURL, selectOpts)

	claims, err := c.ListInProgressClaims()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(claims) != 2 {
		t.Fatalf("expected 2 claims, got %d: %+v", len(claims), claims)
	}
	want, _ := time.Parse("2006-01-02T15:04:05.000-0700", claimedAt)
	if claims[0].Identifier != "PROJ-9" || !claims[0].StartedAt.Equal(want) || !claims[0].HasLinkedPR {
		t.Errorf("claim[0] = %+v, want PROJ-9 startedAt=%v HasLinkedPR=true", claims[0], want)
	}
	if claims[1].Identifier != "PROJ-10" || claims[1].HasLinkedPR {
		t.Errorf("claim[1] = %+v, want PROJ-10 HasLinkedPR=false", claims[1])
	}
}
