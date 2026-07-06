package github

import (
	"encoding/json"
	"fmt"
	"testing"
)

// call records one transport invocation so tests can assert the adapter issued
// the right REST request (method + path + body).
type call struct {
	method string
	path   string
	body   any
}

// fakeTransport returns a Transport that replays canned JSON keyed by
// "<METHOD> <path>", recording every call. An unmapped call is a test failure
// (surfaced as a transport error) so a test that forgets to stub a request fails
// loudly rather than silently parsing null.
func fakeTransport(responses map[string]any) (Transport, *[]call) {
	calls := &[]call{}
	tr := func(method, path string, body any) (json.RawMessage, error) {
		*calls = append(*calls, call{method: method, path: path, body: body})
		resp, ok := responses[method+" "+path]
		if !ok {
			return nil, fmt.Errorf("unexpected transport call: %s %s", method, path)
		}
		b, err := json.Marshal(resp)
		if err != nil {
			return nil, err
		}
		return b, nil
	}
	return tr, calls
}

// TestFetchTicketMapsIssue is the tracer bullet: the GitHub adapter resolves a
// #number Key to the harness ticket shape via one issues GET. It pins that the
// Key is opaque (accepts "#42") and normalises to the bare number in the path.
func TestFetchTicketMapsIssue(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/issues/42": map[string]any{
			"number":   42,
			"title":    "Fix the thing",
			"body":     "Do the work",
			"html_url": "https://github.com/acme/widgets/issues/42",
		},
	})
	c := NewClient(tr, "acme", "widgets", Options{})

	got, err := c.FetchTicket("#42")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "#42" {
		t.Errorf("Identifier = %q, want #42", got.Identifier)
	}
	if got.Title != "Fix the thing" {
		t.Errorf("Title = %q, want %q", got.Title, "Fix the thing")
	}
	if got.Description != "Do the work" {
		t.Errorf("Description = %q, want %q", got.Description, "Do the work")
	}
	if got.URL != "https://github.com/acme/widgets/issues/42" {
		t.Errorf("URL = %q", got.URL)
	}
	if got.TeamID != "acme/widgets" {
		t.Errorf("TeamID = %q, want acme/widgets", got.TeamID)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one transport call, got %d: %+v", len(*calls), *calls)
	}
	if (*calls)[0].method != "GET" || (*calls)[0].path != "/repos/acme/widgets/issues/42" {
		t.Errorf("call = %s %s, want GET /repos/acme/widgets/issues/42", (*calls)[0].method, (*calls)[0].path)
	}
}

// TestFetchTicketPullsTaskListChildren pins the LCD child normalisation: an
// umbrella issue's task-list checkbox lines (`- [ ] #45`) are followed to their
// child issues and inlined as SubIssues, mirroring how the Linear adapter inlines
// sub-issues (BEH-619) so a sandboxed session never reaches GitHub. A bare `#99`
// in prose is NOT a task-list item and must be ignored.
func TestFetchTicketPullsTaskListChildren(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/issues/10": map[string]any{
			"number": 10,
			"title":  "Umbrella",
			"body":   "Do these:\n- [ ] #45\n- [x] #46 done\n\nSee also #99 inline in prose.",
		},
		"GET /repos/acme/widgets/issues/45": map[string]any{"number": 45, "title": "Child A", "body": "body a"},
		"GET /repos/acme/widgets/issues/46": map[string]any{"number": 46, "title": "Child B", "body": "body b"},
	})
	c := NewClient(tr, "acme", "widgets", Options{})

	got, err := c.FetchTicket("#10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.SubIssues) != 2 {
		t.Fatalf("expected 2 task-list children (#45, #46), got %d: %+v", len(got.SubIssues), got.SubIssues)
	}
	if got.SubIssues[0].Identifier != "#45" || got.SubIssues[0].Title != "Child A" || got.SubIssues[0].Description != "body a" {
		t.Errorf("child[0] = %+v, want #45/Child A/body a", got.SubIssues[0])
	}
	if got.SubIssues[1].Identifier != "#46" || got.SubIssues[1].Title != "Child B" {
		t.Errorf("child[1] = %+v, want #46/Child B", got.SubIssues[1])
	}
}
