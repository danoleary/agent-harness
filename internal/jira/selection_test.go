package jira

import "testing"

// selectOpts is the selection surface the selection + claim tests share.
var selectOpts = Options{
	ProjectKey:           "PROJ",
	ReadyJQL:             "status = Ready",
	InProgressJQL:        "status = \"In Progress\"",
	Findings:             "agent-harness",
	InProgressTransition: "Start Progress",
	TodoTransition:       "Back to To Do",
	CanceledTransition:   "Cancel",
}

// hasCall reports whether calls contains one matching method + path.
func hasCall(calls []call, method, path string) bool {
	for _, c := range calls {
		if c.method == method && c.path == path {
			return true
		}
	}
	return false
}

// selectPath is the exact search/jql request SelectNextTicket issues (keyed with
// the transport's "<METHOD> " prefix): the ready JQL, summary+description fields.
const selectPath = "GET /rest/api/2/search/jql?fields=summary%2Cdescription&jql=status+%3D+Ready&maxResults=50"

// TestSelectNextTicketPicksFirst pins the queue read: the first issue the ready
// JQL returns wins (the JQL itself encodes ready/blocked/ordering, so there is no
// separate eligibility screen). It is a PURE READ — claim-on-select (ADR-0003) is
// the caller composing it with MoveToInProgress — so dry-run resolves a ticket
// without mutating Jira.
func TestSelectNextTicketPicksFirst(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		selectPath: map[string]any{
			"issues": []any{
				map[string]any{"key": "PROJ-9", "fields": map[string]any{"summary": "Real work", "description": "do it"}},
				map[string]any{"key": "PROJ-10", "fields": map[string]any{"summary": "Next"}},
			},
		},
	})
	c := NewClient(tr, baseURL, selectOpts)

	got, ok, err := c.SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected an eligible ticket, got ok=false")
	}
	if got.Identifier != "PROJ-9" || got.Title != "Real work" || got.Description != "do it" {
		t.Errorf("selected = %+v, want PROJ-9/Real work/do it", got)
	}
	if got.URL != "https://acme.atlassian.net/browse/PROJ-9" || got.TeamID != "PROJ" {
		t.Errorf("selected URL/TeamID = %q/%q", got.URL, got.TeamID)
	}
	if len(*calls) != 1 || (*calls)[0].method != "GET" {
		t.Errorf("expected one GET (pure read), got %+v", *calls)
	}
}

// TestSelectNextTicketEmptyQueue returns ok=false (not an error) when the ready
// JQL matches nothing, so the caller idles rather than crashing.
func TestSelectNextTicketEmptyQueue(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{selectPath: map[string]any{"issues": []any{}}})
	c := NewClient(tr, baseURL, selectOpts)

	_, ok, err := c.SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for an empty queue")
	}
}

// transitionsPath is the GET that lists an issue's available transitions.
func transitionsPath(key string) string {
	return "GET /rest/api/2/issue/" + key + "/transitions"
}

// transitionsList is a canned transitions response covering the three named
// transitions the claim/release/cancel tests resolve.
var transitionsList = map[string]any{
	"transitions": []any{
		map[string]any{"id": "11", "name": "Back to To Do"},
		map[string]any{"id": "21", "name": "Start Progress"},
		map[string]any{"id": "31", "name": "Cancel"},
	},
}

// TestMoveToInProgressResolvesTransition pins the claim: the adapter resolves the
// configured transition NAME against the issue's available transitions, then POSTs
// the matching id. Named (not id) because transition ids are instance-specific and
// vary by source status.
func TestMoveToInProgressResolvesTransition(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		transitionsPath("PROJ-9"):                   transitionsList,
		"POST /rest/api/2/issue/PROJ-9/transitions": map[string]any{},
	})
	c := NewClient(tr, baseURL, selectOpts)

	if err := c.MoveToInProgress("PROJ-9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCall(*calls, "GET", "/rest/api/2/issue/PROJ-9/transitions") {
		t.Error("expected the available transitions to be listed")
	}
	var posted bool
	for _, cl := range *calls {
		if cl.method == "POST" && cl.path == "/rest/api/2/issue/PROJ-9/transitions" {
			posted = true
			m, _ := cl.body.(map[string]any)
			tr, _ := m["transition"].(map[string]any)
			if tr["id"] != "21" {
				t.Errorf("posted transition id = %v, want 21 (Start Progress)", tr["id"])
			}
		}
	}
	if !posted {
		t.Error("expected a transition POST")
	}
}

// TestReleaseToTodoTransitions undoes a claim via the configured Todo transition.
func TestReleaseToTodoTransitions(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		transitionsPath("PROJ-9"):                   transitionsList,
		"POST /rest/api/2/issue/PROJ-9/transitions": map[string]any{},
	})
	c := NewClient(tr, baseURL, selectOpts)

	if err := c.ReleaseToTodo("PROJ-9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, cl := range *calls {
		if cl.method == "POST" && cl.path == "/rest/api/2/issue/PROJ-9/transitions" {
			m, _ := cl.body.(map[string]any)
			tr, _ := m["transition"].(map[string]any)
			if tr["id"] != "11" {
				t.Errorf("posted transition id = %v, want 11 (Back to To Do)", tr["id"])
			}
		}
	}
}

// TestMoveToCanceledTransitions consumes a recommend-close verdict via the
// terminal Cancel transition (BEH-682).
func TestMoveToCanceledTransitions(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		transitionsPath("PROJ-9"):                   transitionsList,
		"POST /rest/api/2/issue/PROJ-9/transitions": map[string]any{},
	})
	c := NewClient(tr, baseURL, selectOpts)

	if err := c.MoveToCanceled("PROJ-9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	for _, cl := range *calls {
		if cl.method == "POST" && cl.path == "/rest/api/2/issue/PROJ-9/transitions" {
			m, _ := cl.body.(map[string]any)
			tr, _ := m["transition"].(map[string]any)
			if tr["id"] != "31" {
				t.Errorf("posted transition id = %v, want 31 (Cancel)", tr["id"])
			}
		}
	}
}

// TestTransitionUnknownNameErrors fails loud when the configured transition name
// is not among the issue's available transitions — a misconfiguration (or a
// workflow that doesn't allow the move from the current status) must not silently
// no-op the claim.
func TestTransitionUnknownNameErrors(t *testing.T) {
	opts := selectOpts
	opts.InProgressTransition = "Nonexistent"
	tr, _ := fakeTransport(map[string]any{
		transitionsPath("PROJ-9"): transitionsList,
	})
	c := NewClient(tr, baseURL, opts)

	if err := c.MoveToInProgress("PROJ-9"); err == nil {
		t.Fatal("expected an error for an unknown transition name")
	}
}
