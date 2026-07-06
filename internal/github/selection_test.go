package github

import "testing"

// readyLabels is the selection surface the selection + claim tests share.
var readyLabels = Options{Ready: "ready-for-agent", Blocked: "blocked", InProgress: "in-progress", Findings: "agent-harness", Assignee: "harness-bot"}

// hasCall reports whether calls contains one matching method + path.
func hasCall(calls []call, method, path string) bool {
	for _, c := range calls {
		if c.method == method && c.path == path {
			return true
		}
	}
	return false
}

// labelsInBody extracts the "labels" string slice from a recorded POST body, or
// nil when the body isn't the expected shape.
func labelsInBody(body any) []string {
	m, ok := body.(map[string]any)
	if !ok {
		return nil
	}
	ls, ok := m["labels"].([]string)
	if !ok {
		return nil
	}
	return ls
}

// selectPath is the exact list-issues request SelectNextTicket issues (keyed with
// the transport's "<METHOD> " prefix): the small human-gated working set (ready
// label, open, unassigned) ordered oldest-first.
const selectPath = "GET /repos/acme/widgets/issues?assignee=none&direction=asc&labels=ready-for-agent&per_page=100&sort=created&state=open"

// TestSelectNextTicketPicksFirstEligible pins the queue read: the first returned
// issue that is not Blocked-labelled and not a pull request wins (GitHub returns
// PRs from the issues API, so they must be screened out). It is a pure read — no
// claim mutation — so dry-run can resolve a ticket without touching GitHub.
func TestSelectNextTicketPicksFirstEligible(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		selectPath: []any{
			map[string]any{"number": 7, "title": "Blocked one", "labels": []any{
				map[string]any{"name": "ready-for-agent"}, map[string]any{"name": "blocked"},
			}},
			map[string]any{"number": 8, "title": "A PR", "pull_request": map[string]any{"url": "u"}, "labels": []any{
				map[string]any{"name": "ready-for-agent"},
			}},
			map[string]any{"number": 9, "title": "Real work", "body": "do it", "html_url": "https://x/9", "labels": []any{
				map[string]any{"name": "ready-for-agent"},
			}},
		},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	got, ok, err := c.SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected an eligible ticket, got ok=false")
	}
	if got.Identifier != "#9" || got.Title != "Real work" {
		t.Errorf("selected = %s/%q, want #9/Real work", got.Identifier, got.Title)
	}
	if got.TeamID != "acme/widgets" {
		t.Errorf("TeamID = %q, want acme/widgets", got.TeamID)
	}
	if len(*calls) != 1 || (*calls)[0].method != "GET" {
		t.Errorf("expected one GET (pure read), got %+v", *calls)
	}
}

// TestSelectNextTicketEmptyQueue returns ok=false (not an error) when nothing is
// eligible, so the caller idles rather than crashing.
func TestSelectNextTicketEmptyQueue(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{selectPath: []any{}})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	_, ok, err := c.SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("expected ok=false for an empty queue")
	}
}

// TestMoveToInProgressClaims pins the claim: the label column transitions
// ready→in-progress (add in-progress, remove ready) and the configured bot is
// assigned. Removing the ready label is what drops the ticket from the queue (the
// list query requires it); the in-progress label is what the reaper finds.
func TestMoveToInProgressClaims(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"POST /repos/acme/widgets/issues/9/labels":                   []any{},
		"DELETE /repos/acme/widgets/issues/9/labels/ready-for-agent": []any{},
		"POST /repos/acme/widgets/issues/9/assignees":                map[string]any{},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	if err := c.MoveToInProgress("#9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCall(*calls, "POST", "/repos/acme/widgets/issues/9/labels") {
		t.Error("expected the in-progress label to be added")
	}
	if !hasCall(*calls, "DELETE", "/repos/acme/widgets/issues/9/labels/ready-for-agent") {
		t.Error("expected the ready label to be removed")
	}
	if !hasCall(*calls, "POST", "/repos/acme/widgets/issues/9/assignees") {
		t.Error("expected the bot to be assigned")
	}
	// The added label is the configured in-progress label, not something else.
	for _, cl := range *calls {
		if cl.method == "POST" && cl.path == "/repos/acme/widgets/issues/9/labels" {
			if ls := labelsInBody(cl.body); len(ls) != 1 || ls[0] != "in-progress" {
				t.Errorf("added labels = %v, want [in-progress]", ls)
			}
		}
	}
}

// TestMoveToInProgressWithoutAssignee omits the assignment call entirely when no
// bot is configured — the label transition alone still claims the ticket.
func TestMoveToInProgressWithoutAssignee(t *testing.T) {
	opts := readyLabels
	opts.Assignee = ""
	tr, calls := fakeTransport(map[string]any{
		"POST /repos/acme/widgets/issues/9/labels":                   []any{},
		"DELETE /repos/acme/widgets/issues/9/labels/ready-for-agent": []any{},
	})
	c := NewClient(tr, "acme", "widgets", opts)

	if err := c.MoveToInProgress("#9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if hasCall(*calls, "POST", "/repos/acme/widgets/issues/9/assignees") {
		t.Error("no assignee configured, so no assignment call should be made")
	}
}

// TestReleaseToTodoReverses undoes a claim: in-progress→ready and the bot is
// unassigned, returning the ticket to the queue.
func TestReleaseToTodoReverses(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"POST /repos/acme/widgets/issues/9/labels":               []any{},
		"DELETE /repos/acme/widgets/issues/9/labels/in-progress": []any{},
		"DELETE /repos/acme/widgets/issues/9/assignees":          map[string]any{},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	if err := c.ReleaseToTodo("#9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !hasCall(*calls, "POST", "/repos/acme/widgets/issues/9/labels") {
		t.Error("expected the ready label to be restored")
	}
	if !hasCall(*calls, "DELETE", "/repos/acme/widgets/issues/9/labels/in-progress") {
		t.Error("expected the in-progress label to be removed")
	}
	if !hasCall(*calls, "DELETE", "/repos/acme/widgets/issues/9/assignees") {
		t.Error("expected the bot to be unassigned")
	}
}

// TestMoveToCanceledClosesNotPlanned closes the issue with state_reason
// not_planned — GitHub's terminal "won't do" state — so a superseded/duplicate
// ticket leaves both the select and reap pools (both scope to open issues).
func TestMoveToCanceledClosesNotPlanned(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"PATCH /repos/acme/widgets/issues/9": map[string]any{"number": 9, "state": "closed"},
	})
	c := NewClient(tr, "acme", "widgets", readyLabels)

	if err := c.MoveToCanceled("#9"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	var patched bool
	for _, cl := range *calls {
		if cl.method == "PATCH" && cl.path == "/repos/acme/widgets/issues/9" {
			patched = true
			m, _ := cl.body.(map[string]any)
			if m["state"] != "closed" || m["state_reason"] != "not_planned" {
				t.Errorf("PATCH body = %+v, want state=closed state_reason=not_planned", m)
			}
		}
	}
	if !patched {
		t.Error("expected a PATCH closing the issue")
	}
}
