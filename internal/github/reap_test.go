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

// TestListInProgressClaimsIgnoresPRsThatPredateTheClaim pins #87: a pull request
// that cross-referenced the issue before the current claim (say, an earlier merged
// ADR PR that mentioned it) is not evidence of this claim's work, so it must not
// set HasLinkedPR — otherwise a dead claim past the TTL with no branch is never
// reaped. A cross-reference at or after the claim still counts, and one whose time
// cannot be read counts too, failing safe toward not-reaping.
func TestListInProgressClaimsIgnoresPRsThatPredateTheClaim(t *testing.T) {
	prRef := func(number int, at string) map[string]any {
		return map[string]any{"event": "cross-referenced", "created_at": at, "source": map[string]any{
			"type": "issue", "issue": map[string]any{
				"number": number, "state": "closed",
				"pull_request": map[string]any{"url": "u", "merged_at": "2026-10-24T12:00:00Z"},
			},
		}}
	}
	inProgress := func(at string) map[string]any {
		return map[string]any{"event": "labeled", "label": map[string]any{"name": "in-progress"}, "created_at": at}
	}
	tr, _ := fakeTransport(map[string]any{
		claimsPath: []any{
			map[string]any{"number": 74},
			map[string]any{"number": 75},
			map[string]any{"number": 76},
			map[string]any{"number": 77},
		},
		// An older merged PR mentioned the issue before it was claimed.
		"GET /repos/acme/widgets/issues/74/timeline?per_page=100": []any{
			prRef(72, "2026-10-24T09:00:00Z"),
			inProgress("2026-10-25T00:32:00Z"),
		},
		// A PR opened by an earlier claim, then the issue was re-claimed.
		"GET /repos/acme/widgets/issues/75/timeline?per_page=100": []any{
			inProgress("2026-10-24T08:00:00Z"),
			prRef(80, "2026-10-24T08:20:00Z"),
			inProgress("2026-10-25T00:32:00Z"),
		},
		// This claim's own PR.
		"GET /repos/acme/widgets/issues/76/timeline?per_page=100": []any{
			inProgress("2026-10-25T00:32:00Z"),
			prRef(90, "2026-10-25T00:50:00Z"),
		},
		// An untimeable cross-reference.
		"GET /repos/acme/widgets/issues/77/timeline?per_page=100": []any{
			inProgress("2026-10-25T00:32:00Z"),
			prRef(91, ""),
		},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	claims, err := c.ListInProgressClaims()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := map[string]bool{"#74": false, "#75": false, "#76": true, "#77": true}
	if len(claims) != len(want) {
		t.Fatalf("expected %d claims, got %d: %+v", len(want), len(claims), claims)
	}
	for _, cl := range claims {
		if cl.HasLinkedPR != want[cl.Identifier] {
			t.Errorf("%s HasLinkedPR = %v, want %v", cl.Identifier, cl.HasLinkedPR, want[cl.Identifier])
		}
	}
}
