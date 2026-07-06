package jira

import (
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/tracker"
)

// bodyString extracts a top-level string field from a recorded POST/PUT body.
func bodyString(body any, key string) string {
	m, ok := body.(map[string]any)
	if !ok {
		return ""
	}
	s, _ := m[key].(string)
	return s
}

// fieldsMap extracts the nested "fields" object from a recorded create/update body.
func fieldsMap(body any) map[string]any {
	m, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	f, _ := m["fields"].(map[string]any)
	return f
}

// TestFileFindingCreatesIssue pins the findings sink: filing creates a project
// issue carrying the findings label, the configured issue type, the "surfaced
// during" attribution, and — when the finding has a dedup key — the
// machine-readable finding-key marker a later run dedups on. The Key is the new
// issue key, the URL its browse URL.
func TestFileFindingCreatesIssue(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"POST /rest/api/2/issue": map[string]any{"key": "PROJ-123"},
	})
	c := NewClient(tr, baseURL, selectOpts)

	got, err := c.FileFinding(
		findings.Finding{Title: "Flaky worktree setup", Body: "The install races.", Kind: "setup", Key: "worktree-install-race"},
		tracker.FileFindingOptions{RelatedKey: "PROJ-99", TeamID: "PROJ"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "PROJ-123" || got.URL != "https://acme.atlassian.net/browse/PROJ-123" {
		t.Errorf("created = %+v, want PROJ-123 + browse url", got)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected one create call, got %d", len(*calls))
	}
	f := fieldsMap((*calls)[0].body)
	if f["summary"] != "Flaky worktree setup" {
		t.Errorf("summary = %v", f["summary"])
	}
	desc, _ := f["description"].(string)
	if !strings.Contains(desc, "The install races.") {
		t.Error("description should carry the finding text")
	}
	if !strings.Contains(desc, "worktree-install-race") {
		t.Error("description should carry the finding-key dedup marker")
	}
	if !strings.Contains(desc, "PROJ-99") {
		t.Error("description should attribute the surfacing ticket")
	}
	labels, _ := f["labels"].([]string)
	if len(labels) != 1 || labels[0] != "agent-harness" {
		t.Errorf("labels = %v, want [agent-harness]", labels)
	}
	proj, _ := f["project"].(map[string]any)
	if proj["key"] != "PROJ" {
		t.Errorf("project = %v, want key PROJ", proj)
	}
	it, _ := f["issuetype"].(map[string]any)
	if it["name"] != "Task" {
		t.Errorf("issuetype = %v, want Task (default)", it)
	}
}

// findingsSearchPath is the exact search/jql request SearchFindings issues: the
// project's open, findings-labelled issues.
const findingsSearchPath = "GET /rest/api/2/search/jql?fields=summary%2Cdescription&jql=project+%3D+PROJ+AND+labels+%3D+agent-harness+AND+statusCategory+%21%3D+Done&maxResults=50"

// TestSearchFindingsScopedToLabel lists the project's already-filed open harness
// findings (scoped by project + findings label + not-Done), recovering each one's
// dedup key from its description marker.
func TestSearchFindingsScopedToLabel(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		findingsSearchPath: map[string]any{"issues": []any{
			map[string]any{"key": "PROJ-50", "fields": map[string]any{"summary": "Keyed finding", "description": "text\n\n<!-- finding-key: race-x -->"}},
			map[string]any{"key": "PROJ-51", "fields": map[string]any{"summary": "Unkeyed finding", "description": "no marker"}},
		}},
	})
	c := NewClient(tr, baseURL, selectOpts)

	got, err := c.SearchFindings("PROJ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("expected 2 findings, got %d: %+v", len(got), got)
	}
	if got[0].Identifier != "PROJ-50" || got[0].Key != "race-x" {
		t.Errorf("finding[0] = %+v, want PROJ-50 key race-x", got[0])
	}
	if got[1].Identifier != "PROJ-51" || got[1].Key != "" {
		t.Errorf("finding[1] = %+v, want PROJ-51 empty key", got[1])
	}
	if got[0].Closed {
		t.Error("an open finding must not be marked Closed")
	}
}

// TestRecordOccurrenceBumpsAndComments bumps an already-tracked finding's
// occurrence marker (defaulting to 1 → 2) via a description PUT and leaves a
// breadcrumb comment, returning the new count.
func TestRecordOccurrenceBumpsAndComments(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /rest/api/2/issue/PROJ-50?fields=description": map[string]any{"key": "PROJ-50", "fields": map[string]any{"description": "the finding text"}},
		"PUT /rest/api/2/issue/PROJ-50":                    map[string]any{},
		"POST /rest/api/2/issue/PROJ-50/comment":           map[string]any{},
	})
	c := NewClient(tr, baseURL, selectOpts)

	n, err := c.RecordOccurrence("PROJ-50", "PROJ-700")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if n != 2 {
		t.Errorf("count = %d, want 2", n)
	}
	var updated, commented bool
	for _, cl := range *calls {
		if cl.method == "PUT" && cl.path == "/rest/api/2/issue/PROJ-50" {
			updated = true
			if !strings.Contains(bodyString(fieldsMap(cl.body), "description"), "occurrences: 2") {
				desc := fieldsMap(cl.body)["description"]
				t.Errorf("updated description missing occurrences: 2, got %v", desc)
			}
		}
		if cl.method == "POST" && cl.path == "/rest/api/2/issue/PROJ-50/comment" {
			commented = true
			if !strings.Contains(bodyString(cl.body, "body"), "PROJ-700") {
				t.Error("comment should reference the related ticket")
			}
		}
	}
	if !updated || !commented {
		t.Errorf("expected a description PUT and a comment, updated=%v commented=%v", updated, commented)
	}
}

// TestAddCommentPostsToIssue posts a comment straight to the issue by its key.
func TestAddCommentPostsToIssue(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"POST /rest/api/2/issue/PROJ-9/comment": map[string]any{},
	})
	c := NewClient(tr, baseURL, selectOpts)

	if err := c.AddComment("PROJ-9", "stranded work note"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/rest/api/2/issue/PROJ-9/comment" {
		t.Fatalf("expected one comment POST, got %+v", *calls)
	}
	if bodyString((*calls)[0].body, "body") != "stranded work note" {
		t.Errorf("comment body = %q", bodyString((*calls)[0].body, "body"))
	}
}
