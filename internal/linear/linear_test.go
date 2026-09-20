package linear

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// testOptions is the selection surface the adapter's tests run against — the
// values that were hardcoded constants before BEH-641 made them Consumer config,
// so the existing expectations keep asserting the same queries and labels.
func testOptions() Options {
	return Options{
		TeamKey:         "BEH",
		Ready:           "ready-for-agent",
		Blocked:         "Blocked",
		FindingsLabelID: "788a5654-a4b3-4ac2-8483-a4d50408ebc0",
		FindingsLabel:   "agent-harness",
	}
}

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
			"url":           "https://linear.app/example-workspace/issue/BEH-362",
			"priorityLabel": "Urgent",
			"team":          map[string]any{"id": "team-uuid"},
		},
	})

	got, err := NewClient(tr, testOptions()).FetchTicket("BEH-362")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := ticket.Ticket{
		Identifier:  "BEH-362",
		Title:       "Agent harness Phase 1",
		Description: "## What to build\n\nA CLI.",
		URL:         "https://linear.app/example-workspace/issue/BEH-362",
		Priority:    "Urgent",
		TeamID:      "team-uuid",
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("ticket = %+v, want %+v", got, want)
	}
}

// BEH-619: an umbrella/batch ticket's real work lives in its child sub-issues.
// The sandbox is isolated from Linear (ADR-0002), so FetchTicket must pull each
// child's title + body host-side (the query requests `children`) and carry them
// on the ticket — the prompt inlines them so the session never reaches Linear.
func TestFetchTicketParsesSubIssues(t *testing.T) {
	tr, calls := transportReturning(t, map[string]any{
		"issue": map[string]any{
			"identifier":  "BEH-520",
			"title":       "Lint/boundary guard sweep",
			"description": "Batch the small static-rule tickets.",
			"children": map[string]any{
				"nodes": []any{
					map[string]any{"identifier": "BEH-293", "title": "no-forced-open-modal rule", "description": "Forbid open={true}."},
					map[string]any{"identifier": "BEH-381", "title": "story-module boundary", "description": "Import via the seam."},
				},
			},
		},
	})

	got, err := NewClient(tr, testOptions()).FetchTicket("BEH-520")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	want := []ticket.SubIssue{
		{Identifier: "BEH-293", Title: "no-forced-open-modal rule", Description: "Forbid open={true}."},
		{Identifier: "BEH-381", Title: "story-module boundary", Description: "Import via the seam."},
	}
	if !reflect.DeepEqual(got.SubIssues, want) {
		t.Errorf("SubIssues = %+v, want %+v", got.SubIssues, want)
	}
	// The fetch must actually request the children connection — otherwise the
	// host has nothing to inline and the sandbox dead-ends on Linear again.
	if !strings.Contains((*calls)[0].query, "children") {
		t.Errorf("fetch query does not request children: %s", (*calls)[0].query)
	}
}

// A ticket with no children carries an empty SubIssues slice (not a panic) — the
// common, non-umbrella case.
func TestFetchTicketNoSubIssues(t *testing.T) {
	tr, _ := transportReturning(t, map[string]any{
		"issue": map[string]any{
			"identifier": "BEH-362",
			"title":      "Ordinary ticket",
			"children":   map[string]any{"nodes": []any{}},
		},
	})

	got, err := NewClient(tr, testOptions()).FetchTicket("BEH-362")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got.SubIssues) != 0 {
		t.Errorf("SubIssues = %+v, want empty", got.SubIssues)
	}
}

func TestFetchTicketNotFound(t *testing.T) {
	tr, _ := transportReturning(t, map[string]any{"issue": nil})

	_, err := NewClient(tr, testOptions()).FetchTicket("BEH-999")
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

	if err := NewClient(tr, testOptions()).MoveToInProgress("BEH-362"); err != nil {
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

// pickState prefers the canonically-named state of the right type, falling back
// to the first state of that type, and returns "" when none matches.
func TestPickState(t *testing.T) {
	states := []issueState{
		{ID: "s-todo", Name: "Todo", Type: "unstarted"},
		{ID: "s-backlog", Name: "Backlog", Type: "backlog"},
		{ID: "s-prog", Name: "In Progress", Type: "started"},
		{ID: "s-review", Name: "In Review", Type: "started"},
	}
	if got := pickState(states, "started", "In Progress"); got != "s-prog" {
		t.Errorf("pickState started/In Progress = %q, want s-prog", got)
	}
	if got := pickState(states, "unstarted", "Todo"); got != "s-todo" {
		t.Errorf("pickState unstarted/Todo = %q, want s-todo", got)
	}
	// Name miss falls back to the first state of the requested type.
	if got := pickState(states, "started", "Doing"); got != "s-prog" {
		t.Errorf("pickState fallback by type = %q, want s-prog (first started)", got)
	}
	// No state of that type → empty.
	if got := pickState(states, "completed", "Done"); got != "" {
		t.Errorf("pickState no-match = %q, want empty", got)
	}
}

// ReleaseToTodo moves the ticket back to its team's unstarted "Todo" state,
// undoing an In-Progress claim that yielded nothing (BEH-543).
func TestReleaseToTodo(t *testing.T) {
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
							},
						},
					},
				},
			})
		}
		return json.Marshal(map[string]any{"issueUpdate": map[string]any{"success": true}})
	}

	if err := NewClient(tr, testOptions()).ReleaseToTodo("BEH-324"); err != nil {
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
	if update.variables["stateId"] != "state-todo" {
		t.Errorf("update stateId = %v, want state-todo", update.variables["stateId"])
	}
}

// MoveToCanceled moves the ticket into its team's terminal "canceled" state — the
// host-side action that consumes a recommend-close verdict and actually closes a
// superseded/duplicate ticket (BEH-682), so it exits the --next selection pool and
// the reaper pool permanently instead of re-looping to the same "nothing to ship".
func TestMoveToCanceled(t *testing.T) {
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
								map[string]any{"id": "state-done", "name": "Done", "type": "completed"},
								map[string]any{"id": "state-canceled", "name": "Canceled", "type": "canceled"},
							},
						},
					},
				},
			})
		}
		return json.Marshal(map[string]any{"issueUpdate": map[string]any{"success": true}})
	}

	if err := NewClient(tr, testOptions()).MoveToCanceled("BEH-682"); err != nil {
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
	if update.variables["stateId"] != "state-canceled" {
		t.Errorf("update stateId = %v, want state-canceled", update.variables["stateId"])
	}
}

func TestSearchFindingsParsesIssuesAndKeys(t *testing.T) {
	tr, calls := transportReturning(t, map[string]any{
		"issues": map[string]any{
			"nodes": []any{
				map[string]any{
					"identifier":  "BEH-405",
					"title":       "Storybook unrunnable",
					"description": "missing deps\n\n<!-- finding-key: sandbox-playwright-missing-deps -->",
					"state":       map[string]any{"type": "unstarted"},
				},
				map[string]any{
					"identifier":  "BEH-406",
					"title":       "Old wontfix",
					"description": "no marker here",
					"state":       map[string]any{"type": "canceled"},
				},
			},
		},
	})

	got, err := NewClient(tr, testOptions()).SearchFindings("team-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("findings len = %d, want 2", len(got))
	}
	if got[0] != (tracker.ExistingFinding{Identifier: "BEH-405", Title: "Storybook unrunnable", Key: "sandbox-playwright-missing-deps", Closed: false}) {
		t.Errorf("findings[0] = %+v", got[0])
	}
	if !got[1].Closed {
		t.Errorf("findings[1] should be Closed (canceled), got %+v", got[1])
	}
	if got[1].Key != "" {
		t.Errorf("findings[1].Key = %q, want empty (no marker)", got[1].Key)
	}

	// The query must scope by team + the agent-harness label so it only sees
	// harness findings, not every issue on the team.
	if len(*calls) != 1 {
		t.Fatalf("expected one query, got %d", len(*calls))
	}
	rawVars, err := json.Marshal((*calls)[0].variables)
	if err != nil {
		t.Fatalf("marshal variables: %v", err)
	}
	vars := string(rawVars)
	if !strings.Contains(vars, "team-uuid") || !strings.Contains(vars, "agent-harness") {
		t.Errorf("query variables missing team/label scope: %s", vars)
	}
	// The query must exclude terminal states so closed findings don't consume the
	// result window — dedup coverage must not silently shrink as the backlog grows.
	if !strings.Contains(vars, "completed") || !strings.Contains(vars, "canceled") {
		t.Errorf("query variables missing terminal-state exclusion: %s", vars)
	}
}

// fileFindingTransport answers both the label-resolution query and the
// issueCreate mutation, capturing the mutation input.
func fileFindingTransport(t *testing.T, captured *map[string]any) Transport {
	t.Helper()
	return func(query string, variables map[string]any) (json.RawMessage, error) {
		if strings.Contains(query, "issueCreate") {
			*captured = variables
			return json.Marshal(map[string]any{
				"issueCreate": map[string]any{
					"success": true,
					"issue":   map[string]any{"identifier": "BEH-400", "url": "https://x/BEH-400"},
				},
			})
		}
		// label-resolution query
		return json.Marshal(map[string]any{
			"team": map[string]any{
				"labels": map[string]any{
					"nodes": []any{
						map[string]any{"id": "label-other", "name": "bug"},
						map[string]any{"id": "label-harness", "name": "agent-harness"},
					},
				},
			},
		})
	}
}

func TestFileFinding(t *testing.T) {
	var captured map[string]any
	created, err := NewClient(fileFindingTransport(t, &captured), testOptions()).FileFinding(
		findings.Finding{Title: "tokens:build missing", Body: "Storybook died", Kind: "setup"},
		tracker.FileFindingOptions{TeamID: "team-uuid", RelatedKey: "BEH-362"},
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

	// Filed findings must carry the agent-harness label so SearchFindings can find them.
	labelIDs, _ := input["labelIds"].([]string)
	if len(labelIDs) != 1 || labelIDs[0] != testOptions().FindingsLabelID {
		t.Errorf("labelIds = %v, want [%s]", input["labelIds"], testOptions().FindingsLabelID)
	}
}

// A finding with an explicit key gets the machine-readable marker embedded so a
// later run can dedup on an exact-key lookup.
func TestFileFindingEmbedsKeyMarker(t *testing.T) {
	var captured map[string]any
	_, err := NewClient(fileFindingTransport(t, &captured), testOptions()).FileFinding(
		findings.Finding{Title: "Playwright missing", Body: "no deps", Key: "sandbox-playwright-missing-deps"},
		tracker.FileFindingOptions{TeamID: "team-uuid", RelatedKey: "BEH-394"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	desc, _ := captured["input"].(map[string]any)["description"].(string)
	if got := ExtractFindingKey(desc); got != "sandbox-playwright-missing-deps" {
		t.Errorf("round-trip key = %q, want sandbox-playwright-missing-deps (desc: %q)", got, desc)
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

	_, err := NewClient(tr, testOptions()).FileFinding(
		findings.Finding{Title: "x", Body: "y"},
		tracker.FileFindingOptions{TeamID: "team-uuid", RelatedKey: "BEH-362"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, _ := captured["input"].(map[string]any)
	if !labelIDsContain(input["labelIds"], testOptions().FindingsLabelID) {
		t.Errorf("labelIds = %v, want it to contain agent-harness label %q", input["labelIds"], testOptions().FindingsLabelID)
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

	_, err := NewClient(tr, testOptions()).FileFinding(
		findings.Finding{Title: "x", Body: "y", LabelIDs: []string{"own-label-uuid"}},
		tracker.FileFindingOptions{TeamID: "team-uuid", RelatedKey: "BEH-362"},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, _ := captured["input"].(map[string]any)
	if !labelIDsContain(input["labelIds"], "own-label-uuid") {
		t.Errorf("labelIds = %v, want it to keep the finding's own label", input["labelIds"])
	}
	if !labelIDsContain(input["labelIds"], testOptions().FindingsLabelID) {
		t.Errorf("labelIds = %v, want it to also contain agent-harness label", input["labelIds"])
	}
}

// routingTransport returns a Transport that picks a response by matching a
// substring of the query, recording every call so a test can assert the
// variables sent. A query matching no route fails the test.
func routingTransport(t *testing.T, routes map[string]any) (Transport, *[]call) {
	t.Helper()
	var calls []call
	tr := func(query string, variables map[string]any) (json.RawMessage, error) {
		calls = append(calls, call{query: query, variables: variables})
		for needle, data := range routes {
			if strings.Contains(query, needle) {
				b, err := json.Marshal(data)
				if err != nil {
					t.Fatalf("marshal route %q: %v", needle, err)
				}
				return b, nil
			}
		}
		t.Fatalf("no route matched query: %s", query)
		return nil, nil
	}
	return tr, &calls
}

func TestRecordOccurrenceBumpsCountAndComments(t *testing.T) {
	tr, calls := routingTransport(t, map[string]any{
		"OccurrenceContext": map[string]any{
			"issue": map[string]any{"id": "uuid-1", "description": "the original body"},
		},
		"UpdateDescription": map[string]any{"issueUpdate": map[string]any{"success": true}},
		"AddComment":        map[string]any{"commentCreate": map[string]any{"success": true}},
	})

	count, err := NewClient(tr, testOptions()).RecordOccurrence("BEH-405", "BEH-370")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	// No marker in the original body → original counts as 1, first recurrence is 2.
	if count != 2 {
		t.Errorf("count = %d, want 2", count)
	}

	var updateVars, commentVars map[string]any
	for _, c := range *calls {
		if strings.Contains(c.query, "UpdateDescription") {
			updateVars = c.variables
		}
		if strings.Contains(c.query, "AddComment") {
			commentVars = c.variables
		}
	}
	desc, _ := updateVars["description"].(string)
	if !strings.Contains(desc, "<!-- occurrences: 2 -->") {
		t.Errorf("bumped description missing the occurrences:2 marker, got %q", desc)
	}
	if !strings.Contains(desc, "the original body") {
		t.Errorf("bump must preserve the original body, got %q", desc)
	}
	input, _ := commentVars["input"].(map[string]any)
	body, _ := input["body"].(string)
	if !strings.Contains(body, "BEH-370") || !strings.Contains(body, "occurrence 2") {
		t.Errorf("comment = %q, want it to name the pipeline ticket and occurrence 2", body)
	}
	if id, _ := input["issueId"].(string); id != "uuid-1" {
		t.Errorf("comment issueId = %q, want the resolved UUID uuid-1", id)
	}
}

// An existing marker is incremented in place, not duplicated.
func TestRecordOccurrenceIncrementsExistingMarker(t *testing.T) {
	tr, calls := routingTransport(t, map[string]any{
		"OccurrenceContext": map[string]any{
			"issue": map[string]any{"id": "uuid-1", "description": "body\n\n<!-- occurrences: 3 -->"},
		},
		"UpdateDescription": map[string]any{"issueUpdate": map[string]any{"success": true}},
		"AddComment":        map[string]any{"commentCreate": map[string]any{"success": true}},
	})

	count, err := NewClient(tr, testOptions()).RecordOccurrence("BEH-405", "BEH-370")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if count != 4 {
		t.Errorf("count = %d, want 4 (3 + 1)", count)
	}
	var desc string
	for _, c := range *calls {
		if strings.Contains(c.query, "UpdateDescription") {
			desc, _ = c.variables["description"].(string)
		}
	}
	if strings.Count(desc, "<!-- occurrences:") != 1 {
		t.Errorf("expected exactly one marker after the bump, got %q", desc)
	}
	if !strings.Contains(desc, "<!-- occurrences: 4 -->") {
		t.Errorf("expected the marker bumped to 4, got %q", desc)
	}
}

func TestRecordOccurrenceErrorsWhenIssueMissing(t *testing.T) {
	tr, _ := routingTransport(t, map[string]any{
		"OccurrenceContext": map[string]any{"issue": nil},
	})
	if _, err := NewClient(tr, testOptions()).RecordOccurrence("BEH-405", "BEH-370"); err == nil {
		t.Error("expected an error when the issue is not found, got nil")
	}
}

func TestExtractOccurrencesDefaultsToOne(t *testing.T) {
	if got := extractOccurrences("a body with no marker"); got != 1 {
		t.Errorf("extractOccurrences(no marker) = %d, want 1", got)
	}
	if got := extractOccurrences("x <!-- occurrences: 5 --> y"); got != 5 {
		t.Errorf("extractOccurrences(marker 5) = %d, want 5", got)
	}
}

func TestWithOccurrencesAppendsAndReplaces(t *testing.T) {
	appended := withOccurrences("body", 2)
	if !strings.Contains(appended, "body") || !strings.Contains(appended, "<!-- occurrences: 2 -->") {
		t.Errorf("withOccurrences append = %q", appended)
	}
	replaced := withOccurrences("body <!-- occurrences: 2 -->", 3)
	if strings.Count(replaced, "occurrences:") != 1 || !strings.Contains(replaced, "<!-- occurrences: 3 -->") {
		t.Errorf("withOccurrences replace = %q", replaced)
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

// BEH-581: when the pre-push conflict-resolution session can't land the rebase,
// the harness posts a breadcrumb comment so the stranded-but-reviewed work
// surfaces autonomously instead of sitting silent in a worktree. AddComment
// resolves the issue's node UUID first (commentCreate keys off the UUID, not the
// human identifier) then posts the comment body.
func TestAddCommentResolvesIDThenComments(t *testing.T) {
	var calls []call
	tr := func(query string, variables map[string]any) (json.RawMessage, error) {
		calls = append(calls, call{query: query, variables: variables})
		// First call resolves the node id; second creates the comment.
		if len(calls) == 1 {
			return json.RawMessage(`{"issue":{"id":"node-uuid"}}`), nil
		}
		return json.RawMessage(`{"commentCreate":{"success":true}}`), nil
	}

	if err := NewClient(tr, testOptions()).AddComment("BEH-581", "branch needs a manual rebase"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(calls) != 2 {
		t.Fatalf("expected an id-resolve then a commentCreate, got %d calls", len(calls))
	}
	// The id-resolve is keyed by the human identifier.
	if calls[0].variables["id"] != "BEH-581" {
		t.Errorf("first call should resolve by identifier, got %v", calls[0].variables["id"])
	}
	// The comment is posted against the resolved UUID with the given body.
	input, ok := calls[1].variables["input"].(map[string]any)
	if !ok {
		t.Fatalf("commentCreate input not a map: %v", calls[1].variables["input"])
	}
	if input["issueId"] != "node-uuid" {
		t.Errorf("comment should key off the resolved UUID, got %v", input["issueId"])
	}
	if input["body"] != "branch needs a manual rebase" {
		t.Errorf("comment body = %v, want the passed body", input["body"])
	}
}

// A failure resolving the issue surfaces as an error (the caller degrades on it
// best-effort) — it must never silently swallow a missing issue.
func TestAddCommentErrorsWhenIssueNotFound(t *testing.T) {
	tr := func(string, map[string]any) (json.RawMessage, error) {
		return json.RawMessage(`{"issue":null}`), nil
	}
	if err := NewClient(tr, testOptions()).AddComment("BEH-404", "x"); err == nil {
		t.Error("expected an error when the issue does not resolve")
	}
}

// --- Consumer-configured findings label (BEH-641) ---

// Linear's create API takes a label UUID while its issue filter matches labels
// by name, so the adapter carries both spellings. Filing must use the Consumer's
// UUID, not the workspace UUID that used to be a package constant.
func TestFileFindingUsesTheConfiguredLabelID(t *testing.T) {
	var captured map[string]any
	opts := Options{TeamKey: "PROJ", Ready: "agent-ready", FindingsLabelID: "consumer-label-uuid"}

	if _, err := NewClient(fileFindingTransport(t, &captured), opts).FileFinding(
		findings.Finding{Title: "t", Body: "b", Kind: "setup"},
		tracker.FileFindingOptions{TeamID: "team-uuid", RelatedKey: "PROJ-1"},
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, _ := captured["input"].(map[string]any)
	labelIDs, _ := input["labelIds"].([]string)
	if len(labelIDs) != 1 || labelIDs[0] != "consumer-label-uuid" {
		t.Errorf("labelIds = %v, want [consumer-label-uuid]", input["labelIds"])
	}
}

// Filing is best-effort by contract (ADR-0001), so an unconfigured label files
// the finding unlabelled rather than refusing it or sending an empty label id.
func TestFileFindingWithNoLabelConfiguredFilesUnlabelled(t *testing.T) {
	var captured map[string]any
	opts := Options{TeamKey: "PROJ", Ready: "agent-ready"}

	if _, err := NewClient(fileFindingTransport(t, &captured), opts).FileFinding(
		findings.Finding{Title: "t", Body: "b", Kind: "setup"},
		tracker.FileFindingOptions{TeamID: "team-uuid", RelatedKey: "PROJ-1"},
	); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	input, _ := captured["input"].(map[string]any)
	if labelIDs, _ := input["labelIds"].([]string); len(labelIDs) != 0 {
		t.Errorf("labelIds = %v, want none — an unset label must not send an empty id", labelIDs)
	}
}

func TestSearchFindingsScopesToTheConfiguredLabelName(t *testing.T) {
	tr, calls := transportReturning(t, map[string]any{"issues": map[string]any{"nodes": []any{}}})
	opts := Options{TeamKey: "PROJ", Ready: "agent-ready", FindingsLabel: "consumer-findings"}

	if _, err := NewClient(tr, opts).SearchFindings("team-uuid"); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) == 0 {
		t.Fatal("no GraphQL call recorded")
	}
	filter, _ := (*calls)[0].variables["filter"].(map[string]any)
	labels, _ := filter["labels"].(map[string]any)
	name, _ := labels["name"].(map[string]any)
	if name["eq"] != "consumer-findings" {
		t.Errorf("labels.name.eq = %v, want consumer-findings", name["eq"])
	}
}

// With no findings label the dedup search must return nothing rather than widen
// to the whole team: a duplicate filed finding is a smaller harm than
// mis-deduping a real one against an unrelated issue.
func TestSearchFindingsWithNoLabelConfiguredReturnsNothing(t *testing.T) {
	tr, calls := transportReturning(t, map[string]any{"issues": map[string]any{"nodes": []any{}}})
	opts := Options{TeamKey: "PROJ", Ready: "agent-ready"}

	got, err := NewClient(tr, opts).SearchFindings("team-uuid")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("got %d findings, want none", len(got))
	}
	if len(*calls) != 0 {
		t.Errorf("an unscoped dedup search must not query Linear at all, saw %d call(s)", len(*calls))
	}
}
