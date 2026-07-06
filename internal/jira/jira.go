// Package jira is the host-only Jira Cloud adapter behind the tracker port
// (ADR-0010). It maps the three ADR-0010 layers onto Jira issues: ticket source
// = an issue's summary/description plus its sub-tasks (normalized to "children");
// queue = a configured "ready" JQL; claim/release = named status transitions;
// findings sink = issue creation with a body-marker dedup. The Key is the Jira
// issue key (`PROJ-123`). Like the Linear and GitHub adapters, all I/O stays on
// the host (ADR-0001): the credential lives in the transport and never crosses
// the sandbox boundary.
//
// It speaks Jira REST API v2, deliberately: v2 returns issue descriptions and
// comment bodies as plain strings (wiki markup), whereas v3 forces the rich
// Atlassian Document Format (ADF) JSON. The harness only injects readable text
// into a prompt and files plain-text findings, so v2's strings are the right LCD
// shape — the ADF round-trip buys nothing here and leaks a Jira-unique format.
//
// The interface stays LCD: Jira-unique shapes (epic links, issue-link "blocks"
// relations, workflow-specific transition graphs) are normalised to the port's
// vocabulary or dropped, never leaked out.
package jira

import (
	"encoding/json"
	"net/url"

	"github.com/beherd/agent-harness/internal/ticket"
	"github.com/beherd/agent-harness/internal/tracker"
)

// Client is the Jira adapter behind the host-side tracker port (ADR-0010).
var _ tracker.Tracker = (*Client)(nil)

// Transport issues one Jira REST request and returns the response body as raw
// JSON. It holds the host-only credential (base URL + email + API token, Basic
// auth) and is exercised in the unit suite via a fake, so it stays thin and out
// of the suite (mirroring the Linear/GitHub transport seams).
type Transport func(method, path string, body any) (json.RawMessage, error)

// Options is the non-secret selection surface the adapter keys queue + claim +
// findings off — the Jira analogue of the GitHub adapter's label Options,
// sourced from `.agent-harness/config` so a Consumer names them per instance.
type Options struct {
	// ProjectKey is the Jira project findings are filed into and the ticket's
	// TeamID (the tracker-native container, per the port). Opaque to the harness.
	ProjectKey string
	// ReadyJQL is the human-gated "ready for the agent" queue, expressed as a JQL
	// query (the Jira twin of the GitHub ready label). The selector runs it and
	// takes the top result; the JQL itself encodes ready/blocked/ordering, so no
	// separate blocked screen is needed (LCD: issue-link relations dropped).
	ReadyJQL string
	// InProgressJQL is the reaper's read: the JQL for the agent-claimed In Progress
	// set (BEH-677), so a claim stranded by a dead agent can be released.
	InProgressJQL string
	// Findings is the Jira label every harness finding is filed under, so dedup
	// (SearchFindings) never trips over unrelated project issues.
	Findings string
	// FindingsIssueType is the issue type new findings are created as (Jira
	// requires one). Instance-specific; empty defaults to "Task" (see NewClient).
	FindingsIssueType string
	// InProgressTransition is the workflow transition name applied to claim a
	// ticket (Todo → In Progress). Named, not an id: transition ids are
	// instance-specific and vary by source status, so the adapter resolves the
	// name against the issue's available transitions at claim time.
	InProgressTransition string
	// TodoTransition is the transition name that releases a claim that yielded
	// nothing (In Progress → Todo), returning the ticket to the ready queue.
	TodoTransition string
	// CanceledTransition is the transition name into the terminal canceled state,
	// consuming a recommend-close verdict (BEH-682).
	CanceledTransition string
}

// Client is the Jira Cloud adapter behind the host-side tracker port.
type Client struct {
	transport Transport
	baseURL   string
	opts      Options
}

// NewClient builds a Client bound to one Jira site (baseURL, e.g.
// https://acme.atlassian.net) with the given selection surface. baseURL is used
// to derive human browse URLs; the transport holds the same base for API calls.
func NewClient(t Transport, baseURL string, opts Options) *Client {
	if opts.FindingsIssueType == "" {
		opts.FindingsIssueType = "Task"
	}
	return &Client{transport: t, baseURL: baseURL, opts: opts}
}

// browseURL renders the human-facing web URL for an issue key.
func (c *Client) browseURL(key string) string {
	return c.baseURL + "/browse/" + key
}

// issuePath builds the REST path for one issue key, with an optional
// query-string tail (fields, expand). key is path-escaped so a well-formed
// `PROJ-123` passes through unchanged.
func (c *Client) issuePath(key, query string) string {
	p := "/rest/api/2/issue/" + url.PathEscape(key)
	if query != "" {
		p += "?" + query
	}
	return p
}

// issue is the subset of a Jira issue the adapter reads.
type issue struct {
	Key    string `json:"key"`
	Fields struct {
		Summary     string `json:"summary"`
		Description string `json:"description"`
		Subtasks    []struct {
			Key    string `json:"key"`
			Fields struct {
				Summary string `json:"summary"`
			} `json:"fields"`
		} `json:"subtasks"`
	} `json:"fields"`
}

// FetchTicket resolves a PROJ-123 Key to the harness ticket shape via one issue
// GET. Jira has no cross-tracker priority concept the port carries, so Priority
// is left empty (LCD).
func (c *Client) FetchTicket(key string) (ticket.Ticket, error) {
	data, err := c.transport("GET", c.issuePath(key, "fields=summary,description,subtasks"), nil)
	if err != nil {
		return ticket.Ticket{}, err
	}
	var iss issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return ticket.Ticket{}, err
	}
	t := ticket.Ticket{
		Identifier:  iss.Key,
		Title:       iss.Fields.Summary,
		Description: iss.Fields.Description,
		URL:         c.browseURL(iss.Key),
		TeamID:      c.opts.ProjectKey,
	}
	// Follow sub-tasks so an umbrella issue's real work is inlined into the prompt
	// host-side — the sandbox never reaches Jira (ADR-0002/BEH-619). The subtask
	// entries carry no description, so each is fetched for its full spec.
	for _, sub := range iss.Fields.Subtasks {
		child, err := c.fetchChild(sub.Key)
		if err != nil {
			return ticket.Ticket{}, err
		}
		t.SubIssues = append(t.SubIssues, child)
	}
	return t, nil
}

// fetchChild resolves one sub-task to a SubIssue (summary + description).
func (c *Client) fetchChild(key string) (ticket.SubIssue, error) {
	data, err := c.transport("GET", c.issuePath(key, "fields=summary,description"), nil)
	if err != nil {
		return ticket.SubIssue{}, err
	}
	var iss issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return ticket.SubIssue{}, err
	}
	return ticket.SubIssue{
		Identifier:  iss.Key,
		Title:       iss.Fields.Summary,
		Description: iss.Fields.Description,
	}, nil
}
