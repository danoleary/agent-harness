// Package trackers wires the concrete tracker adapters to the host-side tracker
// port and selects one by the Consumer's configured kind (ADR-0010: the port has
// Linear/Jira/GitHub adapters, selected by `.agent-harness/config`). It is the one
// place adapter selection happens: cmd/loop, cmd/pipeline's --next and the
// per-run hostio.Real all build theirs through FromConfig, so a future adapter
// plugs in here, not at every call site.
//
// It sits above internal/tracker (the port) and the adapters (internal/linear,
// internal/github): the port must not import an adapter, so the selection lives
// here. It also maps the non-secret config.TrackerConfig selection surface onto
// each adapter's own shape, so the adapters stay config-free.
package trackers

import (
	"fmt"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
	"github.com/danoleary/agent-harness/internal/github"
	"github.com/danoleary/agent-harness/internal/jira"
	"github.com/danoleary/agent-harness/internal/linear"
	"github.com/danoleary/agent-harness/internal/tracker"
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
// unsupported kind — or any selection missing its credential or its addressing
// (a github selection with no repo, a linear one with no API key, a jira one with
// no auth triple) — is a loud error: the harness refuses to run against a tracker
// it can't address. The credential check lives here rather than in config.Load
// because WHICH credential is required depends on the kind (BEH-641). Each of
// those errors ends by pointing at `agent-harness doctor`, which reports the rest
// of the setup in the same pass.
func New(tc config.TrackerConfig, secrets Secrets) (tracker.Tracker, error) {
	t, err := newTracker(tc, secrets)
	if err != nil {
		return nil, config.WithDoctorHint(err)
	}
	return t, nil
}

// FromConfig is New over the loaded config: the Consumer's tracker selection and
// the host's credentials for it. It is the one place the config's secrets map onto
// Secrets (#29), so every composition root builds the tracker the same way.
func FromConfig(cfg config.Config) (tracker.Tracker, error) {
	return New(cfg.Tracker, Secrets{
		LinearKey:   cfg.LinearAPIKey,
		GitHubToken: cfg.GitHubToken,
		JiraBaseURL: cfg.JiraBaseURL,
		JiraEmail:   cfg.JiraEmail,
		JiraToken:   cfg.JiraAPIToken,
	})
}

func newTracker(tc config.TrackerConfig, secrets Secrets) (tracker.Tracker, error) {
	switch tc.Kind {
	case "linear":
		if secrets.LinearKey == "" {
			return nil, fmt.Errorf("LINEAR_API_KEY is not set. tracker.kind = \"linear\" reads and claims tickets with it.\n" +
				"Fix: create a personal API key at https://linear.app/settings/account/security and set LINEAR_API_KEY to it")
		}
		return linear.NewClient(linear.NewTransport(secrets.LinearKey), linear.Options{
			TeamKey:         tc.TeamKey,
			Ready:           tc.ReadyLabel,
			Blocked:         tc.BlockedLabel,
			FindingsLabelID: tc.FindingsLabelID,
			FindingsLabel:   tc.FindingsLabel,
		}), nil
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
		if missing := missingJiraSecrets(secrets); len(missing) > 0 {
			verb := "is"
			if len(missing) > 1 {
				verb = "are"
			}
			return nil, fmt.Errorf("%s %s not set. tracker.kind = \"jira\" signs in to the Jira site (JIRA_BASE_URL, e.g. "+
				"https://acme.atlassian.net) as an account (JIRA_EMAIL) with an API token (JIRA_API_TOKEN).\n"+
				"Fix: create an API token at https://id.atlassian.com/manage-profile/security/api-tokens and set the missing variables",
				strings.Join(missing, " and "), verb)
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

// missingJiraSecrets names the unset members of the Jira Basic-auth triple, so the
// error asks for exactly what is missing.
func missingJiraSecrets(s Secrets) []string {
	var missing []string
	for _, v := range []struct{ name, value string }{
		{"JIRA_BASE_URL", s.JiraBaseURL},
		{"JIRA_EMAIL", s.JiraEmail},
		{"JIRA_API_TOKEN", s.JiraToken},
	} {
		if v.value == "" {
			missing = append(missing, v.name)
		}
	}
	return missing
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
