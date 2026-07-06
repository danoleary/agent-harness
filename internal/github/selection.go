package github

import (
	"encoding/json"
	"net/url"

	"github.com/beherd/agent-harness/internal/ticket"
)

// selectQuery builds the list-issues query for the ready queue: the human-gated
// working set only — open, unassigned, carrying the ready label — ordered oldest
// first. GitHub has no board sort order or numeric priority (the Linear ordering
// signals), so created-ascending is the natural FIFO fallback (LCD: priority /
// board order dropped, not leaked into the port).
func (c *Client) selectQuery() string {
	v := url.Values{}
	v.Set("state", "open")
	v.Set("assignee", "none")
	v.Set("labels", c.opts.Ready)
	v.Set("sort", "created")
	v.Set("direction", "asc")
	v.Set("per_page", "100")
	return c.issuesPath("") + "?" + v.Encode()
}

// SelectNextTicket resolves the top-of-queue eligible ticket: the first open,
// unassigned, ready-labelled issue that is neither Blocked-labelled nor a pull
// request (the issues API returns PRs too). It is a PURE READ — claim-on-select
// (ADR-0003) is the caller composing it with MoveToInProgress — so dry-run can
// resolve a ticket without mutating GitHub. ok=false means the queue is empty.
func (c *Client) SelectNextTicket() (ticket.Ticket, bool, error) {
	data, err := c.transport("GET", c.selectQuery(), nil)
	if err != nil {
		return ticket.Ticket{}, false, err
	}
	var issues []issue
	if err := json.Unmarshal(data, &issues); err != nil {
		return ticket.Ticket{}, false, err
	}
	for _, iss := range issues {
		if !c.eligible(iss) {
			continue
		}
		return ticket.Ticket{
			Identifier:  numberToKey(iss.Number),
			Title:       iss.Title,
			Description: iss.Body,
			URL:         iss.HTMLURL,
			TeamID:      c.slug(),
		}, true, nil
	}
	return ticket.Ticket{}, false, nil
}

// eligible reports whether a listed issue may be auto-worked: it is a real issue
// (not a PR) and is not flagged Blocked. The ready label is already guaranteed by
// the query; GitHub has no native blocks relation, so blocked-by is expressed by
// the human-applied Blocked label alone (LCD: relation traversal dropped).
func (c *Client) eligible(iss issue) bool {
	if iss.PullRequest != nil {
		return false
	}
	if c.hasLabel(iss, c.opts.Blocked) {
		return false
	}
	return true
}

// addLabel adds one label to an issue via the labels REST endpoint.
func (c *Client) addLabel(number, label string) error {
	_, err := c.transport("POST", c.issuesPath(number)+"/labels", map[string]any{"labels": []string{label}})
	return err
}

// removeLabel removes one label from an issue. The label name is path-escaped so a
// label containing a space or slash addresses the right resource.
func (c *Client) removeLabel(number, label string) error {
	_, err := c.transport("DELETE", c.issuesPath(number)+"/labels/"+url.PathEscape(label), nil)
	return err
}

// setAssignee assigns / unassigns the configured bot. A no-op when no assignee is
// configured — the label transition alone still claims/releases the ticket.
func (c *Client) setAssignee(number, method string) error {
	if c.opts.Assignee == "" {
		return nil
	}
	_, err := c.transport(method, c.issuesPath(number)+"/assignees", map[string]any{"assignees": []string{c.opts.Assignee}})
	return err
}

// MoveToInProgress claims a ticket: it transitions the label column
// ready→in-progress (add in-progress, then remove ready) and assigns the bot.
// Adding in-progress first means a mid-transition failure leaves the ticket
// discoverable by the reaper rather than orphaned with neither label. Removing
// the ready label is what drops it from the select queue (the list query requires
// that label). GitHub issues have no workflow states, so this label column IS the
// "In Progress" state (LCD: state normalised to a label).
func (c *Client) MoveToInProgress(key string) error {
	n := keyToNumber(key)
	if err := c.addLabel(n, c.opts.InProgress); err != nil {
		return err
	}
	if err := c.removeLabel(n, c.opts.Ready); err != nil {
		return err
	}
	return c.setAssignee(n, "POST")
}

// ReleaseToTodo undoes a claim that yielded nothing: it transitions the label
// column back in-progress→ready and unassigns the bot, returning the ticket to the
// select queue for a later run (BEH-543).
func (c *Client) ReleaseToTodo(key string) error {
	n := keyToNumber(key)
	if err := c.addLabel(n, c.opts.Ready); err != nil {
		return err
	}
	if err := c.removeLabel(n, c.opts.InProgress); err != nil {
		return err
	}
	return c.setAssignee(n, "DELETE")
}

// MoveToCanceled closes the issue with state_reason not_planned — GitHub's
// terminal "won't do" state, the analogue of a Linear Canceled state — consuming a
// recommend-close verdict (BEH-682). A closed issue leaves both the select and
// reap pools (both scope to open issues), so it can never re-enter the pipeline.
func (c *Client) MoveToCanceled(key string) error {
	_, err := c.transport("PATCH", c.issuesPath(keyToNumber(key)), map[string]any{
		"state":        "closed",
		"state_reason": "not_planned",
	})
	return err
}

// hasLabel reports whether an issue carries the named label (empty name never
// matches, so an unconfigured Blocked label screens nothing).
func (c *Client) hasLabel(iss issue, name string) bool {
	if name == "" {
		return false
	}
	for _, l := range iss.Labels {
		if l.Name == name {
			return true
		}
	}
	return false
}
