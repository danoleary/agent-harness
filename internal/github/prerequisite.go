package github

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"github.com/danoleary/agent-harness/internal/ticket"
)

// bareRefRe matches a same-repo `#N` reference in prose. The leading group stands
// in for a lookbehind (RE2 has none): a `#` glued to a word, a path/URL fragment
// (`/path#16`) or an HTML entity (`&#17;`) is not a reference.
var bareRefRe = regexp.MustCompile(`(?:^|[^\w/&#])#(\d+)\b`)

// urlRefRe matches a GitHub issue or pull-request URL, capturing owner, repo and
// number so only the Client's own repo is followed (LCD: cross-repo dropped, as
// for task-list children).
var urlRefRe = regexp.MustCompile(`https://github\.com/([\w.-]+)/([\w.-]+)/(?:pull|issues)/(\d+)`)

// referencedNumbers extracts the de-duplicated same-repo issue/PR numbers a body
// names — bare `#N` and full URLs — in first-seen order, skipping self (#82). A
// ticket names its prerequisite in prose ("step one of the design in #81") far
// more often than as a tracker relation, so this is what selection reads.
func referencedNumbers(body, owner, repo string, self int) []int {
	type hit struct{ at, n int }
	var hits []hit
	for _, m := range bareRefRe.FindAllStringSubmatchIndex(body, -1) {
		n, _ := strconv.Atoi(body[m[2]:m[3]])
		hits = append(hits, hit{m[2], n})
	}
	for _, m := range urlRefRe.FindAllStringSubmatchIndex(body, -1) {
		if !strings.EqualFold(body[m[2]:m[3]], owner) || !strings.EqualFold(body[m[4]:m[5]], repo) {
			continue
		}
		n, _ := strconv.Atoi(body[m[6]:m[7]])
		hits = append(hits, hit{m[6], n})
	}
	// Order by position so the first prerequisite named is the first reported.
	sort.Slice(hits, func(i, j int) bool { return hits[i].at < hits[j].at })
	seen := map[int]bool{self: true}
	for _, n := range taskListChildNumbers(body, self) {
		seen[n] = true
	}
	var out []int
	for _, h := range hits {
		if !seen[h.n] {
			seen[h.n] = true
			out = append(out, h.n)
		}
	}
	return out
}

// openPRs reads every item body names and returns the ones that are still-open
// pull requests — an unmerged head branch the ticket builds on. A merged/closed
// PR is on main (or abandoned), and a plain issue named in prose is a mention, not
// a branch: a declared blocked-by relation is how an issue blocks. A reference
// that no longer resolves (404) holds nothing, so a stale number cannot wedge the
// queue; any other read failure is an error (fail closed, like openBlocker).
func (c *Client) openPRs(body string, self int) ([]issue, error) {
	var out []issue
	for _, n := range referencedNumbers(body, c.owner, c.repo, self) {
		data, err := c.transport("GET", c.issuesPath(strconv.Itoa(n)), nil)
		if errors.Is(err, errNotFound) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read %s referenced by %s: %w", numberToKey(n), numberToKey(self), err)
		}
		var ref issue
		if err := json.Unmarshal(data, &ref); err != nil {
			return nil, fmt.Errorf("read %s referenced by %s: %w", numberToKey(n), numberToKey(self), err)
		}
		if ref.PullRequest != nil && ref.State == "open" {
			out = append(out, ref)
		}
	}
	return out, nil
}

// prerequisites resolves each open PR body names to the branch it lives on, for
// the implementation envelope (#82).
func (c *Client) prerequisites(body string, self int) ([]ticket.Prerequisite, error) {
	prs, err := c.openPRs(body, self)
	if err != nil {
		return nil, err
	}
	var out []ticket.Prerequisite
	for _, pr := range prs {
		data, err := c.transport("GET", fmt.Sprintf("/repos/%s/%s/pulls/%d", c.owner, c.repo, pr.Number), nil)
		if err != nil {
			return nil, fmt.Errorf("read prerequisite %s: %w", numberToKey(pr.Number), err)
		}
		var p pull // pulls.go's shape; only its title, URL and head branch are read
		if err := json.Unmarshal(data, &p); err != nil {
			return nil, fmt.Errorf("read prerequisite %s: %w", numberToKey(pr.Number), err)
		}
		out = append(out, ticket.Prerequisite{
			Identifier: numberToKey(pr.Number),
			Title:      p.Title,
			Branch:     p.Head.Ref,
			URL:        p.HTMLURL,
		})
	}
	return out, nil
}
