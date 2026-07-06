// Package github is the host-only GitHub Issues adapter behind the tracker port
// (ADR-0010). It maps the three ADR-0010 layers onto GitHub Issues: ticket source
// = an issue's title/body plus its task-list children; queue = a configured
// "ready" label + assignee; findings sink = issue creation with a body-marker
// dedup. The Key is the `#number`. Like the Linear adapter, all I/O stays on the
// host (ADR-0001): the credential lives in the transport and never crosses the
// sandbox boundary.
//
// The interface stays LCD: GitHub-unique shapes (task-list vs sub-issues,
// open/closed vs Linear workflow states, no native priority or blocks relation)
// are normalised to the port's vocabulary or dropped, never leaked out.
package github

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/beherd/agent-harness/internal/ticket"
	"github.com/beherd/agent-harness/internal/tracker"
)

// Client is the GitHub Issues adapter behind the host-side tracker port (ADR-0010).
var _ tracker.Tracker = (*Client)(nil)

// taskListRefRe matches a GitHub task-list checkbox line that tracks another
// issue: `- [ ] #45` / `* [x] #46 done`. Anchored to the checkbox so a bare `#99`
// in prose (not a task-list item) is never mistaken for a child. GitHub also
// renders `owner/repo#45` cross-repo refs, but the harness is repo-scoped, so
// only same-repo `#number` children are followed (LCD: cross-repo dropped).
var taskListRefRe = regexp.MustCompile(`(?m)^\s*[-*]\s+\[[ xX]\]\s+.*?#(\d+)`)

// taskListChildNumbers extracts the de-duplicated issue numbers referenced by a
// body's task-list items, in first-seen order. self is the parent's own number,
// skipped so a self-referential checkbox can't recurse.
func taskListChildNumbers(body string, self int) []int {
	seen := map[int]bool{self: true}
	var out []int
	for _, m := range taskListRefRe.FindAllStringSubmatch(body, -1) {
		n, err := strconv.Atoi(m[1])
		if err != nil || seen[n] {
			continue
		}
		seen[n] = true
		out = append(out, n)
	}
	return out
}

// Transport issues one GitHub REST request and returns the response body as raw
// JSON. It holds the host-only credential and the api.github.com base; the Client
// logic is exercised with a fake transport, so this stays thin and out of the
// unit suite (mirroring the Linear adapter's GraphQL transport seam).
type Transport func(method, path string, body any) (json.RawMessage, error)

// Options is the non-secret selection surface the adapter keys queue + claim +
// findings off — the GitHub analogue of the Linear adapter's hardcoded label
// constants, sourced from `.agent-harness/config` so a Consumer can rename them.
type Options struct {
	// Ready is the human-applied blast-radius gate: only issues carrying it are
	// eligible for auto-selection (the GitHub twin of ready-for-agent).
	Ready string
	// Blocked marks an issue a human flagged as blocked; never auto-selected.
	Blocked string
	// InProgress models the claim: GitHub issues have no workflow states, so the
	// harness expresses "In Progress" as this label (added on claim, removed on
	// release) — the label "column" transition that is the load-bearing claim
	// mechanism, since GitHub's issues list can require a label but not exclude one.
	InProgress string
	// Findings scopes filing + dedup to harness-surfaced findings, so dedup never
	// trips over unrelated repo issues.
	Findings string
	// Assignee is the optional bot login the harness assigns on claim and clears on
	// release, the "+ assignee" half of the claim semantics. Empty skips assignment
	// entirely — the label transition alone still removes a claimed ticket from the
	// queue and surfaces it to the reaper.
	Assignee string
}

// Client is the GitHub Issues adapter behind the host-side tracker port.
type Client struct {
	transport Transport
	owner     string
	repo      string
	opts      Options
}

// NewClient builds a Client bound to one owner/repo with the given selection
// surface.
func NewClient(t Transport, owner, repo string, opts Options) *Client {
	return &Client{transport: t, owner: owner, repo: repo, opts: opts}
}

// slug is the "owner/repo" identity used as the ticket's TeamID (findings are
// filed back into the same repo) and in narration.
func (c *Client) slug() string {
	return c.owner + "/" + c.repo
}

// issuesPath builds the repo-scoped issues REST path, optionally for one issue
// number (n == "" for the collection endpoint).
func (c *Client) issuesPath(n string) string {
	base := fmt.Sprintf("/repos/%s/%s/issues", c.owner, c.repo)
	if n == "" {
		return base
	}
	return base + "/" + n
}

// keyToNumber strips a leading '#' from a Key, yielding the bare issue number
// string GitHub's REST paths use.
func keyToNumber(key string) string {
	return strings.TrimPrefix(strings.TrimSpace(key), "#")
}

// numberToKey renders a bare issue number as the canonical `#number` Key.
func numberToKey(n int) string {
	return fmt.Sprintf("#%d", n)
}

// issue is the subset of a GitHub issue the adapter reads.
type issue struct {
	Number  int    `json:"number"`
	Title   string `json:"title"`
	Body    string `json:"body"`
	HTMLURL string `json:"html_url"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	// PullRequest is present only when this "issue" is actually a pull request
	// (GitHub returns PRs from the issues API); the adapter filters those out.
	PullRequest *struct {
		URL string `json:"url"`
	} `json:"pull_request"`
}

// FetchTicket resolves a #number Key to the harness ticket shape via one issues
// GET. GitHub has no native priority, so Priority is left empty.
func (c *Client) FetchTicket(key string) (ticket.Ticket, error) {
	data, err := c.transport("GET", c.issuesPath(keyToNumber(key)), nil)
	if err != nil {
		return ticket.Ticket{}, err
	}
	var iss issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return ticket.Ticket{}, err
	}
	t := ticket.Ticket{
		Identifier:  numberToKey(iss.Number),
		Title:       iss.Title,
		Description: iss.Body,
		URL:         iss.HTMLURL,
		TeamID:      c.slug(),
	}
	// Follow task-list children so an umbrella issue's real work is inlined into
	// the prompt host-side — the sandbox never reaches GitHub (ADR-0002/BEH-619).
	for _, n := range taskListChildNumbers(iss.Body, iss.Number) {
		child, err := c.fetchChild(n)
		if err != nil {
			return ticket.Ticket{}, err
		}
		t.SubIssues = append(t.SubIssues, child)
	}
	return t, nil
}

// fetchChild resolves one task-list child issue to a SubIssue.
func (c *Client) fetchChild(n int) (ticket.SubIssue, error) {
	data, err := c.transport("GET", c.issuesPath(strconv.Itoa(n)), nil)
	if err != nil {
		return ticket.SubIssue{}, err
	}
	var iss issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return ticket.SubIssue{}, err
	}
	return ticket.SubIssue{
		Identifier:  numberToKey(iss.Number),
		Title:       iss.Title,
		Description: iss.Body,
	}, nil
}
