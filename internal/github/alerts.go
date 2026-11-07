package github

import (
	"encoding/json"
	"fmt"
	"net/url"
	"strings"
	"time"
)

// Alert is one open Dependabot alert, reduced to what the security-alerts Source
// (ADR-0040) groups by and the host check reads: the package, the vulnerable range
// and the first patched version.
type Alert struct {
	Number       int
	URL          string
	Ecosystem    string
	Package      string
	ManifestPath string
	// Scope is "runtime" or "development".
	Scope    string
	GHSA     string
	CVE      string
	Summary  string
	Severity string
	// VulnerableRange is GitHub's range expression, e.g. "< 4.17.21".
	VulnerableRange string
	// Patched is the first patched version; empty when there is none, so no
	// dependency change can fix the alert (ADR-0040, decision 2).
	Patched string
}

type alert struct {
	Number     int    `json:"number"`
	HTMLURL    string `json:"html_url"`
	Dependency struct {
		Package struct {
			Ecosystem string `json:"ecosystem"`
			Name      string `json:"name"`
		} `json:"package"`
		ManifestPath string `json:"manifest_path"`
		Scope        string `json:"scope"`
	} `json:"dependency"`
	Advisory struct {
		GHSAID   string `json:"ghsa_id"`
		CVEID    string `json:"cve_id"`
		Summary  string `json:"summary"`
		Severity string `json:"severity"`
	} `json:"security_advisory"`
	Vulnerability struct {
		VulnerableVersionRange string `json:"vulnerable_version_range"`
		FirstPatchedVersion    *struct {
			Identifier string `json:"identifier"`
		} `json:"first_patched_version"`
	} `json:"security_vulnerability"`
}

// DependabotAlerts lists the repository's open Dependabot alerts at severity (all
// open alerts when severity is empty). It takes the first 100 only, as [OpenPRs]
// does: the [Transport] drops the Link header, so it cannot page further.
func (p *Pulls) DependabotAlerts(severity []string) ([]Alert, error) {
	query := "state=open"
	if len(severity) > 0 {
		query += "&severity=" + strings.Join(severity, ",")
	}
	data, err := p.transport("GET", p.path("/dependabot/alerts?%s&per_page=100", query), nil)
	if err != nil {
		// Both transports carry the status as "HTTP 403": the token can read the
		// repository but not its alerts.
		if strings.Contains(err.Error(), "HTTP 403") {
			return nil, fmt.Errorf("reading Dependabot alerts: %w; the token needs the `security_events` scope (classic PAT) or Dependabot alerts read permission (fine-grained)", err)
		}
		return nil, err
	}
	var raw []alert
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]Alert, 0, len(raw))
	for _, a := range raw {
		al := Alert{
			Number: a.Number, URL: a.HTMLURL,
			Ecosystem: a.Dependency.Package.Ecosystem, Package: a.Dependency.Package.Name,
			ManifestPath: a.Dependency.ManifestPath, Scope: a.Dependency.Scope,
			GHSA: a.Advisory.GHSAID, CVE: a.Advisory.CVEID, Summary: a.Advisory.Summary, Severity: a.Advisory.Severity,
			VulnerableRange: a.Vulnerability.VulnerableVersionRange,
		}
		if fp := a.Vulnerability.FirstPatchedVersion; fp != nil {
			al.Patched = fp.Identifier
		}
		out = append(out, al)
	}
	return out, nil
}

// BranchPR is one PR from a branch, in any state: what the security-alerts Source
// reads to find its earlier attempts on a package (ADR-0040, decision 7).
type BranchPR struct {
	Number int
	// State is "open" or "closed"; a merged PR is closed with MergedAt set.
	State    string
	MergedAt time.Time
	Labels   []string
	Body     string
}

// Merged reports whether the PR was merged.
func (pr BranchPR) Merged() bool { return !pr.MergedAt.IsZero() }

// PRsForBranch lists every PR, open, closed or merged, whose head is branch in
// this repository. head= takes "owner:branch"; the owner is the repo's own, or
// gh's {owner} placeholder, which gh fills anywhere in the path. It takes the
// first 100 only, as [OpenPRs] does: the [Transport] drops the Link header, so it
// cannot page further.
func (p *Pulls) PRsForBranch(branch string) ([]BranchPR, error) {
	owner, _, _ := strings.Cut(p.repo, "/")
	data, err := p.transport("GET", p.path("/pulls?state=all&head=%s:%s&per_page=100", owner, url.QueryEscape(branch)), nil)
	if err != nil {
		return nil, err
	}
	var raw []struct {
		Number   int        `json:"number"`
		State    string     `json:"state"`
		MergedAt *time.Time `json:"merged_at"`
		Body     string     `json:"body"`
		Labels   []struct {
			Name string `json:"name"`
		} `json:"labels"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, err
	}
	out := make([]BranchPR, 0, len(raw))
	for _, r := range raw {
		pr := BranchPR{Number: r.Number, State: r.State, Body: r.Body}
		if r.MergedAt != nil {
			pr.MergedAt = *r.MergedAt
		}
		for _, l := range r.Labels {
			pr.Labels = append(pr.Labels, l.Name)
		}
		out = append(out, pr)
	}
	return out, nil
}
