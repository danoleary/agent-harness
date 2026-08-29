// Package linear is the host-only Linear GraphQL client (ADR-0001): fetch/claim
// tickets and file harness-improvement findings.
package linear

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strconv"
	"strings"

	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/ticket"
	"github.com/danoleary/agent-harness/internal/tracker"
)

// Client is the Linear adapter behind the host-side tracker port (ADR-0010).
var _ tracker.Tracker = (*Client)(nil)

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

// occurrenceRe matches the occurrence-count marker a recurrence bump writes into a
// filed finding's body: `<!-- occurrences: <n> -->`. Like the finding-key marker
// it's machine-readable and invisible in rendered Markdown, so the count survives
// round-trips without cluttering the human-facing issue.
var occurrenceRe = regexp.MustCompile(`<!--\s*occurrences:\s*(\d+)\s*-->`)

// occurrenceMarker renders the body marker for an occurrence count.
func occurrenceMarker(n int) string {
	return fmt.Sprintf("<!-- occurrences: %d -->", n)
}

// extractOccurrences reads the occurrence count from a finding body, defaulting
// to 1 (the original filing) when the body carries no marker yet.
func extractOccurrences(body string) int {
	m := occurrenceRe.FindStringSubmatch(body)
	if m == nil {
		return 1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// withOccurrences returns body with its occurrence marker set to n — replacing an
// existing marker in place, or appending one when the body has none.
func withOccurrences(body string, n int) string {
	if occurrenceRe.MatchString(body) {
		return occurrenceRe.ReplaceAllString(body, occurrenceMarker(n))
	}
	if strings.TrimSpace(body) == "" {
		return occurrenceMarker(n)
	}
	return body + "\n\n" + occurrenceMarker(n)
}

// Transport sends a GraphQL operation and returns the `data` payload as raw JSON
// (the transport handles auth + transport-level errors).
type Transport func(query string, variables map[string]any) (json.RawMessage, error)

// agentHarnessLabelID is the BeHerd "agent-harness" label (team BeHerd). Every
// finding the harness files is, by construction, about the harness/environment
// itself (ADR-0001), so it always belongs under this label. The create API
// (IssueCreateInput.labelIds) takes a UUID, not a name — resolving the name at
// file-time would add a GraphQL round-trip per finding plus a failure mode that
// could silently drop the label — so we reference the id directly (BEH-409).
const agentHarnessLabelID = "788a5654-a4b3-4ac2-8483-a4d50408ebc0"

// Client wraps a Transport with the harness's Linear operations.
type Client struct {
	transport Transport
}

// NewClient builds a Client over the given transport.
func NewClient(t Transport) *Client {
	return &Client{transport: t}
}

// fetchTicketQuery pulls the ticket plus its child sub-issues (BEH-619). An
// umbrella/batch ticket defers its real work to children; the sandbox can't reach
// Linear (ADR-0002), so the host fetches each child's title + body here and the
// prompt inlines them. The children list is bounded (first: 50) — far beyond any
// real umbrella's fan-out, but a hard cap so a pathological parent can't balloon
// the prompt.
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
			children(first: 50) {
				nodes {
					identifier
					title
					description
				}
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

// occurrenceContextQuery fetches the fields a recurrence bump needs: the issue's
// node id (commentCreate/issueUpdate key off the UUID, not the human identifier)
// and its current description (to read + rewrite the occurrence marker).
const occurrenceContextQuery = `
	query OccurrenceContext($id: String!) {
		issue(id: $id) {
			id
			description
		}
	}
`

// updateDescriptionMutation rewrites an issue's body (used to bump the occurrence
// marker in place).
const updateDescriptionMutation = `
	mutation UpdateDescription($id: String!, $description: String!) {
		issueUpdate(id: $id, input: { description: $description }) {
			success
		}
	}
`

// addCommentMutation appends a comment to an issue (the recurrence breadcrumb).
const addCommentMutation = `
	mutation AddComment($input: CommentCreateInput!) {
		commentCreate(input: $input) {
			success
		}
	}
`

// issueIDQuery resolves an issue's node UUID from its human identifier, which
// commentCreate keys off (its CommentCreateInput.issueId is the UUID, not the
// BEH-NNN identifier).
const issueIDQuery = `
	query IssueID($id: String!) {
		issue(id: $id) {
			id
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
			Children *struct {
				Nodes []struct {
					Identifier  string `json:"identifier"`
					Title       string `json:"title"`
					Description string `json:"description"`
				} `json:"nodes"`
			} `json:"children"`
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
	if resp.Issue.Children != nil {
		for _, c := range resp.Issue.Children.Nodes {
			t.SubIssues = append(t.SubIssues, ticket.SubIssue{
				Identifier:  c.Identifier,
				Title:       c.Title,
				Description: c.Description,
			})
		}
	}
	return t, nil
}

// issueState is one Linear workflow state (id/name/type) as returned by
// fetchIssueStatesQuery.
type issueState struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Type string `json:"type"`
}

// pickState returns the id of the team state to move to: the one matching both
// type and canonical name, falling back to the first state of that type (teams
// may rename "In Progress"/"Todo" but the type is stable). "" means no state of
// the requested type exists.
func pickState(states []issueState, typ, name string) string {
	for _, s := range states {
		if s.Type == typ && s.Name == name {
			return s.ID
		}
	}
	for _, s := range states {
		if s.Type == typ {
			return s.ID
		}
	}
	return ""
}

// moveToState fetches the issue's team states, picks a target via pick, and moves
// the issue there. what names the target for the error message. Shared by
// MoveToInProgress and ReleaseToTodo so the fetch/parse/mutate plumbing lives once.
func (c *Client) moveToState(identifier, what string, pick func([]issueState) string) error {
	data, err := c.transport(fetchIssueStatesQuery, map[string]any{"id": identifier})
	if err != nil {
		return err
	}

	var resp struct {
		Issue *struct {
			ID   string `json:"id"`
			Team struct {
				States struct {
					Nodes []issueState `json:"nodes"`
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

	target := pick(resp.Issue.Team.States.Nodes)
	if target == "" {
		return fmt.Errorf("no %s state for %s", what, identifier)
	}

	_, err = c.transport(moveIssueMutation, map[string]any{"id": resp.Issue.ID, "stateId": target})
	return err
}

// MoveToInProgress moves a ticket into its team's started "In Progress" state.
func (c *Client) MoveToInProgress(identifier string) error {
	return c.moveToState(identifier, `started "In Progress"`, func(s []issueState) string {
		return pickState(s, "started", "In Progress")
	})
}

// ReleaseToTodo moves a ticket back to its team's unstarted "Todo" state, undoing
// an In-Progress claim that yielded nothing — used when an implementation session
// crashed environmentally before creating a worktree or commit (BEH-543), so a
// later run re-grabs the ticket rather than finding it stranded In Progress.
func (c *Client) ReleaseToTodo(identifier string) error {
	return c.moveToState(identifier, `unstarted "Todo"`, func(s []issueState) string {
		return pickState(s, "unstarted", "Todo")
	})
}

// MoveToCanceled moves a ticket into its team's terminal "canceled" state — the
// host-side action that consumes a recommend-close verdict (BEH-682): a run that
// found the branch makes zero net change against origin/main is a superseded /
// duplicate ticket, so it is closed rather than left In Progress. A canceled ticket
// is neither unstarted (out of the --next selection pool) nor In Progress (out of
// the stale-claim reaper pool), so it can never re-enter the pipeline — unlike the
// old "keep In Progress" disposition, which the reaper released back to Todo past
// the claim TTL, re-looping to the same conclusion forever.
func (c *Client) MoveToCanceled(identifier string) error {
	return c.moveToState(identifier, `canceled "Canceled"`, func(s []issueState) string {
		return pickState(s, "canceled", "Canceled")
	})
}

// FileFinding files a harness-improvement finding as a new issue referencing the
// worked ticket. It tags the issue with the agent-harness label (so dedup search
// can find it) and, when the finding carries an explicit dedup key, embeds the
// `<!-- finding-key: … -->` marker so a later run dedups on an exact-key lookup.
// Label resolution is best-effort: if it fails, the issue is still filed (only
// future dedup of this issue is weakened — never a crash).
func (c *Client) FileFinding(f findings.Finding, opts tracker.FileFindingOptions) (tracker.CreatedIssue, error) {
	kindLine := ""
	if f.Kind != "" {
		kindLine = "**Kind:** " + f.Kind + "\n\n"
	}
	description := fmt.Sprintf(
		"%s%s\n\n_Surfaced during %s by the agent harness._",
		kindLine, f.Body, opts.RelatedKey,
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
		return tracker.CreatedIssue{}, err
	}

	var resp struct {
		IssueCreate struct {
			Success bool                  `json:"success"`
			Issue   *tracker.CreatedIssue `json:"issue"`
		} `json:"issueCreate"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return tracker.CreatedIssue{}, err
	}
	if !resp.IssueCreate.Success || resp.IssueCreate.Issue == nil {
		return tracker.CreatedIssue{}, fmt.Errorf("failed to file finding: %s", f.Title)
	}
	return *resp.IssueCreate.Issue, nil
}

// AddComment posts a comment to an issue identified by its human identifier
// (e.g. "BEH-581"). It resolves the issue's node UUID first, then creates the
// comment. Used by the pre-push conflict path (BEH-581) to leave an autonomous
// breadcrumb when a gate-green, reviewed branch can't be auto-rebased — so the
// stranded work surfaces on the ticket instead of sitting silent in a worktree.
// The caller treats a failure best-effort (a missing breadcrumb must never sink
// the run); a failed id-resolve is surfaced rather than silently swallowed.
func (c *Client) AddComment(identifier, body string) error {
	data, err := c.transport(issueIDQuery, map[string]any{"id": identifier})
	if err != nil {
		return err
	}
	var resp struct {
		Issue *struct {
			ID string `json:"id"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(data, &resp); err != nil {
		return err
	}
	if resp.Issue == nil {
		return fmt.Errorf("Linear issue not found: %s", identifier)
	}
	_, err = c.transport(addCommentMutation, map[string]any{
		"input": map[string]any{"issueId": resp.Issue.ID, "body": body},
	})
	return err
}

// SearchFindings lists the team's already-filed harness findings (scoped by the
// agent-harness label), recovering each one's dedup key from its body marker and
// whether it is closed. The caller decides the open/closed dedup policy.
func (c *Client) SearchFindings(teamID string) ([]tracker.ExistingFinding, error) {
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

	out := make([]tracker.ExistingFinding, 0, len(resp.Issues.Nodes))
	for _, n := range resp.Issues.Nodes {
		out = append(out, tracker.ExistingFinding{
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

// RecordOccurrence records that an already-tracked finding recurred during
// relatedIdentifier's pipeline (BEH-573): it bumps the issue's occurrence marker
// (`<!-- occurrences: N -->`, defaulting to 1 for the original filing) and
// appends a breadcrumb comment, returning the new count. Turning N reworded
// duplicates into one issue carrying N occurrences is both less backlog noise and
// a sharper prioritisation signal. The bump and the comment are two mutations: if
// the comment fails after the bump landed the count is still advanced (the
// breadcrumb is the lossy half, not the count), and either failure surfaces as an
// error the caller degrades on — it never files a duplicate instead.
func (c *Client) RecordOccurrence(identifier, relatedIdentifier string) (int, error) {
	data, err := c.transport(occurrenceContextQuery, map[string]any{"id": identifier})
	if err != nil {
		return 0, err
	}
	var ctx struct {
		Issue *struct {
			ID          string `json:"id"`
			Description string `json:"description"`
		} `json:"issue"`
	}
	if err := json.Unmarshal(data, &ctx); err != nil {
		return 0, err
	}
	if ctx.Issue == nil {
		return 0, fmt.Errorf("Linear issue not found: %s", identifier)
	}

	next := extractOccurrences(ctx.Issue.Description) + 1
	if _, err := c.transport(updateDescriptionMutation, map[string]any{
		"id":          ctx.Issue.ID,
		"description": withOccurrences(ctx.Issue.Description, next),
	}); err != nil {
		return 0, err
	}

	comment := fmt.Sprintf("Recurred in %s pipeline (occurrence %d).", relatedIdentifier, next)
	if _, err := c.transport(addCommentMutation, map[string]any{
		"input": map[string]any{"issueId": ctx.Issue.ID, "body": comment},
	}); err != nil {
		return next, err
	}
	return next, nil
}
