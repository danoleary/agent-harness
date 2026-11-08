package github

import (
	"reflect"
	"testing"
)

const untriagedPath = "GET /repos/acme/widgets/issues?assignee=none&direction=asc&per_page=100&sort=created&state=open"

// Untriaged keeps the open issues no label has already decided: not ready, not
// claimed, not blocked, and not a pull request.
func TestUntriagedDropsLabelledIssuesAndPRs(t *testing.T) {
	label := func(name string) []any { return []any{map[string]any{"name": name}} }
	tr, _ := fakeTransport(map[string]any{
		untriagedPath: []any{
			map[string]any{"number": 1, "title": "ready", "labels": label("ready-for-agent")},
			map[string]any{"number": 2, "title": "claimed", "labels": label("in-progress")},
			map[string]any{"number": 3, "title": "blocked", "labels": label("blocked")},
			map[string]any{"number": 4, "title": "a PR", "pull_request": map[string]any{"url": "u"}},
			map[string]any{"number": 5, "title": "new idea", "body": "do it", "labels": label("enhancement")},
			map[string]any{"number": 6, "title": "bare"},
		},
	})

	got, err := NewClient(tr, "acme", "widgets", readyLabels).Untriaged()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var keys []string
	for _, tk := range got {
		keys = append(keys, tk.Identifier)
	}
	if want := []string{"#5", "#6"}; !reflect.DeepEqual(keys, want) {
		t.Fatalf("Untriaged = %v, want %v", keys, want)
	}
	if got[0].Description != "do it" || !got[0].HasLabel("enhancement") || got[0].TeamID != "acme/widgets" {
		t.Errorf("ticket = %+v, want its body, labels and repo", got[0])
	}
}

// MarkReady adds the ready label and nothing else.
func TestMarkReadyAddsTheReadyLabel(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{"POST /repos/acme/widgets/issues/5/labels": []any{}})
	if err := NewClient(tr, "acme", "widgets", readyLabels).MarkReady("#5"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 || !reflect.DeepEqual(labelsInBody((*calls)[0].body), []string{"ready-for-agent"}) {
		t.Errorf("calls = %+v, want one add of ready-for-agent", *calls)
	}
}
