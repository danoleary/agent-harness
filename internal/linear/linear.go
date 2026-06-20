// Package linear is the host-only Linear GraphQL client (ADR-0001): fetch/claim
// tickets and file harness-improvement findings.
package linear

import (
	"encoding/json"
	"fmt"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/ticket"
)

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
// worked ticket.
func (c *Client) FileFinding(f findings.Finding, opts FileFindingOptions) (CreatedIssue, error) {
	kindLine := ""
	if f.Kind != "" {
		kindLine = "**Kind:** " + f.Kind + "\n\n"
	}
	description := fmt.Sprintf(
		"%s%s\n\n_Surfaced during %s by the agent harness._",
		kindLine, f.Body, opts.RelatedIdentifier,
	)

	// Always carry the agent-harness label, added alongside (not in place of)
	// any future per-finding labels (BEH-409).
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
