package jira

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

const baseURL = "https://acme.atlassian.net"

// TestFetchTicketMapsIssue is the tracer bullet: the Jira adapter resolves a
// PROJ-123 Key to the harness ticket shape via one issue GET. It pins that the
// Key is the opaque Jira issue key, the browse URL is derived from the base URL,
// and the findings project is surfaced as the TeamID.
func TestFetchTicketMapsIssue(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /rest/api/2/issue/PROJ-123?fields=summary,description,subtasks": map[string]any{
			"key": "PROJ-123",
			"fields": map[string]any{
				"summary":     "Fix the thing",
				"description": "Do the work",
			},
		},
	})
	c := NewClient(tr, baseURL, Options{ProjectKey: "PROJ"})

	got, err := c.FetchTicket("PROJ-123")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "PROJ-123" {
		t.Errorf("Identifier = %q, want PROJ-123", got.Identifier)
	}
	if got.Title != "Fix the thing" {
		t.Errorf("Title = %q, want %q", got.Title, "Fix the thing")
	}
	if got.Description != "Do the work" {
		t.Errorf("Description = %q, want %q", got.Description, "Do the work")
	}
	if got.URL != "https://acme.atlassian.net/browse/PROJ-123" {
		t.Errorf("URL = %q, want the derived browse URL", got.URL)
	}
	if got.TeamID != "PROJ" {
		t.Errorf("TeamID = %q, want PROJ", got.TeamID)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected exactly one transport call, got %d: %+v", len(*calls), *calls)
	}
}

// TestFetchTicketPullsSubtaskChildren pins the LCD child normalisation: an
// umbrella issue's sub-tasks are followed to their child issues and inlined as
// SubIssues (each sub-task entry carries only key+summary, so its description is
// fetched), mirroring how the Linear/GitHub adapters inline children (BEH-619) so
// a sandboxed session never reaches Jira.
func TestFetchTicketPullsSubtaskChildren(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		"GET /rest/api/2/issue/PROJ-10?fields=summary,description,subtasks": map[string]any{
			"key": "PROJ-10",
			"fields": map[string]any{
				"summary":     "Umbrella",
				"description": "Do the children",
				"subtasks": []any{
					map[string]any{"key": "PROJ-45", "fields": map[string]any{"summary": "Child A"}},
					map[string]any{"key": "PROJ-46", "fields": map[string]any{"summary": "Child B"}},
				},
			},
		},
		"GET /rest/api/2/issue/PROJ-45?fields=summary,description": map[string]any{
			"key": "PROJ-45", "fields": map[string]any{"summary": "Child A", "description": "body a"},
		},
		"GET /rest/api/2/issue/PROJ-46?fields=summary,description": map[string]any{
			"key": "PROJ-46", "fields": map[string]any{"summary": "Child B", "description": "body b"},
		},
	})
	c := NewClient(tr, baseURL, Options{ProjectKey: "PROJ"})

	got, err := c.FetchTicket("PROJ-10")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.SubIssues) != 2 {
		t.Fatalf("expected 2 sub-task children, got %d: %+v", len(got.SubIssues), got.SubIssues)
	}
	if got.SubIssues[0].Identifier != "PROJ-45" || got.SubIssues[0].Title != "Child A" || got.SubIssues[0].Description != "body a" {
		t.Errorf("child[0] = %+v, want PROJ-45/Child A/body a", got.SubIssues[0])
	}
	if got.SubIssues[1].Identifier != "PROJ-46" || got.SubIssues[1].Description != "body b" {
		t.Errorf("child[1] = %+v, want PROJ-46/body b", got.SubIssues[1])
	}
}
