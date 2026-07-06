package jira

import (
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/beherd/agent-harness/internal/ticket"
)

// searchPath builds the enhanced JQL search request (`/search/jql`, the
// non-deprecated successor to `/search`). The requested fields must be named
// explicitly — the enhanced endpoint returns only id+key by default.
func searchPath(jql, fields string) string {
	v := url.Values{}
	v.Set("jql", jql)
	v.Set("fields", fields)
	v.Set("maxResults", "50")
	return "/rest/api/2/search/jql?" + v.Encode()
}

// searchResult is the subset of a JQL search response the adapter reads.
type searchResult struct {
	Issues []issue `json:"issues"`
}

// SelectNextTicket resolves the top-of-queue ready ticket by running the
// configured ready JQL and taking the first result. The JQL encodes readiness,
// blocked-exclusion, and ordering, so there is no separate eligibility screen
// (LCD: issue-link relations dropped). It is a PURE READ — claim-on-select
// (ADR-0003) is the caller composing it with MoveToInProgress — so dry-run can
// resolve a ticket without mutating Jira. ok=false means the queue is empty.
func (c *Client) SelectNextTicket() (ticket.Ticket, bool, error) {
	data, err := c.transport("GET", searchPath(c.opts.ReadyJQL, "summary,description"), nil)
	if err != nil {
		return ticket.Ticket{}, false, err
	}
	var res searchResult
	if err := json.Unmarshal(data, &res); err != nil {
		return ticket.Ticket{}, false, err
	}
	if len(res.Issues) == 0 {
		return ticket.Ticket{}, false, nil
	}
	iss := res.Issues[0]
	return ticket.Ticket{
		Identifier:  iss.Key,
		Title:       iss.Fields.Summary,
		Description: iss.Fields.Description,
		URL:         c.browseURL(iss.Key),
		TeamID:      c.opts.ProjectKey,
	}, true, nil
}

// availableTransition is one entry in an issue's available-transitions list.
type availableTransition struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// transition performs a named workflow transition on an issue: it lists the
// issue's currently-available transitions, resolves the configured NAME to its
// instance-specific id, and POSTs it. Named resolution (not a hardcoded id) is
// what keeps the config portable — ids differ per instance and per source status.
// An unknown name is a loud error: a claim that silently no-ops would strand the
// ticket in the queue for every future run.
func (c *Client) transition(key, name string) error {
	if name == "" {
		return fmt.Errorf("no transition configured for issue %s", key)
	}
	data, err := c.transport("GET", "/rest/api/2/issue/"+url.PathEscape(key)+"/transitions", nil)
	if err != nil {
		return err
	}
	var body struct {
		Transitions []availableTransition `json:"transitions"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return err
	}
	for _, tr := range body.Transitions {
		if tr.Name == name {
			_, err := c.transport("POST", "/rest/api/2/issue/"+url.PathEscape(key)+"/transitions", map[string]any{
				"transition": map[string]any{"id": tr.ID},
			})
			return err
		}
	}
	return fmt.Errorf("transition %q not available on issue %s", name, key)
}

// MoveToInProgress claims a ticket by applying the configured in-progress
// transition. Moving the ticket out of the ready status is what drops it from the
// select queue and surfaces it to the reaper.
func (c *Client) MoveToInProgress(key string) error {
	return c.transition(key, c.opts.InProgressTransition)
}

// ReleaseToTodo undoes a claim that yielded nothing, returning the ticket to the
// ready queue via the configured Todo transition (BEH-543).
func (c *Client) ReleaseToTodo(key string) error {
	return c.transition(key, c.opts.TodoTransition)
}

// MoveToCanceled consumes a recommend-close verdict (BEH-682) via the configured
// terminal Cancel transition, so a superseded/duplicate ticket leaves both the
// select and reap pools.
func (c *Client) MoveToCanceled(key string) error {
	return c.transition(key, c.opts.CanceledTransition)
}
