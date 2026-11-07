package github

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

// The security-alerts Source (ADR-0040) reads the open Dependabot alerts at its
// configured severities in one list read, decoded down to what it groups and
// checks on. An alert with no patched version decodes to an empty Patched, so the
// Source can skip it without a session.
func TestDependabotAlertsDecodesTheOpenAlertsAtTheSeverities(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/dependabot/alerts?state=open&severity=critical,high&per_page=100": []any{
			map[string]any{
				"number": 7, "html_url": "https://github.com/acme/widgets/security/dependabot/7",
				"dependency": map[string]any{
					"package":       map[string]any{"ecosystem": "npm", "name": "lodash"},
					"manifest_path": "pnpm-lock.yaml", "scope": "runtime",
				},
				"security_advisory": map[string]any{
					"ghsa_id": "GHSA-xxxx-yyyy-zzzz", "cve_id": "CVE-2026-1234",
					"summary": "Prototype pollution", "severity": "high",
				},
				"security_vulnerability": map[string]any{
					"vulnerable_version_range": "< 4.17.21",
					"first_patched_version":    map[string]any{"identifier": "4.17.21"},
				},
			},
			map[string]any{
				"number": 8, "html_url": "https://github.com/acme/widgets/security/dependabot/8",
				"dependency": map[string]any{
					"package":       map[string]any{"ecosystem": "npm", "name": "left-pad"},
					"manifest_path": "packages/a/package.json", "scope": "development",
				},
				"security_advisory": map[string]any{
					"ghsa_id": "GHSA-aaaa-bbbb-cccc", "cve_id": nil,
					"summary": "No fix yet", "severity": "critical",
				},
				"security_vulnerability": map[string]any{
					"vulnerable_version_range": ">= 0",
					"first_patched_version":    nil,
				},
			},
		},
	})
	got, err := NewPulls(tr, "acme/widgets").DependabotAlerts([]string{"critical", "high"})
	if err != nil {
		t.Fatalf("DependabotAlerts: %v", err)
	}
	want := []Alert{
		{
			Number: 7, URL: "https://github.com/acme/widgets/security/dependabot/7",
			Ecosystem: "npm", Package: "lodash", ManifestPath: "pnpm-lock.yaml", Scope: "runtime",
			GHSA: "GHSA-xxxx-yyyy-zzzz", CVE: "CVE-2026-1234", Summary: "Prototype pollution", Severity: "high",
			VulnerableRange: "< 4.17.21", Patched: "4.17.21",
		},
		{
			Number: 8, URL: "https://github.com/acme/widgets/security/dependabot/8",
			Ecosystem: "npm", Package: "left-pad", ManifestPath: "packages/a/package.json", Scope: "development",
			GHSA: "GHSA-aaaa-bbbb-cccc", Summary: "No fix yet", Severity: "critical",
			VulnerableRange: ">= 0",
		},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("DependabotAlerts =\n%+v\nwant\n%+v", got, want)
	}
	if len(*calls) != 1 {
		t.Errorf("calls = %+v, want one list read", *calls)
	}
}

// With no severities configured the read is not filtered by severity at all,
// rather than sending an empty filter GitHub would reject.
func TestDependabotAlertsWithNoSeveritiesReadsEveryOpenAlert(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/dependabot/alerts?state=open&per_page=100": []any{},
	})
	got, err := NewPulls(tr, "acme/widgets").DependabotAlerts(nil)
	if err != nil {
		t.Fatalf("DependabotAlerts: %v", err)
	}
	if len(got) != 0 {
		t.Errorf("DependabotAlerts = %+v, want none", got)
	}
}

// A token without Dependabot read access gets a 403, from either transport. The
// error says which scope is missing, so the operator can fix the token rather than
// read it as a GitHub outage — and still wraps the transport's own error.
func TestDependabotAlertsNamesTheMissingScopeOnA403(t *testing.T) {
	for _, denied := range []error{
		errors.New("GitHub HTTP 403: Resource not accessible by personal access token"),
		errors.New("gh api GET /repos/{owner}/{repo}/dependabot/alerts: exit status 1: gh: Resource not accessible by personal access token (HTTP 403)"),
	} {
		tr := func(string, string, any) (json.RawMessage, error) { return nil, denied }
		_, err := NewPulls(tr, "acme/widgets").DependabotAlerts([]string{"high"})
		if !errors.Is(err, denied) {
			t.Fatalf("DependabotAlerts() = %v, want it to wrap %v", err, denied)
		}
		if !strings.Contains(err.Error(), "security_events") {
			t.Errorf("DependabotAlerts() = %q, want it to name the security_events scope", err)
		}
	}
}

// Any other failure is passed through without blaming the token's scopes.
func TestDependabotAlertsDoesNotBlameScopesForOtherFailures(t *testing.T) {
	down := errors.New("GitHub HTTP 502: Bad Gateway")
	tr := func(string, string, any) (json.RawMessage, error) { return nil, down }
	_, err := NewPulls(tr, "acme/widgets").DependabotAlerts([]string{"high"})
	if !errors.Is(err, down) || strings.Contains(err.Error(), "security_events") {
		t.Errorf("DependabotAlerts() = %v, want the 502 alone", err)
	}
}

// The Source finds its own earlier attempts on a package's branch (ADR-0040,
// decision 7): every PR from that branch, open, closed or merged, with the labels
// and the body that carries the attempt marker. head= is owner-qualified, and the
// owner is the gh placeholder when the repo is.
func TestPRsForBranchReadsEveryPRFromTheBranch(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/pulls?state=all&head=acme:security%2Flodash&per_page=100": []any{
			map[string]any{
				"number": 31, "state": "closed", "merged_at": "2026-11-01T09:30:00Z",
				"body": "<!-- marker -->", "labels": []any{map[string]any{"name": "in-progress"}},
			},
			map[string]any{"number": 35, "state": "open", "merged_at": nil, "body": nil},
		},
	})
	got, err := NewPulls(tr, "acme/widgets").PRsForBranch("security/lodash")
	if err != nil {
		t.Fatalf("PRsForBranch: %v", err)
	}
	want := []BranchPR{
		{Number: 31, State: "closed", MergedAt: time.Date(2026, 11, 1, 9, 30, 0, 0, time.UTC), Labels: []string{"in-progress"}, Body: "<!-- marker -->"},
		{Number: 35, State: "open"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("PRsForBranch =\n%+v\nwant\n%+v", got, want)
	}
	if !got[0].Merged() || got[1].Merged() {
		t.Errorf("Merged() = %v, %v; want true, false", got[0].Merged(), got[1].Merged())
	}
}

func TestPRsForBranchQualifiesHeadWithTheGhOwnerPlaceholder(t *testing.T) {
	tr, _ := fakeTransport(map[string]any{
		"GET /repos/{owner}/{repo}/pulls?state=all&head={owner}:feat%2Fgh-7&per_page=100": []any{},
	})
	if _, err := NewPulls(tr, "{owner}/{repo}").PRsForBranch("feat/gh-7"); err != nil {
		t.Fatalf("PRsForBranch: %v", err)
	}
}
