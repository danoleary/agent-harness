package github

import (
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// bodyString extracts the "body" field from a recorded POST/PATCH body.
func bodyString(body any) string {
	m, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m["body"].(string)
	return s
}

// TestFileFindingCreatesIssue pins the findings sink: filing creates an issue
// carrying the findings label, the "surfaced during" attribution, and — when the
// finding has a dedup key — the machine-readable finding-key marker a later run
// dedups on. The Key of the created issue is its #number.
func TestFileFindingCreatesIssue(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"POST /repos/acme/widgets/issues": map[string]any{
			"number":   123,
			"html_url": "https://github.com/acme/widgets/issues/123",
		},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	got, err := c.FileFinding(
		findings.Finding{Title: "Flaky worktree setup", Body: "The install races.", Kind: "setup", Key: "worktree-install-race"},
		tracker.FileFindingOptions{RelatedKey: "BEH-637", TeamID: "acme/widgets"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "#123" || got.URL != "https://github.com/acme/widgets/issues/123" {
		t.Errorf("created = %+v, want #123 + url", got)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected one create call, got %d", len(*calls))
	}
	m, _ := (*calls)[0].body.(map[string]any)
	if m["title"] != "Flaky worktree setup" {
		t.Errorf("title = %v", m["title"])
	}
	body, _ := m["body"].(string)
	if !strings.Contains(body, "The install races.") {
		t.Error("body should carry the finding text")
	}
	if !strings.Contains(body, "worktree-install-race") {
		t.Error("body should carry the finding-key dedup marker")
	}
	if !strings.Contains(body, "BEH-637") {
		t.Error("body should attribute the surfacing ticket")
	}
	ls, _ := m["labels"].([]string)
	if len(ls) != 1 || ls[0] != "agent-harness" {
		t.Errorf("labels = %v, want [agent-harness]", ls)
	}
}

// searchPath is the exact list-issues request SearchFindings issues: the repo's
// open, findings-labelled issues.
const searchPath = "GET /repos/acme/widgets/issues?labels=agent-harness&per_page=100&state=open"

// TestSearchFindingsScopedToLabel lists the repo's open harness findings,
// recovering each one's dedup key from its body marker and screening out pull
// requests (the issues API returns PRs too).
func TestSearchFindingsScopedToLabel(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		searchPath: []any{
			map[string]any{"number": 50, "title": "Keyed finding", "body": "text\n\n<!-- finding-key: race-x -->"},
			map[string]any{"number": 51, "title": "Unkeyed finding", "body": "no marker"},
			map[string]any{"number": 52, "title": "A PR", "pull_request": map[string]any{"url": "u"}, "body": ""},
		},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	got, err := c.SearchFindings("acme/widgets")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 findings (PR screened out), got %d: %+v", len(got), got)
	}
	if got[0].Identifier != "#50" || got[0].Key != "race-x" {
		t.Errorf("finding[0] = %+v, want #50 key race-x", got[0])
	}
	if got[1].Identifier != "#51" || got[1].Key != "" {
		t.Errorf("finding[1] = %+v, want #51 empty key", got[1])
	}
	if got[0].Closed {
		t.Error("an open finding must not be marked Closed")
	}
}

// TestRecordOccurrenceBumpsAndComments bumps an already-tracked finding's
// occurrence marker (defaulting to 1 for the original filing → 2) via a body
// PATCH and leaves a breadcrumb comment, returning the new count.
func TestRecordOccurrenceBumpsAndComments(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/issues/50":           map[string]any{"number": 50, "body": "the finding text"},
		"PATCH /repos/acme/widgets/issues/50":         map[string]any{"number": 50},
		"POST /repos/acme/widgets/issues/50/comments": map[string]any{},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	n, err := c.RecordOccurrence("#50", "BEH-700")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	var patched, commented bool
	for _, cl := range *calls {
		if cl.method == "PATCH" && cl.path == "/repos/acme/widgets/issues/50" {
			patched = true
			if !strings.Contains(bodyString(cl.body), "occurrences: 2") {
				t.Errorf("patched body missing occurrences: 2, got %q", bodyString(cl.body))
			}
		}
		if cl.method == "POST" && cl.path == "/repos/acme/widgets/issues/50/comments" {
			commented = true
			if !strings.Contains(bodyString(cl.body), "BEH-700") {
				t.Error("comment should reference the related ticket")
			}
		}
	}
	if !patched || !commented {
		t.Errorf("expected a body PATCH and a comment, patched=%v commented=%v", patched, commented)
	}
}

// TestAddCommentPostsToIssue posts a comment straight to the issue by its number —
// GitHub needs no id resolution (unlike Linear's UUID lookup).
func TestAddCommentPostsToIssue(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"POST /repos/acme/widgets/issues/9/comments": map[string]any{},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	if err := c.AddComment("#9", "stranded work note"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/repos/acme/widgets/issues/9/comments" {
		t.Fatalf("expected one comment POST, got %+v", *calls)
	}
	if bodyString((*calls)[0].body) != "stranded work note" {
		t.Errorf("comment body = %q", bodyString((*calls)[0].body))
	}
}
