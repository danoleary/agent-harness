package linear

import (
	"encoding/json"
	"strings"
	"testing"
)

// issueNode builds one issue node for the SelectNextTicket fake response. labels
// and blockers default to the eligible shape (carries ready-for-agent, nothing
// blocking) unless overridden via the option funcs.
type issueNode struct {
	identifier string
	priority   int
	sortOrder  float64
	createdAt  string
	// title and description default to a benign placeholder; set them to exercise
	// content-based routing (the telemetry-remeasure skip).
	title       string
	description string
	labels      []string
	// blockers each model one inverse "blocks" relation: the state type of the
	// issue that blocks this one ("started"/"completed"/…).
	blockers []string
	// children each model one sub-issue: the state type of that child
	// ("started"/"unstarted"/"completed"/…). A non-empty children list marks this
	// node an umbrella/tracker issue.
	children []string
}

func (n issueNode) toMap() map[string]any {
	labelNodes := make([]any, 0, len(n.labels))
	for _, name := range n.labels {
		labelNodes = append(labelNodes, map[string]any{"name": name})
	}
	relNodes := make([]any, 0, len(n.blockers))
	for _, stateType := range n.blockers {
		relNodes = append(relNodes, map[string]any{
			"type":  "blocks",
			"issue": map[string]any{"state": map[string]any{"type": stateType}},
		})
	}
	childNodes := make([]any, 0, len(n.children))
	for _, stateType := range n.children {
		childNodes = append(childNodes, map[string]any{
			"state": map[string]any{"type": stateType},
		})
	}
	title := n.title
	if title == "" {
		title = n.identifier + " title"
	}
	description := n.description
	if description == "" {
		description = "body"
	}
	return map[string]any{
		"identifier":       n.identifier,
		"title":            title,
		"description":      description,
		"url":              "https://linear.app/beherd/issue/" + n.identifier,
		"priorityLabel":    "Urgent",
		"priority":         n.priority,
		"sortOrder":        n.sortOrder,
		"createdAt":        n.createdAt,
		"team":             map[string]any{"id": "team-uuid"},
		"labels":           map[string]any{"nodes": labelNodes},
		"inverseRelations": map[string]any{"nodes": relNodes},
		"children":         map[string]any{"nodes": childNodes},
	}
}

// selectTransport returns a Transport answering the SelectNextTicket query with
// the given nodes, recording the calls it saw.
func selectTransport(t *testing.T, nodes ...issueNode) (Transport, *[]call) {
	t.Helper()
	var calls []call
	tr := func(query string, variables map[string]any) (json.RawMessage, error) {
		calls = append(calls, call{query: query, variables: variables})
		ns := make([]any, 0, len(nodes))
		for _, n := range nodes {
			ns = append(ns, n.toMap())
		}
		b, err := json.Marshal(map[string]any{"issues": map[string]any{"nodes": ns}})
		if err != nil {
			t.Fatalf("marshal fake: %v", err)
		}
		return b, nil
	}
	return tr, &calls
}

// eligibleNode is the canonical agent-workable node: carries ready-for-agent, no
// Blocked label, no open blocker.
func eligibleNode(id string, priority int) issueNode {
	return issueNode{identifier: id, priority: priority, labels: []string{"ready-for-agent"}}
}

func TestSelectNextTicketReturnsEligibleTicketWithoutClaiming(t *testing.T) {
	tr, calls := selectTransport(t, eligibleNode("BEH-100", 1))

	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok {
		t.Fatal("expected a ticket to be selected, got none")
	}
	if got.Identifier != "BEH-100" {
		t.Errorf("selected = %q, want BEH-100", got.Identifier)
	}
	if got.TeamID != "team-uuid" {
		t.Errorf("TeamID = %q, want team-uuid (findings are filed back into it)", got.TeamID)
	}

	// Selection is a PURE READ — claim-on-select is the caller composing this with
	// MoveToInProgress, so dry-run can resolve a ticket without mutating Linear. A
	// claim would issue an issueUpdate mutation; there must be none.
	for _, c := range *calls {
		if strings.Contains(c.query, "issueUpdate") {
			t.Errorf("SelectNextTicket must not mutate Linear, saw issueUpdate: %s", c.query)
		}
	}
}

// An empty queue is a normal steady state, not an error: ok=false, no error.
func TestSelectNextTicketEmptyQueueReturnsNotOK(t *testing.T) {
	tr, _ := selectTransport(t)
	_, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("an empty queue must return ok=false, not a ticket")
	}
}

// The query must scope the fetch to the BeHerd backlog's auto-workable candidate
// set: team key BEH, Todo-type (unstarted) state, unassigned, and ready-for-agent —
// the last filtered in GraphQL (not only in eligible) so the first:250 page is the
// small human-gated set and ordering can't miss a higher-priority ticket beyond it.
func TestSelectNextTicketQueryScopesTeamStateAssignee(t *testing.T) {
	tr, calls := selectTransport(t, eligibleNode("BEH-1", 1))
	if _, _, err := NewClient(tr).SelectNextTicket(); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(*calls) != 1 {
		t.Fatalf("expected one query, got %d", len(*calls))
	}
	raw, err := json.Marshal((*calls)[0].variables)
	if err != nil {
		t.Fatalf("marshal variables: %v", err)
	}
	vars := string(raw)
	for _, want := range []string{"BEH", "unstarted", "assignee", "null", "ready-for-agent"} {
		if !strings.Contains(vars, want) {
			t.Errorf("query filter missing %q: %s", want, vars)
		}
	}
}

// ready-for-agent is the human gate: a Todo, unassigned ticket WITHOUT it is never
// auto-selected. Here the only labelled ticket lacks ready-for-agent, so the queue is
// effectively empty.
func TestSelectNextTicketSkipsTicketsWithoutAgentReady(t *testing.T) {
	noLabel := issueNode{identifier: "BEH-2", priority: 1, labels: []string{"bug"}}
	tr, _ := selectTransport(t, noLabel)
	_, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ok {
		t.Error("a ticket without the ready-for-agent label must not be selected")
	}
}

// A ticket a human flagged Blocked is skipped even if it carries ready-for-agent and
// nothing else blocks it.
func TestSelectNextTicketSkipsBlockedLabel(t *testing.T) {
	blocked := issueNode{identifier: "BEH-3", priority: 1, labels: []string{"ready-for-agent", "Blocked"}}
	ready := eligibleNode("BEH-4", 2)
	tr, _ := selectTransport(t, blocked, ready)
	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || got.Identifier != "BEH-4" {
		t.Errorf("selected = %q (ok=%v), want BEH-4 — the Blocked ticket must be skipped despite higher priority", got.Identifier, ok)
	}
}

// A ticket blocked by a still-OPEN issue (a "blocks" inverse relation whose source
// is started) is skipped; the lower-priority unblocked ticket is taken instead.
func TestSelectNextTicketSkipsTicketBlockedByOpenIssue(t *testing.T) {
	blocked := issueNode{identifier: "BEH-5", priority: 1, labels: []string{"ready-for-agent"}, blockers: []string{"started"}}
	ready := eligibleNode("BEH-6", 3)
	tr, _ := selectTransport(t, blocked, ready)
	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || got.Identifier != "BEH-6" {
		t.Errorf("selected = %q (ok=%v), want BEH-6 — a ticket blocked by an open issue must be skipped", got.Identifier, ok)
	}
}

// A blocker that is already completed/canceled no longer blocks: a ticket whose
// only "blocks" relation points at a closed issue is eligible.
func TestSelectNextTicketIgnoresClosedBlockers(t *testing.T) {
	wasBlocked := issueNode{identifier: "BEH-7", priority: 1, labels: []string{"ready-for-agent"}, blockers: []string{"completed"}}
	tr, _ := selectTransport(t, wasBlocked)
	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || got.Identifier != "BEH-7" {
		t.Errorf("selected = %q (ok=%v), want BEH-7 — a closed blocker must not block", got.Identifier, ok)
	}
}

// An umbrella/tracker issue — one with an OPEN sub-issue (a child whose state is
// not completed/canceled) — must never be auto-selected, even with ready-for-agent
// and top priority. Its real work lives in the children; grabbing the umbrella
// would strand it In Progress producing no branch/PR. The unblocked leaf wins.
func TestSelectNextTicketSkipsUmbrellaWithOpenChild(t *testing.T) {
	umbrella := issueNode{identifier: "BEH-UMB", priority: 1, labels: []string{"ready-for-agent"}, children: []string{"unstarted"}}
	leaf := eligibleNode("BEH-LEAF", 3)
	tr, _ := selectTransport(t, umbrella, leaf)
	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || got.Identifier != "BEH-LEAF" {
		t.Errorf("selected = %q (ok=%v), want BEH-LEAF — an umbrella with an open child must be skipped despite higher priority", got.Identifier, ok)
	}
}

// A tracker issue whose children are ALL closed (completed/canceled) is no longer
// an umbrella deferring live work — it is eligible like any leaf.
func TestSelectNextTicketAllowsIssueWithOnlyClosedChildren(t *testing.T) {
	done := issueNode{identifier: "BEH-DONE", priority: 1, labels: []string{"ready-for-agent"}, children: []string{"completed", "canceled"}}
	tr, _ := selectTransport(t, done)
	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || got.Identifier != "BEH-DONE" {
		t.Errorf("selected = %q (ok=%v), want BEH-DONE — a ticket with only closed children is not an umbrella", got.Identifier, ok)
	}
}

// Ordering is by priority first: Urgent (1) outranks Low (4), and No-priority (0)
// sorts LAST despite its smaller Linear number.
func TestSelectNextTicketOrdersByPriority(t *testing.T) {
	none := eligibleNode("BEH-NONE", 0)
	low := eligibleNode("BEH-LOW", 4)
	urgent := eligibleNode("BEH-URGENT", 1)
	tr, _ := selectTransport(t, none, low, urgent)
	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || got.Identifier != "BEH-URGENT" {
		t.Errorf("selected = %q (ok=%v), want BEH-URGENT (highest priority)", got.Identifier, ok)
	}
}

// No-priority sorts after every real priority, so with only a No-priority and a
// Low ticket the Low one wins — guards the 0-sorts-last rule directly.
func TestSelectNextTicketNoPrioritySortsLast(t *testing.T) {
	none := eligibleNode("BEH-NONE", 0)
	low := eligibleNode("BEH-LOW", 4)
	tr, _ := selectTransport(t, none, low)
	got, _, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "BEH-LOW" {
		t.Errorf("selected = %q, want BEH-LOW (No-priority must sort after Low)", got.Identifier)
	}
}

// Within the same priority, ties break by board sort order ascending, then
// createdAt ascending.
func TestSelectNextTicketTieBreaksBySortOrderThenCreatedAt(t *testing.T) {
	first := issueNode{identifier: "BEH-A", priority: 2, sortOrder: 1.0, createdAt: "2026-01-01T00:00:00Z", labels: []string{"ready-for-agent"}}
	lowerSort := issueNode{identifier: "BEH-B", priority: 2, sortOrder: 0.5, createdAt: "2026-02-01T00:00:00Z", labels: []string{"ready-for-agent"}}
	tr, _ := selectTransport(t, first, lowerSort)
	got, _, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "BEH-B" {
		t.Errorf("selected = %q, want BEH-B (lower sortOrder wins the tie)", got.Identifier)
	}

	// Same sortOrder → older createdAt wins.
	older := issueNode{identifier: "BEH-OLD", priority: 2, sortOrder: 1.0, createdAt: "2026-01-01T00:00:00Z", labels: []string{"ready-for-agent"}}
	newer := issueNode{identifier: "BEH-NEW", priority: 2, sortOrder: 1.0, createdAt: "2026-03-01T00:00:00Z", labels: []string{"ready-for-agent"}}
	tr2, _ := selectTransport(t, newer, older)
	got2, _, err := NewClient(tr2).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got2.Identifier != "BEH-OLD" {
		t.Errorf("selected = %q, want BEH-OLD (older createdAt wins when sortOrder ties)", got2.Identifier)
	}
}
