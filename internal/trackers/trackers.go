// Package trackers wires the concrete tracker adapters to the host-side tracker
// port and selects one by the Consumer's configured kind (ADR-0010: the port has
// Linear/Jira/GitHub adapters, selected by `.agent-harness/config`). It is the one
// place adapter selection happens, so the five construction sites (the three
// stages plus cmd/loop and cmd/pipeline) share it and a future adapter plugs in
// here, not at every call site.
//
// It sits above internal/tracker (the port) and the adapters (internal/linear,
// internal/github): the port must not import an adapter, so the selection lives
// here. It also maps the non-secret config.TrackerConfig selection surface onto
// each adapter's own shape, so the adapters stay config-free.
package trackers

import (
	"fmt"
	"strings"

	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/github"
	"github.com/beherd/agent-harness/internal/jira"
	"github.com/beherd/agent-harness/internal/linear"
	"github.com/beherd/agent-harness/internal/tracker"
)

// Secrets carries the host-only tracker credentials (ADR-0001: they stay on the
// host, never crossing the sandbox boundary). Only the fields the selected kind
// needs are read — a Linear consumer leaves the GitHub/Jira fields empty. Grouped
// into a struct so a fourth adapter's credential doesn't grow New's arg list.
type Secrets struct {
	// LinearKey is the Linear API key (kind=linear).
	LinearKey string
	// GitHubToken is the host's GH_TOKEN (kind=github).
	GitHubToken string
	// JiraBaseURL / JiraEmail / JiraToken are Jira Cloud's Basic-auth triple
	// (kind=jira): the site URL, the account email, and its API token.
	JiraBaseURL string
	JiraEmail   string
	JiraToken   string
}

// New returns the tracker adapter for the configured kind, holding the host-only
// credential from secrets (ADR-0001). Linear uses LinearKey; GitHub uses
// GitHubToken (the host's existing GH_TOKEN); Jira uses the Basic-auth triple. An
// unsupported kind — a github selection missing its repo, or a jira selection
// missing its auth — is a loud error: the harness refuses to run against a tracker
// it can't address.
func New(tc config.TrackerConfig, secrets Secrets) (tracker.Tracker, error) {
	switch tc.Kind {
	case "linear":
		return linear.NewClient(linear.NewTransport(secrets.LinearKey)), nil
	case "github":
		owner, repo, err := splitRepo(tc.Repo)
		if err != nil {
			return nil, err
		}
		return github.NewClient(github.NewTransport(secrets.GitHubToken), owner, repo, github.Options{
			Ready:      tc.ReadyLabel,
			Blocked:    tc.BlockedLabel,
			InProgress: tc.InProgressLabel,
			Findings:   tc.FindingsLabelID,
			Assignee:   tc.Assignee,
		}), nil
	case "jira":
		if secrets.JiraBaseURL == "" || secrets.JiraEmail == "" || secrets.JiraToken == "" {
			return nil, fmt.Errorf("kind=jira requires the host-only secrets JIRA_BASE_URL, JIRA_EMAIL, and JIRA_API_TOKEN")
		}
		return jira.NewClient(jira.NewTransport(secrets.JiraBaseURL, secrets.JiraEmail, secrets.JiraToken), secrets.JiraBaseURL, jira.Options{
			ProjectKey:           tc.ProjectKey,
			ReadyJQL:             tc.ReadyJQL,
			InProgressJQL:        tc.InProgressJQL,
			Findings:             tc.FindingsLabelID,
			FindingsIssueType:    tc.FindingsIssueType,
			InProgressTransition: tc.InProgressTransition,
			TodoTransition:       tc.TodoTransition,
			CanceledTransition:   tc.CanceledTransition,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported tracker kind %q (supported: linear, github, jira)", tc.Kind)
	}
}

// splitRepo parses a github "owner/name" into its parts, failing loud on a value
// that isn't exactly one owner and one name — a repo-scoped adapter can't address
// any issue without both.
func splitRepo(repo string) (string, string, error) {
	owner, name, ok := strings.Cut(repo, "/")
	if !ok || owner == "" || name == "" || strings.Contains(name, "/") {
		return "", "", fmt.Errorf("tracker.repo must be \"owner/name\" for kind=github, got %q", repo)
	}
	return owner, name, nil
}
