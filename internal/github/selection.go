package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strconv"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// selectQuery builds the list-issues query for the ready queue: the human-gated
// working set only — open, unassigned, carrying the ready label — ordered oldest
// first. GitHub has no board sort order or numeric priority (the Linear ordering
// signals), so created-ascending is the natural FIFO fallback (LCD: priority /
// board order dropped, not leaked into the port). Each listed issue carries its
// issue_dependencies_summary, which eligible reads to screen native blockers.
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
// unassigned, ready-labelled issue that is not Blocked-labelled, not a pull
// request (the issues API returns PRs too), not blocked by a still-open issue, and
// not building on a still-open PR its body names (#82).
// A failed blocker read is an error, not an unblocked ticket (fail closed). It is
// a PURE READ — claim-on-select (ADR-0003) is the caller composing it with
// MoveToInProgress — so dry-run can resolve a ticket without mutating GitHub.
// ok=false means the queue is empty.
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
		ok, err := c.eligible(iss)
		if err != nil {
			return ticket.Ticket{}, false, err
		}
		if !ok {
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
// (not a PR), is not flagged Blocked, and no still-open issue blocks it through
// GitHub's native "blocked by" dependency. Closed blockers do not block, matching
// the Linear adapter. The ready label is already guaranteed by the query. The
// blockers are fetched only when the list's dependency summary reports any. Nor
// may the body name a still-open PR (#82): that is a prerequisite not yet on main.
func (c *Client) eligible(iss issue) (bool, error) {
	if iss.PullRequest != nil {
		return false, nil
	}
	if c.hasLabel(iss, c.opts.Blocked) {
		return false, nil
	}
	if iss.IssueDependenciesSummary.TotalBlockedBy > 0 {
		blocker, err := c.openBlocker(iss.Number)
		if err != nil {
			return false, err
		}
		if blocker != 0 {
			fmt.Fprintf(c.log, "%s skipped: blocked by open %s\n", numberToKey(iss.Number), numberToKey(blocker))
			return false, nil
		}
	}
	// A prerequisite named only in prose, or one that is an open PR rather than an
	// issue, is invisible to the native relation. Working the ticket now would build
	// on a base that changes underneath it when that PR merges (#82), so hold it.
	prs, err := c.openPRs(iss.Body, iss.Number)
	if err != nil {
		return false, err
	}
	if len(prs) > 0 {
		fmt.Fprintf(c.log, "%s skipped: builds on open PR %s\n", numberToKey(iss.Number), numberToKey(prs[0].Number))
		return false, nil
	}
	return true, nil
}

// openBlocker returns the number of the first still-open issue blocking issue n,
// or 0 when every blocker is closed. per_page=100 reads every blocker in one page
// (GitHub caps an issue's blocked-by list below that), so an open blocker past the
// default page of 30 is never missed.
func (c *Client) openBlocker(n int) (int, error) {
	data, err := c.transport("GET", c.issuesPath(strconv.Itoa(n))+"/dependencies/blocked_by?per_page=100", nil)
	if err != nil {
		return 0, fmt.Errorf("read blockers of %s: %w", numberToKey(n), err)
	}
	var blockers []issue
	if err := json.Unmarshal(data, &blockers); err != nil {
		return 0, fmt.Errorf("read blockers of %s: %w", numberToKey(n), err)
	}
	for _, b := range blockers {
		if b.State == "open" {
			return b.Number, nil
		}
	}
	return 0, nil
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
