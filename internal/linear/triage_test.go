package linear

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// Untriaged reads the team's unassigned backlog and Todo issues, drops the ones a
// human already marked ready or blocked, and orders the rest as selection would.
func TestUntriagedListsUnlabelledBacklogAndTodoIssues(t *testing.T) {
	label := func(names ...string) map[string]any {
		var nodes []any
		for _, n := range names {
			nodes = append(nodes, map[string]any{"name": n})
		}
		return map[string]any{"nodes": nodes}
	}
	tr, calls := transportReturning(t, map[string]any{
		"issues": map[string]any{"nodes": []any{
			map[string]any{"identifier": "BEH-1", "title": "low", "priority": 4, "labels": label()},
			map[string]any{"identifier": "BEH-2", "title": "ready", "priority": 1, "labels": label("ready-for-agent")},
			map[string]any{"identifier": "BEH-3", "title": "blocked", "priority": 1, "labels": label("Blocked")},
			map[string]any{"identifier": "BEH-4", "title": "urgent", "priority": 1, "labels": label("bug")},
		}},
	})

	got, err := NewClient(tr, testOptions()).Untriaged()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var keys []string
	for _, tk := range got {
		keys = append(keys, tk.Identifier)
	}
	if want := []string{"BEH-4", "BEH-1"}; !reflect.DeepEqual(keys, want) {
		t.Errorf("Untriaged = %v, want %v (ready and blocked dropped, urgent first)", keys, want)
	}
	if !got[0].HasLabel("bug") {
		t.Errorf("labels = %v, want them carried on the ticket", got[0].Labels)
	}

	filter := (*calls)[0].variables["filter"].(map[string]any)
	state, _ := json.Marshal(filter["state"])
	if string(state) != `{"type":{"in":["backlog","unstarted"]}}` {
		t.Errorf("state filter = %s, want backlog and unstarted", state)
	}
	if _, ok := filter["labels"]; ok {
		t.Error("Untriaged must not require the ready label")
	}
}

// markReadyTransport answers the label lookup with an issue in stateType, the
// state read with the team's states, and every mutation with success.
func markReadyTransport(stateType string) (Transport, *[]call) {
	var calls []call
	tr := func(query string, variables map[string]any) (json.RawMessage, error) {
		calls = append(calls, call{query: query, variables: variables})
		switch {
		case strings.Contains(query, "LabelByName"):
			return json.Marshal(map[string]any{
				"issue": map[string]any{
					"id":    "issue-uuid",
					"state": map[string]any{"type": stateType},
					"team":  map[string]any{"labels": map[string]any{"nodes": []any{map[string]any{"id": "ready-id"}}}},
				},
				"issueLabels": map[string]any{"nodes": []any{}},
			})
		case strings.Contains(query, "IssueStates"):
			return json.Marshal(map[string]any{"issue": map[string]any{"id": "issue-uuid", "team": map[string]any{"states": map[string]any{"nodes": []any{
				map[string]any{"id": "backlog-id", "name": "Backlog", "type": "backlog"},
				map[string]any{"id": "todo-id", "name": "Todo", "type": "unstarted"},
			}}}}})
		}
		return json.Marshal(map[string]any{"ok": map[string]any{"success": true}})
	}
	return tr, &calls
}

// A backlog ticket is moved to Todo and then labelled ready, in that order.
func TestMarkReadyMovesABacklogTicketToTodoThenLabelsIt(t *testing.T) {
	tr, calls := markReadyTransport("backlog")
	if err := NewClient(tr, testOptions()).MarkReady("BEH-9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var move, add = -1, -1
	for i, c := range *calls {
		if strings.Contains(c.query, "issueUpdate") {
			move = i
			if c.variables["stateId"] != "todo-id" {
				t.Errorf("moved to %v, want todo-id", c.variables["stateId"])
			}
		}
		if strings.Contains(c.query, "issueAddLabel") {
			add = i
			if c.variables["labelId"] != "ready-id" || c.variables["id"] != "issue-uuid" {
				t.Errorf("add = %+v, want issueAddLabel(issue-uuid, ready-id)", c.variables)
			}
		}
	}
	if move < 0 || add < 0 || move > add {
		t.Errorf("move at %d, label at %d: want the move to Todo before the label", move, add)
	}
	if (*calls)[0].variables["name"] != "ready-for-agent" {
		t.Errorf("label lookup = %+v, want the configured ready label", (*calls)[0].variables)
	}
}

// A ticket already in Todo, or one a human moved elsewhere, only gets the label.
func TestMarkReadyLeavesANonBacklogStateAlone(t *testing.T) {
	for _, state := range []string{"unstarted", "started"} {
		tr, calls := markReadyTransport(state)
		if err := NewClient(tr, testOptions()).MarkReady("BEH-9"); err != nil {
			t.Fatalf("%s: unexpected error: %v", state, err)
		}
		labelled := false
		for _, c := range *calls {
			if strings.Contains(c.query, "issueUpdate") {
				t.Errorf("%s: a ticket outside Backlog must keep its state", state)
			}
			labelled = labelled || strings.Contains(c.query, "issueAddLabel")
		}
		if !labelled {
			t.Errorf("%s: the ready label was not added", state)
		}
	}
}
