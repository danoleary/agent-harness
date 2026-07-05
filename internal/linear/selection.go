package linear

import (
	"encoding/json"

	"github.com/beherd/agent-harness/internal/ticket"
)

// harnessTeamKey scopes ticket selection to the BeHerd backlog. The harness is
// single-team (the agent-harness label UUID above is BeHerd's too), and selection
// has no ticket to source a team UUID from, so the stable team key is the natural
// scope. Filtered in GraphQL via team.key.
const harnessTeamKey = "BEH"

// agentReadyLabel is the human-in-the-loop blast-radius gate: a person decides
// WHAT runs unattended by applying it; the harness decides HOW. Only labelled
// tickets are eligible for auto-selection (DESIGN.md "Ticket selection").
const agentReadyLabel = "ready-for-agent"

// blockedLabel marks a ticket a human has flagged as blocked; it is never
// auto-selected even if nothing else blocks it.
const blockedLabel = "Blocked"

// selectNextQuery lists the team's unassigned, Todo-type (unstarted), ready-for-agent
// issues with the fields the eligibility predicate + ordering need: labels (to
// re-check ready-for-agent and screen the Blocked label) and the inverse "blocks"
// relations (who blocks this issue, and whether that blocker is still open). The
// cheap, clean filter fields — team/type/assignee/ready-for-agent — scope the fetch so
// the first:250 page is the small human-gated working set; the Blocked label and
// the blocking-relation traversal are applied in Go (eligible) so one fixture still
// exercises the whole predicate.
const selectNextQuery = `
	query SelectNext($filter: IssueFilter!) {
		issues(filter: $filter, first: 250) {
			nodes {
				identifier
				title
				description
				url
				priorityLabel
				priority
				sortOrder
				createdAt
				team {
					id
				}
				labels {
					nodes {
						name
					}
				}
				inverseRelations {
					nodes {
						type
						issue {
							state {
								type
							}
						}
					}
				}
				children {
					nodes {
						state {
							type
						}
					}
				}
			}
		}
	}
`

// selectedIssue is one candidate issue parsed from selectNextQuery, carrying the
// fields the eligibility predicate and ordering read.
type selectedIssue struct {
	Identifier    string  `json:"identifier"`
	Title         string  `json:"title"`
	Description   *string `json:"description"`
	URL           *string `json:"url"`
	PriorityLabel *string `json:"priorityLabel"`
	Priority      int     `json:"priority"`
	SortOrder     float64 `json:"sortOrder"`
	CreatedAt     string  `json:"createdAt"`
	Team          *struct {
		ID string `json:"id"`
	} `json:"team"`
	Labels struct {
		Nodes []struct {
			Name string `json:"name"`
		} `json:"nodes"`
	} `json:"labels"`
	InverseRelations struct {
		Nodes []struct {
			Type  string `json:"type"`
			Issue *struct {
				State *struct {
					Type string `json:"type"`
				} `json:"state"`
			} `json:"issue"`
		} `json:"nodes"`
	} `json:"inverseRelations"`
	Children struct {
		Nodes []struct {
			State *struct {
				Type string `json:"type"`
			} `json:"state"`
		} `json:"nodes"`
	} `json:"children"`
}

// SelectNextTicket resolves the top-of-queue eligible ticket from the BeHerd
// backlog (DESIGN.md "Ticket selection"): Todo + unassigned + ready-for-agent +
// not-blocked, ordered by priority then board sort order then createdAt. It is a
// PURE READ — claim-on-select (ADR-0003) is the caller composing it with
// MoveToInProgress, so dry-run can resolve a ticket without mutating Linear.
// ok=false means the queue is empty.
func (c *Client) SelectNextTicket() (ticket.Ticket, bool, error) {
	// ready-for-agent is filtered in GraphQL (not only in eligible) so the first:250
	// page is the small human-gated working set, never the whole unassigned-Todo
	// backlog — otherwise a higher-priority ready-for-agent ticket beyond the 250th
	// node would be silently skipped, since ordering is applied client-side over
	// the fetched page. eligible() still re-checks the label as defensive depth.
	filter := map[string]any{
		"team":     map[string]any{"key": map[string]any{"eq": harnessTeamKey}},
		"state":    map[string]any{"type": map[string]any{"eq": "unstarted"}},
		"assignee": map[string]any{"null": true},
		"labels":   map[string]any{"name": map[string]any{"eq": agentReadyLabel}},
	}
	data, err := c.transport(selectNextQuery, map[string]any{"filter": filter})
	if err != nil {
		return ticket.Ticket{}, false, err
	}

	var resp struct {
		Issues struct {
			Nodes []selectedIssue `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return ticket.Ticket{}, false, err
	}

	best, ok := topEligible(resp.Issues.Nodes)
	if !ok {
		return ticket.Ticket{}, false, nil
	}
	return toTicket(best), true, nil
}

// toTicket projects a selected issue onto the harness ticket shape.
func toTicket(s selectedIssue) ticket.Ticket {
	t := ticket.Ticket{Identifier: s.Identifier, Title: s.Title}
	if s.Description != nil {
		t.Description = *s.Description
	}
	if s.URL != nil {
		t.URL = *s.URL
	}
	if s.PriorityLabel != nil {
		t.Priority = *s.PriorityLabel
	}
	if s.Team != nil {
		t.TeamID = s.Team.ID
	}
	return t
}

// topEligible returns the highest-priority eligible issue, or ok=false when none
// is eligible.
func topEligible(issues []selectedIssue) (selectedIssue, bool) {
	var best selectedIssue
	found := false
	for _, iss := range issues {
		if !eligible(iss) {
			continue
		}
		if !found || less(iss, best) {
			best = iss
			found = true
		}
	}
	return best, found
}

// eligible reports whether an issue may be auto-worked: it carries ready-for-agent, is
// not labelled Blocked, and has no still-OPEN issue blocking it (DESIGN.md "Ticket
// selection"). Linear models "A blocks B" as an IssueRelation with type "blocks",
// issue=A (the blocker), relatedIssue=B; from B that relation appears in
// inverseRelations, so an open blocker of B is an inverse "blocks" relation whose
// source issue is not yet completed/canceled.
func eligible(s selectedIssue) bool {
	if !hasLabel(s, agentReadyLabel) {
		return false
	}
	if hasLabel(s, blockedLabel) {
		return false
	}
	// An umbrella/tracker issue with an OPEN child defers its real work to that
	// child; claiming it would strand it In Progress producing no branch/PR (the
	// BEH-497 incident). Skip it while any child is still open.
	for _, ch := range s.Children.Nodes {
		if ch.State == nil {
			continue
		}
		if !isClosedStateType(ch.State.Type) {
			return false
		}
	}
	for _, r := range s.InverseRelations.Nodes {
		if r.Type != "blocks" {
			continue
		}
		if r.Issue == nil || r.Issue.State == nil {
			continue
		}
		if !isClosedStateType(r.Issue.State.Type) {
			return false
		}
	}
	return true
}

// hasLabel reports whether the issue carries the named label.
func hasLabel(s selectedIssue, name string) bool {
	for _, l := range s.Labels.Nodes {
		if l.Name == name {
			return true
		}
	}
	return false
}

// less orders eligible candidates by the DESIGN.md ordering: priority
// (Urgent→High→Medium→Low→None), then board sort order ascending, then createdAt
// ascending. Returns true if a should be taken before b.
func less(a, b selectedIssue) bool {
	ra, rb := priorityRank(a.Priority), priorityRank(b.Priority)
	if ra != rb {
		return ra < rb
	}
	if a.SortOrder != b.SortOrder {
		return a.SortOrder < b.SortOrder
	}
	return a.CreatedAt < b.CreatedAt
}

// priorityRank maps Linear's numeric priority to a sort rank where Urgent is
// highest and No-priority is lowest. Linear encodes 0=None, 1=Urgent, 2=High,
// 3=Medium, 4=Low — so None (0) must sort AFTER Low (4) despite its smaller
// number; 1..4 are already Urgent..Low in ascending order.
func priorityRank(p int) int {
	if p == 0 {
		return 5
	}
	return p
}
