// Package linear is the host-only Linear GraphQL client (ADR-0001): fetch/claim
// tickets and file harness-improvement findings.
package linear

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/ticket"
)

// findingKeyRe matches the machine-readable dedup marker embedded in a filed
// finding's body: `<!-- finding-key: <key> -->`. The harness writes it on filing
// and reads it back on search, so dedup is an exact-key lookup, not fuzzy text.
var findingKeyRe = regexp.MustCompile(`<!--\s*finding-key:\s*(\S+)\s*-->`)

// ExtractFindingKey pulls the dedup key out of a filed finding's body, or "" when
// the body carries no marker.
func ExtractFindingKey(body string) string {
	m := findingKeyRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// findingKeyMarker renders the body marker for a dedup key.
func findingKeyMarker(key string) string {
	return "<!-- finding-key: " + key + " -->"
}

// Transport sends a GraphQL operation and returns the `data` payload as raw JSON
// (the transport handles auth + transport-level errors).
type Transport func(query string, variables map[string]any) (json.RawMessage, error)

// FileFindingOptions parameterises filing one finding.
type FileFindingOptions struct {
	// TeamID is the UUID of the team to create the finding in (BeHerd).
	TeamID string
	// RelatedIdentifier is the human identifier of the ticket whose session
	// surfaced this (e.g. "BEH-362").
	RelatedIdentifier string
}

// agentHarnessLabelID is the BeHerd "agent-harness" label (team BeHerd). Every
// finding the harness files is, by construction, about the harness/environment
// itself (ADR-0001), so it always belongs under this label. The create API
// (IssueCreateInput.labelIds) takes a UUID, not a name — resolving the name at
// file-time would add a GraphQL round-trip per finding plus a failure mode that
// could silently drop the label — so we reference the id directly (BEH-409).
const agentHarnessLabelID = "788a5654-a4b3-4ac2-8483-a4d50408ebc0"

// CreatedIssue is the result of filing a finding.
type CreatedIssue struct {
	Identifier string `json:"identifier"`
	URL        string `json:"url"`
}

// ExistingFinding is an already-filed harness finding, returned by SearchFindings
// so filing can skip duplicates. Key is the dedup fingerprint recovered from the
// issue body's `<!-- finding-key: … -->` marker (empty when the issue carries
// none). Closed is true when the issue is in a completed/canceled state — a
// closed match must NOT suppress a re-file, so a wontfix can't permanently mask a
// real regression.
type ExistingFinding struct {
	Identifier string
	Title      string
	Key        string
	Closed     bool
}

// Client wraps a Transport with the harness's Linear operations.
type Client struct {
	transport Transport
}

// NewClient builds a Client over the given transport.
func NewClient(t Transport) *Client {
	return &Client{transport: t}
}

const fetchTicketQuery = `
	query Ticket($id: String!) {
		issue(id: $id) {
			identifier
			title
			description
			url
			priorityLabel
			team {
				id
			}
		}
	}
`

const fetchIssueStatesQuery = `
	query IssueStates($id: String!) {
		issue(id: $id) {
			id
			team {
				states {
					nodes {
						id
						name
						type
					}
				}
			}
		}
	}
`

const moveIssueMutation = `
	mutation MoveIssue($id: String!, $stateId: String!) {
		issueUpdate(id: $id, input: { stateId: $stateId }) {
			success
		}
	}
`

const fileFindingMutation = `
	mutation FileFinding($input: IssueCreateInput!) {
		issueCreate(input: $input) {
			success
			issue {
				identifier
				url
			}
		}
	}
`

// findingsLabel scopes filing + dedup to harness-surfaced findings: filed issues
// carry it and SearchFindings filters on it, so dedup never trips over unrelated
// team issues.
const findingsLabel = "agent-harness"

// searchFindingsQuery lists this team's OPEN harness findings. The query excludes
// terminal (completed/canceled) states so closed findings don't consume the result
// window — as the harness backlog grows, dedup coverage would otherwise silently
// shrink. The caller still defensively skips any closed match it's handed (so the
// open/closed policy stays unit-testable with a fake). Scoped by the agent-harness
// label so it never returns unrelated issues.
const searchFindingsQuery = `
	query Findings($filter: IssueFilter!) {
		issues(filter: $filter, first: 250) {
			nodes {
				identifier
				title
				description
				state {
					type
				}
			}
		}
	}
`

// FetchTicket fetches a ticket by its human identifier (e.g. "BEH-362").
func (c *Client) FetchTicket(identifier string) (ticket.Ticket, error) {
	data, err := c.transport(fetchTicketQuery, map[string]any{"id": identifier})
	if err != nil {
		return ticket.Ticket{}, err
	}

	var resp struct {
		Issue *struct {
			Identifier    string  `json:"identifier"`
			Title         string  `json:"title"`
			Description   *string `json:"description"`
			URL           *string `json:"url"`
			PriorityLabel *string `json:"priorityLabel"`
			Team          *struct {
				ID string `json:"id"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return ticket.Ticket{}, err
	}
	if resp.Issue == nil {
		return ticket.Ticket{}, fmt.Errorf("Linear issue not found: %s", identifier)
	}

	t := ticket.Ticket{Identifier: resp.Issue.Identifier, Title: resp.Issue.Title}
	if resp.Issue.Description != nil {
		t.Description = *resp.Issue.Description
	}
	if resp.Issue.URL != nil {
		t.URL = *resp.Issue.URL
	}
	if resp.Issue.PriorityLabel != nil {
		t.Priority = *resp.Issue.PriorityLabel
	}
	if resp.Issue.Team != nil {
		t.TeamID = resp.Issue.Team.ID
	}
	return t, nil
}

// MoveToInProgress moves a ticket into its team's started "In Progress" state.
func (c *Client) MoveToInProgress(identifier string) error {
	data, err := c.transport(fetchIssueStatesQuery, map[string]any{"id": identifier})
	if err != nil {
		return err
	}

	var resp struct {
		Issue *struct {
			ID   string `json:"id"`
			Team struct {
				States struct {
					Nodes []struct {
						ID   string `json:"id"`
						Name string `json:"name"`
						Type string `json:"type"`
					} `json:"nodes"`
				} `json:"states"`
			} `json:"team"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return err
	}
	if resp.Issue == nil {
		return fmt.Errorf("Linear issue not found: %s", identifier)
	}

	states := resp.Issue.Team.States.Nodes
	target := ""
	for _, s := range states {
		if s.Type == "started" && s.Name == "In Progress" {
			target = s.ID
			break
		}
	}
	if target == "" {
		for _, s := range states {
			if s.Type == "started" {
				target = s.ID
				break
			}
		}
	}
	if target == "" {
		return fmt.Errorf("no started \"In Progress\" state for %s", identifier)
	}

	_, err = c.transport(moveIssueMutation, map[string]any{"id": resp.Issue.ID, "stateId": target})
	return err
}

// FileFinding files a harness-improvement finding as a new issue referencing the
// worked ticket. It tags the issue with the agent-harness label (so dedup search
// can find it) and, when the finding carries an explicit dedup key, embeds the
// `<!-- finding-key: … -->` marker so a later run dedups on an exact-key lookup.
// Label resolution is best-effort: if it fails, the issue is still filed (only
// future dedup of this issue is weakened — never a crash).
func (c *Client) FileFinding(f findings.Finding, opts FileFindingOptions) (CreatedIssue, error) {
	kindLine := ""
	if f.Kind != "" {
		kindLine = "**Kind:** " + f.Kind + "\n\n"
	}
	description := fmt.Sprintf(
		"%s%s\n\n_Surfaced during %s by the agent harness._",
		kindLine, f.Body, opts.RelatedIdentifier,
	)
	if key := strings.TrimSpace(f.Key); key != "" {
		description += "\n\n" + findingKeyMarker(key)
	}

	// Always carry the agent-harness label, added alongside (not in place of)
	// any future per-finding labels (BEH-409). Filing with this label is what lets
	// SearchFindings (BEH-410) find these issues again to dedup re-runs.
	labelIDs := append([]string{}, f.LabelIDs...)
	labelIDs = append(labelIDs, agentHarnessLabelID)

	data, err := c.transport(fileFindingMutation, map[string]any{
		"input": map[string]any{
			"teamId":      opts.TeamID,
			"title":       f.Title,
			"description": description,
			"labelIds":    labelIDs,
		},
	})
	if err != nil {
		return CreatedIssue{}, err
	}

	var resp struct {
		IssueCreate struct {
			Success bool          `json:"success"`
			Issue   *CreatedIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return CreatedIssue{}, err
	}
	if !resp.IssueCreate.Success || resp.IssueCreate.Issue == nil {
		return CreatedIssue{}, fmt.Errorf("failed to file finding: %s", f.Title)
	}
	return *resp.IssueCreate.Issue, nil
}

// SearchFindings lists the team's already-filed harness findings (scoped by the
// agent-harness label), recovering each one's dedup key from its body marker and
// whether it is closed. The caller decides the open/closed dedup policy.
func (c *Client) SearchFindings(teamID string) ([]ExistingFinding, error) {
	filter := map[string]any{
		"team":   map[string]any{"id": map[string]any{"eq": teamID}},
		"labels": map[string]any{"name": map[string]any{"eq": findingsLabel}},
		// Exclude terminal states so closed findings don't eat into the result
		// window; the caller still skips any closed match defensively.
		"state": map[string]any{"type": map[string]any{"nin": []string{"completed", "canceled"}}},
	}
	data, err := c.transport(searchFindingsQuery, map[string]any{"filter": filter})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Issues struct {
			Nodes []struct {
				Identifier  string `json:"identifier"`
				Title       string `json:"title"`
				Description string `json:"description"`
				State       struct {
					Type string `json:"type"`
				} `json:"state"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	out := make([]ExistingFinding, 0, len(resp.Issues.Nodes))
	for _, n := range resp.Issues.Nodes {
		out = append(out, ExistingFinding{
			Identifier: n.Identifier,
			Title:      n.Title,
			Key:        ExtractFindingKey(n.Description),
			Closed:     isClosedStateType(n.State.Type),
		})
	}
	return out, nil
}

// isClosedStateType reports whether a Linear workflow state type is terminal —
// completed or canceled. Dedup keys off open findings only, so a closed match
// must not suppress a re-file.
func isClosedStateType(stateType string) bool {
	return stateType == "completed" || stateType == "canceled"
}
