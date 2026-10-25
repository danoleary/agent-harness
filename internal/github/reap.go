package github

import (
	"encoding/json"
	"net/url"
	"strconv"
	"time"

	"github.com/danoleary/agent-harness/internal/tracker"
)

// claimsQuery builds the list-issues query for the reaper read: the repo's open,
// in-progress-labelled issues — the agent-claimed set, since only the harness
// applies the in-progress label.
func (c *Client) claimsQuery() string {
	v := url.Values{}
	v.Set("labels", c.opts.InProgress)
	v.Set("state", "open")
	v.Set("per_page", "100")
	return c.issuesPath("") + "?" + v.Encode()
}

// timelineEvent is the subset of a GitHub issue timeline event the reaper reads:
// a `labeled` event (to time the claim) and a `cross-referenced` event whose
// source is a pull request (to detect a linked PR).
type timelineEvent struct {
	Event     string `json:"event"`
	CreatedAt string `json:"created_at"`
	Label     *struct {
		Name string `json:"name"`
	} `json:"label"`
	Source *struct {
		Issue *struct {
			PullRequest *struct {
				URL      string `json:"url"`
				MergedAt string `json:"merged_at"`
			} `json:"pull_request"`
		} `json:"issue"`
	} `json:"source"`
}

// ListInProgressClaims returns the agent-claimed In Progress tickets (BEH-677):
// the reaper's read of the set it may release back to Todo. For each, it reads the
// issue timeline to time the claim (the latest in-progress `labeled` event) and to
// detect a linked PR (a `cross-referenced` event whose source is an unmerged PR,
// dated no earlier than the claim). GitHub issues carry no claim timestamp, so the timeline
// is the only signal. The caller applies the grace TTL and branch check before
// reaping.
func (c *Client) ListInProgressClaims() ([]tracker.InProgressClaim, error) {
	data, err := c.transport("GET", c.claimsQuery(), nil)
	if err != nil {
		return nil, err
	}
	var issues []issue
	if err := json.Unmarshal(data, &issues); err != nil {
		return nil, err
	}

	out := make([]tracker.InProgressClaim, 0, len(issues))
	for _, iss := range issues {
		// The issues endpoint returns PRs too, and the PR loop holds a PR with the
		// same label (ADR-0019): that is a PR lease, not a ticket claim.
		if iss.PullRequest != nil {
			continue
		}
		claim := tracker.InProgressClaim{Identifier: numberToKey(iss.Number)}
		started, hasPR, err := c.timelineSignals(iss.Number)
		if err != nil {
			return nil, err
		}
		claim.StartedAt = started
		claim.HasLinkedPR = hasPR
		out = append(out, claim)
	}
	return out, nil
}

// timelineSignals reads issue n's timeline for the two reaper signals: the claim
// time (the latest in-progress `labeled` event; the zero time when none is found,
// which reads as "claimed at the epoch" — safely past any TTL, so an untimeable
// claim is still reapable rather than immortal) and whether a PR cross-referenced
// the issue during that claim. A cross-reference older than the claim — an earlier
// PR that merely mentioned the issue, or one from a previous claim — is not evidence
// of this claim's work, so it does not shield a dead claim from the reaper (#87).
// Neither does a merged PR: the harness's own PR says "Resolves #n", so merging it
// closes the issue and drops it from the open claimed set; a merged PR on an issue
// still open is some other PR that merely mentioned it (#74 was mentioned mid-claim
// by docs PR #84). A cross-reference whose time cannot be read still counts,
// failing safe toward not-reaping.
func (c *Client) timelineSignals(n int) (time.Time, bool, error) {
	data, err := c.transport("GET", c.issuesPath(strconv.Itoa(n))+"/timeline?per_page=100", nil)
	if err != nil {
		return time.Time{}, false, err
	}
	var events []timelineEvent
	if err := json.Unmarshal(data, &events); err != nil {
		return time.Time{}, false, err
	}

	var started time.Time
	var prRefs []time.Time // zero when the cross-reference's time cannot be read
	for _, e := range events {
		switch e.Event {
		case "labeled":
			if e.Label != nil && e.Label.Name == c.opts.InProgress {
				if t, perr := time.Parse(time.RFC3339, e.CreatedAt); perr == nil && t.After(started) {
					started = t
				}
			}
		case "cross-referenced":
			if e.Source != nil && e.Source.Issue != nil && e.Source.Issue.PullRequest != nil &&
				e.Source.Issue.PullRequest.MergedAt == "" {
				t, _ := time.Parse(time.RFC3339, e.CreatedAt)
				prRefs = append(prRefs, t)
			}
		}
	}
	// The timeline is chronological, but the claim time is only known once every
	// event is read, so PR cross-references are judged against it afterwards.
	for _, t := range prRefs {
		if t.IsZero() || !t.Before(started) {
			return started, true, nil
		}
	}
	return started, false, nil
}
