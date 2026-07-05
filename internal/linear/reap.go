package linear

import (
	"encoding/json"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/tracker"
)

// inProgressClaimsQuery lists the team's agent-claimed In Progress tickets with the
// fields the stale-claim reaper needs: startedAt (how long the claim has been held)
// and attachments (to detect a linked PR). Scoped in GraphQL to the small, precise
// candidate set — team BEH, the started state type, unassigned, ready-for-agent — so
// the reaper never even sees a human's In Progress work (which carries an assignee)
// or a non-agent ticket.
const inProgressClaimsQuery = `
	query InProgressClaims($filter: IssueFilter!) {
		issues(filter: $filter, first: 250) {
			nodes {
				identifier
				startedAt
				attachments {
					nodes {
						url
					}
				}
			}
		}
	}
`

// ListInProgressClaims returns the agent-claimed In Progress tickets (BEH-677): the
// reaper's read of the set it may release back to Todo. It reports each claim's
// startedAt and whether a PR is linked; the caller applies the grace TTL and the
// separate branch check before reaping. Scoped to unassigned ready-for-agent started
// tickets so a human's In Progress work is never a candidate.
func (c *Client) ListInProgressClaims() ([]tracker.InProgressClaim, error) {
	filter := map[string]any{
		"team":     map[string]any{"key": map[string]any{"eq": harnessTeamKey}},
		"state":    map[string]any{"type": map[string]any{"eq": "started"}},
		"assignee": map[string]any{"null": true},
		"labels":   map[string]any{"name": map[string]any{"eq": agentReadyLabel}},
	}
	data, err := c.transport(inProgressClaimsQuery, map[string]any{"filter": filter})
	if err != nil {
		return nil, err
	}

	var resp struct {
		Issues struct {
			Nodes []struct {
				Identifier  string  `json:"identifier"`
				StartedAt   *string `json:"startedAt"`
				Attachments struct {
					Nodes []struct {
						URL string `json:"url"`
					} `json:"nodes"`
				} `json:"attachments"`
			} `json:"nodes"`
		} `json:"issues"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return nil, err
	}

	out := make([]tracker.InProgressClaim, 0, len(resp.Issues.Nodes))
	for _, n := range resp.Issues.Nodes {
		claim := tracker.InProgressClaim{Identifier: n.Identifier}
		if n.StartedAt != nil {
			// A malformed startedAt leaves the zero time, which reads as "claimed at the
			// epoch" — safely past any TTL, so a claim whose timestamp we can't parse is
			// still reapable rather than silently immortal.
			if t, perr := time.Parse(time.RFC3339, *n.StartedAt); perr == nil {
				claim.StartedAt = t
			}
		}
		for _, a := range n.Attachments.Nodes {
			if isPullRequestURL(a.URL) {
				claim.HasLinkedPR = true
				break
			}
		}
		out = append(out, claim)
	}
	return out, nil
}

// isPullRequestURL reports whether an attachment URL is a GitHub pull request link
// (`.../pull/<n>`). Linear auto-creates such an attachment when a PR references the
// issue, so it is the reliable "this claim shipped something" signal.
func isPullRequestURL(url string) bool {
	return strings.Contains(url, "/pull/")
}
