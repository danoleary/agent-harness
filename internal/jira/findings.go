package jira

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
// invisible in rendered Markdown. The Linear and GitHub adapters carry identical
// private copies; the shape is deliberately the same so a finding filed under one
// tracker reads the same as under another. (A shared home for these is a
// worthwhile future consolidation — see the handoff.)

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

// commentPath is the REST path for an issue's comments (v2, plain-string body).
func (c *Client) commentPath(key string) string {
	return "/rest/api/2/issue/" + url.PathEscape(key) + "/comment"
}

// FileFinding files a harness-improvement finding as a new project issue tagged
// with the findings label (so SearchFindings can dedup it later) and carrying the
// `<!-- finding-key: … -->` marker when the finding has an explicit dedup key. The
// Key of the created issue is its issue key.
func (c *Client) FileFinding(f findings.Finding, opts tracker.FileFindingOptions) (tracker.CreatedIssue, error) {
	kindLine := ""
	if f.Kind != "" {
		kindLine = "*Kind:* " + f.Kind + "\n\n"
	}
	desc := fmt.Sprintf(
		"%s%s\n\n_Surfaced during %s by the agent harness._",
		kindLine, f.Body, opts.RelatedKey,
	)
	if key := strings.TrimSpace(f.Key); key != "" {
		desc += "\n\n" + findingKeyMarker(key)
	}

	project := strings.TrimSpace(opts.TeamID)
	if project == "" {
		project = c.opts.ProjectKey
	}
	data, err := c.transport("POST", "/rest/api/2/issue", map[string]any{
		"fields": map[string]any{
			"project":     map[string]any{"key": project},
			"summary":     f.Title,
			"description": desc,
			"labels":      []string{c.opts.Findings},
			"issuetype":   map[string]any{"name": c.opts.FindingsIssueType},
		},
	})
	if err != nil {
		return tracker.CreatedIssue{}, err
	}
	var created struct {
		Key string `json:"key"`
	}
	if err := json.Unmarshal(data, &created); err != nil {
		return tracker.CreatedIssue{}, err
	}
	if created.Key == "" {
		return tracker.CreatedIssue{}, fmt.Errorf("failed to file finding: %s", f.Title)
	}
	return tracker.CreatedIssue{Identifier: created.Key, URL: c.browseURL(created.Key)}, nil
}

// findingsSearchJQL scopes the dedup read to the project's open, findings-labelled
// issues. `statusCategory != Done` excludes terminal issues, so SearchFindings
// never returns a closed finding — a closed match can't suppress a re-file
// (ExistingFinding.Closed is always false here).
func (c *Client) findingsSearchJQL(project string) string {
	return fmt.Sprintf("project = %s AND labels = %s AND statusCategory != Done", project, c.opts.Findings)
}

// SearchFindings lists the project's already-filed open harness findings (scoped
// by the findings label), recovering each one's dedup key from its description
// marker. teamID selects the project to search (falling back to the configured
// ProjectKey when empty). The caller decides the dedup policy.
func (c *Client) SearchFindings(teamID string) ([]tracker.ExistingFinding, error) {
	project := strings.TrimSpace(teamID)
	if project == "" {
		project = c.opts.ProjectKey
	}
	data, err := c.transport("GET", searchPath(c.findingsSearchJQL(project), "summary,description"), nil)
	if err != nil {
		return nil, err
	}
	var res searchResult
	if err := json.Unmarshal(data, &res); err != nil {
		return nil, err
	}
	out := make([]tracker.ExistingFinding, 0, len(res.Issues))
	for _, iss := range res.Issues {
		out = append(out, tracker.ExistingFinding{
			Identifier: iss.Key,
			Title:      iss.Fields.Summary,
			Key:        extractFindingKey(iss.Fields.Description),
			Closed:     false,
		})
	}
	return out, nil
}

// RecordOccurrence records that an already-tracked finding recurred during
// relatedKey's pipeline (BEH-573): it bumps the issue's occurrence marker
// (defaulting to 1 for the original filing) via a description PUT and appends a
// breadcrumb comment, returning the new count. If the comment fails after the
// bump landed, the count is still advanced (the breadcrumb is the lossy half).
func (c *Client) RecordOccurrence(key, relatedKey string) (int, error) {
	data, err := c.transport("GET", c.issuePath(key, "fields=description"), nil)
	if err != nil {
		return 0, err
	}
	var iss issue
	if err := json.Unmarshal(data, &iss); err != nil {
		return 0, err
	}

	next := extractOccurrences(iss.Fields.Description) + 1
	if _, err := c.transport("PUT", "/rest/api/2/issue/"+url.PathEscape(key), map[string]any{
		"fields": map[string]any{"description": withOccurrences(iss.Fields.Description, next)},
	}); err != nil {
		return 0, err
	}

	comment := fmt.Sprintf("Recurred in %s pipeline (occurrence %d).", relatedKey, next)
	if _, err := c.transport("POST", c.commentPath(key), map[string]any{"body": comment}); err != nil {
		return next, err
	}
	return next, nil
}

// AddComment posts a comment to an issue by its key. Used by the pre-push
// conflict path (BEH-581) to leave an autonomous breadcrumb; the caller treats a
// failure best-effort.
func (c *Client) AddComment(key, body string) error {
	_, err := c.transport("POST", c.commentPath(key), map[string]any{"body": body})
	return err
}
