package jira

import (
	"encoding/json"
	"net/url"
	"strings"
	"time"

	"github.com/beherd/agent-harness/internal/tracker"
)

// jiraTimeLayout is Jira Cloud's timestamp format (milliseconds + a numeric zone
// with no colon), e.g. `2026-07-05T10:00:00.000+0000` — not RFC3339, which is why
// it needs an explicit layout.
const jiraTimeLayout = "2006-01-02T15:04:05.000-0700"

// claimIssue is the subset of a claimed issue the reaper reads: its key and the
// status-category change date used as the claim time.
type claimIssue struct {
	Key    string `json:"key"`
	Fields struct {
		StatusCategoryChangeDate string `json:"statuscategorychangedate"`
	} `json:"fields"`
}

// claimSearchResult is the JQL search response shape for the reaper read.
type claimSearchResult struct {
	Issues []claimIssue `json:"issues"`
}

// ListInProgressClaims returns the agent-claimed In Progress tickets (BEH-677):
// the reaper's read of the set it may release back to Todo. Claims are found via
// the configured in-progress JQL; each one's StartedAt is its status-category
// change date (Jira's timestamp for when it last entered In Progress), and
// HasLinkedPR is whether any remote link points at a pull request. The caller
// applies the grace TTL and branch check before reaping.
func (c *Client) ListInProgressClaims() ([]tracker.InProgressClaim, error) {
	data, err := c.transport("GET", searchPath(c.opts.InProgressJQL, "statuscategorychangedate"), nil)
	if err != nil {
		return nil, err
	}
	var res claimSearchResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}

	out := make([]tracker.InProgressClaim, 0, len(res.Issues))
	for _, iss := range res.Issues {
		claim := tracker.InProgressClaim{Identifier: iss.Key}
		// A malformed/absent date parses to the zero time, which reads as "claimed
		// at the epoch" — safely past any TTL, so an untimeable claim stays reapable
		// rather than immortal (mirrors the GitHub adapter's zero-time fallback).
		if t, perr := time.Parse(jiraTimeLayout, iss.Fields.StatusCategoryChangeDate); perr == nil {
			claim.StartedAt = t
		}
		hasPR, err := c.hasLinkedPR(iss.Key)
		if err != nil {
			return nil, err
		}
		claim.HasLinkedPR = hasPR
		out = append(out, claim)
	}
	return out, nil
}

// remoteLink is the subset of a Jira remote issue link the reaper reads: the
// linked object's URL.
type remoteLink struct {
	Object struct {
		URL string `json:"url"`
	} `json:"object"`
}

// hasLinkedPR reports whether any of an issue's remote links points at a pull
// request (a `/pull/` URL) — the Jira analogue of the GitHub adapter's
// cross-referenced-PR timeline signal. Jira has no native dev-panel field the
// port can read without the instance-specific dev-status API, so remote links are
// the LCD signal; an instance with no PR linking simply yields false, and the
// caller's branch check is the backstop.
func (c *Client) hasLinkedPR(key string) (bool, error) {
	data, err := c.transport("GET", "/rest/api/2/issue/"+url.PathEscape(key)+"/remotelink", nil)
	if err != nil {
		return false, err
	}
	var links []remoteLink
	if err := json.Unmarshal(data, &links); err != nil {
		return false, err
	}
	for _, l := range links {
		if strings.Contains(l.Object.URL, "/pull/") {
			return true, nil
		}
	}
	return false, nil
}
