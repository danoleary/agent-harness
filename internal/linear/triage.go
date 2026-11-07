package linear

import (
	"encoding/json"
	"fmt"
	"sort"

	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

var _ tracker.Backlog = (*Client)(nil)

// Untriaged lists the team's unassigned issues in a backlog or unstarted state
// that carry neither the ready label nor the blocked one (ADR-0044): the tickets a
// triage loop judges. It reads with the selection query, so each issue arrives
// with the same fields and is ordered as selection orders the ready queue:
// priority, then board order, then age.
func (c *Client) Untriaged() ([]ticket.Ticket, error) {
	filter := map[string]any{
		"team":     map[string]any{"key": map[string]any{"eq": c.opts.TeamKey}},
		"state":    map[string]any{"type": map[string]any{"in": []string{"backlog", "unstarted"}}},
		"assignee": map[string]any{"null": true},
	}
	data, err := c.transport(selectNextQuery, map[string]any{"filter": filter})
	if err != nil {
		return nil, err
	}
	var resp struct {
		Issues struct {
			Nodes []selectedIssue `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}
	var keep []selectedIssue
	for _, iss := range resp.Issues.Nodes {
		if hasLabel(iss, c.opts.Ready) || (c.opts.Blocked != "" && hasLabel(iss, c.opts.Blocked)) {
			continue
		}
		keep = append(keep, iss)
	}
	sort.SliceStable(keep, func(i, j int) bool { return less(keep[i], keep[j]) })
	out := make([]ticket.Ticket, 0, len(keep))
	for _, iss := range keep {
		out = append(out, toTicket(iss))
	}
	return out, nil
}

// MarkReady puts a ticket in the ready queue (ADR-0044). A ticket in a backlog
// state moves to the team's unstarted "Todo" state first, then gets the ready
// label. In that order, a failed label leaves an unlabelled Todo ticket, which
// Untriaged still lists, so the next pass finishes the job; the other order would
// strand a labelled ticket in Backlog, where neither selection nor triage looks.
// A ticket a human has meanwhile moved to any other state keeps it.
func (c *Client) MarkReady(identifier string) error {
	if c.opts.Ready == "" {
		return fmt.Errorf("no tracker.ready_label is configured to mark %s with", identifier)
	}
	iss, err := c.resolveLabel(identifier, c.opts.Ready)
	if err != nil {
		return err
	}
	if iss.stateType == "backlog" {
		if err := c.moveToState(identifier, `unstarted "Todo"`, func(s []issueState) string {
			return pickState(s, "unstarted", "Todo")
		}); err != nil {
			return err
		}
	}
	_, err = c.transport(addLabelMutation, map[string]any{"id": iss.id, "labelId": iss.labelID})
	return err
}
