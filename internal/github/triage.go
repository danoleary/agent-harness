package github

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

var _ tracker.Backlog = (*Client)(nil)

// untriagedQuery lists the repo's open, unassigned issues oldest first: the
// working set Untriaged filters. One page of 100 is enough, as the triage loop
// takes one ticket per poll and a judged ticket leaves the set or is skipped.
func (c *Client) untriagedQuery() string {
	v := url.Values{}
	v.Set("state", "open")
	v.Set("assignee", "none")
	v.Set("sort", "created")
	v.Set("direction", "asc")
	v.Set("per_page", "100")
	return c.issuesPath("") + "?" + v.Encode()
}

// Untriaged lists the open, unassigned issues that carry none of the ready,
// in-progress and blocked labels (ADR-0044): the tickets a triage loop judges.
// Pull requests, which the issues API also returns, are dropped. GitHub has no
// priority, so the order is oldest first, as in selection.
func (c *Client) Untriaged() ([]ticket.Ticket, error) {
	data, err := c.transport("GET", c.untriagedQuery(), nil)
	if err != nil {
		return nil, err
	}
	var issues []issue
	if err := json.Unmarshal(data, &issues); err != nil {
		return nil, err
	}
	var out []ticket.Ticket
	for _, iss := range issues {
		if iss.PullRequest != nil || c.hasLabel(iss, c.opts.Ready) || c.hasLabel(iss, c.opts.InProgress) || c.hasLabel(iss, c.opts.Blocked) {
			continue
		}
		out = append(out, ticket.Ticket{
			Identifier:  numberToKey(iss.Number),
			Title:       iss.Title,
			Description: iss.Body,
			URL:         iss.HTMLURL,
			TeamID:      c.slug(),
			Labels:      labelNames(iss),
		})
	}
	return out, nil
}

// MarkReady adds the ready label, which is all the ready queue reads: GitHub
// issues have no backlog state to move out of.
func (c *Client) MarkReady(key string) error {
	if c.opts.Ready == "" {
		return fmt.Errorf("no tracker.ready_label is configured to mark %s with", key)
	}
	return c.addLabel(keyToNumber(key), c.opts.Ready)
}
