package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/danoleary/agent-harness/internal/ci"
)

// Pulls is the host-side read/write surface the PR loop needs from GitHub
// (ADR-0019): the open PRs, a PR's mergeability, its head commit's checks, and the
// comments and labels the loop keeps its lease and its attempt record in. It is
// separate from [Client] because it is not a Tracker: it works whatever repository
// the Consumer's checkout pushes to, whichever tracker holds its tickets.
type Pulls struct {
	transport Transport
	repo      string
}

// NewPulls binds the client to repo, "owner/name". The gh transport
// ([NewGhTransport]) also accepts gh's own "{owner}/{repo}" placeholders, which it
// fills from the checkout — so the PR loop needs no repo setting at all.
func NewPulls(t Transport, repo string) *Pulls { return &Pulls{transport: t, repo: repo} }

func (p *Pulls) path(format string, a ...any) string {
	return "/repos/" + p.repo + fmt.Sprintf(format, a...)
}

// PullRequest is one open PR, reduced to what the PR loop decides on.
type PullRequest struct {
	Number     int
	Title      string
	Body       string
	URL        string
	HeadBranch string
	HeadSHA    string
	BaseBranch string
	// Fork marks a PR whose head lives outside the base repository (or whose fork
	// was deleted): the harness cannot push there.
	Fork   bool
	Labels []string
}

// HasLabel reports whether the PR carries label.
func (pr PullRequest) HasLabel(label string) bool {
	for _, l := range pr.Labels {
		if l == label {
			return true
		}
	}
	return false
}

type pullRef struct {
	Ref  string `json:"ref"`
	SHA  string `json:"sha"`
	Repo *struct {
		FullName string `json:"full_name"`
	} `json:"repo"`
}

type pull struct {
	Number  int     `json:"number"`
	Title   string  `json:"title"`
	Body    string  `json:"body"`
	HTMLURL string  `json:"html_url"`
	Head    pullRef `json:"head"`
	Base    pullRef `json:"base"`
	Labels  []struct {
		Name string `json:"name"`
	} `json:"labels"`
	Mergeable      *bool  `json:"mergeable"`
	MergeableState string `json:"mergeable_state"`
}

// OpenPRs lists the repository's open PRs (the first 100 — a loop that has more
// open PRs than that has a different problem).
func (p *Pulls) OpenPRs() ([]PullRequest, error) {
	data, err := p.transport("GET", p.path("/pulls?state=open&per_page=100"), nil)
	if err != nil {
		return nil, err
	}
	var pulls []pull
	if err := json.Unmarshal(data, &pulls); err != nil {
		return nil, err
	}
	out := make([]PullRequest, 0, len(pulls))
	for _, pl := range pulls {
		pr := PullRequest{
			Number: pl.Number, Title: pl.Title, Body: pl.Body, URL: pl.HTMLURL,
			HeadBranch: pl.Head.Ref, HeadSHA: pl.Head.SHA, BaseBranch: pl.Base.Ref,
			Fork: pl.Head.Repo == nil || pl.Base.Repo == nil || pl.Head.Repo.FullName != pl.Base.Repo.FullName,
		}
		for _, l := range pl.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		out = append(out, pr)
	}
	return out, nil
}

// Files lists the paths one PR changes, both sides of a rename included: the
// PR's work in flight, which the ticket loop's overlap check compares a candidate
// ticket against (#97). One page of 100 covers the PRs the harness opens.
func (p *Pulls) Files(number int) ([]string, error) {
	data, err := p.transport("GET", p.path("/pulls/%d/files?per_page=100", number), nil)
	if err != nil {
		return nil, err
	}
	var files []struct {
		Filename         string `json:"filename"`
		PreviousFilename string `json:"previous_filename"`
	}
	if err := json.Unmarshal(data, &files); err != nil {
		return nil, err
	}
	var out []string
	for _, f := range files {
		out = append(out, f.Filename)
		if f.PreviousFilename != "" {
			out = append(out, f.PreviousFilename)
		}
	}
	return out, nil
}

// MergeState reads one PR's mergeability. The list endpoint never carries it, and
// GitHub computes it lazily, so an unread PR answers MergeUnknown until the next
// poll. The REST shape is mapped onto the GraphQL one ci.ClassifyMergeState reads,
// so the PR loop and the CI watch share one definition of "conflicting".
func (p *Pulls) MergeState(number int) (ci.MergeVerdict, error) {
	data, err := p.transport("GET", p.path("/pulls/%d", number), nil)
	if err != nil {
		return ci.MergeUnknown, err
	}
	var pl pull
	if err := json.Unmarshal(data, &pl); err != nil {
		return ci.MergeUnknown, err
	}
	s := ci.MergeStatus{Mergeable: "UNKNOWN", MergeStateStatus: strings.ToUpper(pl.MergeableState)}
	if pl.Mergeable != nil {
		s.Mergeable = "MERGEABLE"
		if !*pl.Mergeable {
			s.Mergeable = "CONFLICTING"
		}
	}
	return ci.ClassifyMergeState(s), nil
}

// ChecksVerdict is the head commit's CI, folded to the one question the PR loop
// asks: is there a red check to fix?
type ChecksVerdict int

const (
	// ChecksPending — something is still running and nothing has failed yet.
	ChecksPending ChecksVerdict = iota
	// ChecksGreen — every check finished without failing (or there are none).
	ChecksGreen
	// ChecksRed — at least one check failed.
	ChecksRed
)

func (v ChecksVerdict) String() string {
	switch v {
	case ChecksGreen:
		return "green"
	case ChecksRed:
		return "red"
	default:
		return "pending"
	}
}

// redConclusions are the check-run conclusions that mean the commit failed. A
// cancelled run is left out: it is usually a superseded run, not a red check.
var redConclusions = map[string]bool{"failure": true, "timed_out": true, "startup_failure": true, "action_required": true}

// Checks folds the head commit's check runs and its legacy commit statuses into one
// verdict. A failure wins even while other checks still run: the loop has its
// answer, and the CI watch waits for a terminal result itself.
func (p *Pulls) Checks(sha string) (ChecksVerdict, error) {
	data, err := p.transport("GET", p.path("/commits/%s/check-runs?per_page=100", sha), nil)
	if err != nil {
		return ChecksPending, err
	}
	var runs struct {
		CheckRuns []struct {
			Status     string `json:"status"`
			Conclusion string `json:"conclusion"`
		} `json:"check_runs"`
	}
	if err := json.Unmarshal(data, &runs); err != nil {
		return ChecksPending, err
	}
	data, err = p.transport("GET", p.path("/commits/%s/status", sha), nil)
	if err != nil {
		return ChecksPending, err
	}
	var status struct {
		State      string `json:"state"`
		TotalCount int    `json:"total_count"`
	}
	if err := json.Unmarshal(data, &status); err != nil {
		return ChecksPending, err
	}

	pending := false
	for _, r := range runs.CheckRuns {
		if r.Status != "completed" {
			pending = true
			continue
		}
		if redConclusions[r.Conclusion] {
			return ChecksRed, nil
		}
	}
	// A commit with no legacy statuses reports "pending"; only a real one counts.
	if status.TotalCount > 0 {
		switch status.State {
		case "failure", "error":
			return ChecksRed, nil
		case "pending":
			pending = true
		}
	}
	if pending {
		return ChecksPending, nil
	}
	return ChecksGreen, nil
}

// PRComment is one comment on a PR's conversation.
type PRComment struct {
	Body      string
	CreatedAt time.Time
}

// Comments lists a PR's conversation comments, oldest first (the first 100).
func (p *Pulls) Comments(number int) ([]PRComment, error) {
	data, err := p.transport("GET", p.path("/issues/%d/comments?per_page=100", number), nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Body      string    `json:"body"`
		CreatedAt time.Time `json:"created_at"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]PRComment, 0, len(raw))
	for _, c := range raw {
		out = append(out, PRComment{Body: c.Body, CreatedAt: c.CreatedAt})
	}
	return out, nil
}

// Comment posts a comment on a PR's conversation.
func (p *Pulls) Comment(number int, body string) error {
	_, err := p.transport("POST", p.path("/issues/%d/comments", number), map[string]any{"body": body})
	return err
}

// AddLabel adds label to a PR, creating the label in the repository if it is new.
func (p *Pulls) AddLabel(number int, label string) error {
	_, err := p.transport("POST", p.path("/issues/%d/labels", number), map[string]any{"labels": []string{label}})
	return err
}

// RemoveLabel removes label from a PR.
func (p *Pulls) RemoveLabel(number int, label string) error {
	_, err := p.transport("DELETE", p.path("/issues/%d/labels/%s", number, url.PathEscape(label)), nil)
	return err
}
