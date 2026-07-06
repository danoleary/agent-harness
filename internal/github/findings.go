package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strconv"
	"strings"

	"github.com/beherd/agent-harness/internal/findings"
	"github.com/beherd/agent-harness/internal/tracker"
)

// The finding-key and occurrence markers are the harness's cross-adapter dedup
// protocol: machine-readable HTML comments embedded in a filed finding's body,
// invisible in rendered Markdown. The Linear adapter carries an identical private
// copy; the shape is deliberately the same so a finding filed under one tracker
// reads the same as under another. (A shared home for these is a worthwhile future
// consolidation — see the handoff.)

// findingKeyRe matches the dedup marker `<!-- finding-key: <key> -->`.
var findingKeyRe = regexp.MustCompile(`<!--\s*finding-key:\s*(\S+)\s*-->`)

// extractFindingKey pulls the dedup key out of a filed finding's body, or "" when
// the body carries no marker.
func extractFindingKey(body string) string {
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

// occurrenceRe matches the occurrence-count marker `<!-- occurrences: <n> -->`.
var occurrenceRe = regexp.MustCompile(`<!--\s*occurrences:\s*(\d+)\s*-->`)

// occurrenceMarker renders the body marker for an occurrence count.
func occurrenceMarker(n int) string {
	return fmt.Sprintf("<!-- occurrences: %d -->", n)
}

// extractOccurrences reads the occurrence count from a finding body, defaulting to
// 1 (the original filing) when the body carries no marker yet.
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

// createdIssue is the subset of a created/patched issue the sink reads back.
type createdIssue struct {
	Number  int    `json:"number"`
	HTMLURL string `json:"html_url"`
}

// FileFinding files a harness-improvement finding as a new repo issue tagged with
// the findings label (so SearchFindings can dedup it later) and carrying the
// `<!-- finding-key: … -->` marker when the finding has an explicit dedup key. The
// Key of the created issue is its #number.
func (c *Client) FileFinding(f findings.Finding, opts tracker.FileFindingOptions) (tracker.CreatedIssue, error) {
	kindLine := ""
	if f.Kind != "" {
		kindLine = "**Kind:** " + f.Kind + "\n\n"
	}
	body := fmt.Sprintf(
		"%s%s\n\n_Surfaced during %s by the agent harness._",
		kindLine, f.Body, opts.RelatedKey,
	)
	if key := strings.TrimSpace(f.Key); key != "" {
		body += "\n\n" + findingKeyMarker(key)
	}

	data, err := c.transport("POST", c.issuesPath(""), map[string]any{
		"title":  f.Title,
		"body":   body,
		"labels": []string{c.opts.Findings},
	})
	if err != nil {
		return tracker.CreatedIssue{}, err
	}
	var iss createdIssue
	if err := json.Unmarshal(data, &iss); err != nil {
		return tracker.CreatedIssue{}, err
	}
	if iss.Number == 0 {
		return tracker.CreatedIssue{}, fmt.Errorf("failed to file finding: %s", f.Title)
	}
	return tracker.CreatedIssue{Identifier: numberToKey(iss.Number), URL: iss.HTMLURL}, nil
}

// searchQuery builds the list-issues query for the findings dedup read: the
// repo's open, findings-labelled issues. Querying open-only means SearchFindings
// never returns a closed finding, so a closed match can't suppress a re-file
// (ExistingFinding.Closed is always false here).
func (c *Client) searchQuery() string {
	v := url.Values{}
	v.Set("labels", c.opts.Findings)
	v.Set("state", "open")
	v.Set("per_page", "100")
	return c.issuesPath("") + "?" + v.Encode()
}

// SearchFindings lists the repo's already-filed open harness findings (scoped by
// the findings label), recovering each one's dedup key from its body marker and
// screening out pull requests. teamID is ignored: a GitHub client is repo-bound,
// so its own owner/repo is the container. The caller decides the dedup policy.
func (c *Client) SearchFindings(teamID string) ([]tracker.ExistingFinding, error) {
	data, err := c.transport("GET", c.searchQuery(), nil)
	if err != nil {
		return nil, err
	}
	var issues []issue
	if err := json.Unmarshal(data, &issues); err != nil {
		return nil, err
	}
	out := make([]tracker.ExistingFinding, 0, len(issues))
	for _, iss := range issues {
		if iss.PullRequest != nil {
			continue
		}
		out = append(out, tracker.ExistingFinding{
			Identifier: numberToKey(iss.Number),
			Title:      iss.Title,
			Key:        extractFindingKey(iss.Body),
			Closed:     false,
		})
	}
	return out, nil
}

// RecordOccurrence records that an already-tracked finding recurred during
// relatedKey's pipeline (BEH-573): it bumps the issue's occurrence marker
// (defaulting to 1 for the original filing) via a body PATCH and appends a
// breadcrumb comment, returning the new count. If the comment fails after the
// bump landed, the count is still advanced (the breadcrumb is the lossy half).
func (c *Client) RecordOccurrence(key, relatedKey string) (int, error) {
	n := keyToNumber(key)
	data, err := c.transport("GET", c.issuesPath(n), nil)
	if err != nil {
		return 0, err
	}
	var iss issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return 0, err
	}

	next := extractOccurrences(iss.Body) + 1
	if _, err := c.transport("PATCH", c.issuesPath(n), map[string]any{
		"body": withOccurrences(iss.Body, next),
	}); err != nil {
		return 0, err
	}

	comment := fmt.Sprintf("Recurred in %s pipeline (occurrence %d).", relatedKey, next)
	if _, err := c.transport("POST", c.issuesPath(n)+"/comments", map[string]any{"body": comment}); err != nil {
		return next, err
	}
	return next, nil
}

// AddComment posts a comment to an issue by its #number. GitHub's comments
// endpoint keys off the issue number directly, so — unlike the Linear adapter — no
// id resolution round-trip is needed. Used by the pre-push conflict path (BEH-581)
// to leave an autonomous breadcrumb; the caller treats a failure best-effort.
func (c *Client) AddComment(key, body string) error {
	_, err := c.transport("POST", c.issuesPath(keyToNumber(key))+"/comments", map[string]any{"body": body})
	return err
}
