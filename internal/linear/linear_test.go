package linear

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/ticket"
)

// fakeTransport returns the given data payload (marshalled) for every call,
// recording the operations it saw.
type call struct {
	query     string
	variables map[string]any
}

func transportReturning(t *testing.T, data any) (Transport, *[]call) {
	t.Helper()
	var calls []call
	tr := func(query string, variables map[string]any) (json.RawMessage, error) {
		calls = append(calls, call{query: query, variables: variables})
		b, err := json.Marshal(data)
		if err != nil {
			t.Fatalf("marshal fake data: %v", err)
		}
		return b, nil
	}
	return tr, &calls
}

func TestFetchTicketParsesIssue(t *testing.T) {
	tr, _ := transportReturning(t, map[string]any{
		"issue": map[string]any{
			"identifier":    "BEH-362",
			"title":         "Agent harness Phase 1",
			"description":   "## What to build\n\nA CLI.",
			"url":           "https://linear.app/beherd/issue/BEH-362",
			"priorityLabel": "Urgent",
			"team":          map[string]any{"id": "team-uuid"},
		},
	})

	got, err := NewClient(tr).FetchTicket("BEH-362")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ticket.Ticket{
		Identifier:  "BEH-362",
		Title:       "Agent harness Phase 1",
		Description: "## What to build\n\nA CLI.",
		URL:         "https://linear.app/beherd/issue/BEH-362",
		Priority:    "Urgent",
		TeamID:      "team-uuid",
	}
	if got != want {
		t.Errorf("ticket = %+v, want %+v", got, want)
	}
}

func TestFetchTicketNotFound(t *testing.T) {
	tr, _ := transportReturning(t, map[string]any{"issue": nil})

	_, err := NewClient(tr).FetchTicket("BEH-999")
	if err == nil || !strings.Contains(err.Error(), "BEH-999") {
		t.Errorf("expected not-found error mentioning BEH-999, got %v", err)
	}
}

func TestMoveToInProgress(t *testing.T) {
	var calls []call
	tr := func(query string, variables map[string]any) (json.RawMessage, error) {
		calls = append(calls, call{query: query, variables: variables})
		if strings.Contains(query, "states") {
			return json.Marshal(map[string]any{
				"issue": map[string]any{
					"id": "issue-uuid",
					"team": map[string]any{
						"states": map[string]any{
							"nodes": []any{
								map[string]any{"id": "state-todo", "name": "Todo", "type": "unstarted"},
								map[string]any{"id": "state-progress", "name": "In Progress", "type": "started"},
								map[string]any{"id": "state-review", "name": "In Review", "type": "started"},
							},
						},
					},
				},
			})
		}
		return json.Marshal(map[string]any{"issueUpdate": map[string]any{"success": true}})
	}

	if err := NewClient(tr).MoveToInProgress("BEH-362"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	var update *call
	for i := range calls {
		if strings.Contains(calls[i].query, "issueUpdate") {
			update = &calls[i]
		}
	}
	if update == nil {
		t.Fatal("no issueUpdate mutation issued")
	}
	if update.variables["id"] != "issue-uuid" {
		t.Errorf("update id = %v, want issue-uuid", update.variables["id"])
	}
	if update.variables["stateId"] != "state-progress" {
		t.Errorf("update stateId = %v, want state-progress", update.variables["stateId"])
	}
}

func TestFileFinding(t *testing.T) {
	var captured map[string]any
	tr := func(_ string, variables map[string]any) (json.RawMessage, error) {
		captured = variables
		return json.Marshal(map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue":   map[string]any{"identifier": "BEH-400", "url": "https://x/BEH-400"},
			},
		})
	}

	created, err := NewClient(tr).FileFinding(
		findings.Finding{Title: "tokens:build missing", Body: "Storybook died", Kind: "setup"},
		FileFindingOptions{TeamID: "team-uuid", RelatedIdentifier: "BEH-362"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if created.Identifier != "BEH-400" {
		t.Errorf("created.Identifier = %q, want BEH-400", created.Identifier)
	}

	input, ok := captured["input"].(map[string]any)
	if !ok {
		t.Fatal("input variable missing")
	}
	if input["teamId"] != "team-uuid" {
		t.Errorf("teamId = %v", input["teamId"])
	}
	if input["title"] != "tokens:build missing" {
		t.Errorf("title = %v", input["title"])
	}
	desc, _ := input["description"].(string)
	if !strings.Contains(desc, "Storybook died") {
		t.Errorf("description missing body: %q", desc)
	}
	if !strings.Contains(desc, "BEH-362") {
		t.Errorf("description missing related id: %q", desc)
	}
}

// Every finding the harness files is, by construction, about the harness itself,
// so the create payload must carry the agent-harness label (BEH-409).
func TestFileFindingLabelsWithAgentHarness(t *testing.T) {
	var captured map[string]any
	tr := func(_ string, variables map[string]any) (json.RawMessage, error) {
		captured = variables
		return json.Marshal(map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue":   map[string]any{"identifier": "BEH-400", "url": "https://x/BEH-400"},
			},
		})
	}

	_, err := NewClient(tr).FileFinding(
		findings.Finding{Title: "x", Body: "y"},
		FileFindingOptions{TeamID: "team-uuid", RelatedIdentifier: "BEH-362"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, _ := captured["input"].(map[string]any)
	if !labelIDsContain(input["labelIds"], agentHarnessLabelID) {
		t.Errorf("labelIds = %v, want it to contain agent-harness label %q", input["labelIds"], agentHarnessLabelID)
	}
}

// A finding that carries its own label keeps it: agent-harness is added
// alongside, not in place of it (BEH-409).
func TestFileFindingAddsAgentHarnessAlongsideFindingLabels(t *testing.T) {
	var captured map[string]any
	tr := func(_ string, variables map[string]any) (json.RawMessage, error) {
		captured = variables
		return json.Marshal(map[string]any{
			"issueCreate": map[string]any{
				"success": true,
				"issue":   map[string]any{"identifier": "BEH-400", "url": "https://x/BEH-400"},
			},
		})
	}

	_, err := NewClient(tr).FileFinding(
		findings.Finding{Title: "x", Body: "y", LabelIDs: []string{"own-label-uuid"}},
		FileFindingOptions{TeamID: "team-uuid", RelatedIdentifier: "BEH-362"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, _ := captured["input"].(map[string]any)
	if !labelIDsContain(input["labelIds"], "own-label-uuid") {
		t.Errorf("labelIds = %v, want it to keep the finding's own label", input["labelIds"])
	}
	if !labelIDsContain(input["labelIds"], agentHarnessLabelID) {
		t.Errorf("labelIds = %v, want it to also contain agent-harness label", input["labelIds"])
	}
}

// labelIDsContain reports whether the issueCreate labelIds payload (a []string)
// contains id.
func labelIDsContain(labelIDs any, id string) bool {
	ids, ok := labelIDs.([]string)
	if !ok {
		return false
	}
	for _, got := range ids {
		if got == id {
			return true
		}
	}
	return false
}
