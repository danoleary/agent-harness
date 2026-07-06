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
	"github.com/beherd/agent-harness/internal/linear"
	"github.com/beherd/agent-harness/internal/tracker"
)

// New returns the tracker adapter for the configured kind, holding the host-only
// credential (ADR-0001: the key stays on the host, never crossing the sandbox
// boundary). Linear uses linearKey; GitHub uses githubToken (the host's existing
// GH_TOKEN). An unsupported kind — or a github selection missing its repo — is a
// loud error: the harness refuses to run against a tracker it can't address. Jira
// lands in BEH-638.
func New(tc config.TrackerConfig, linearKey, githubToken string) (tracker.Tracker, error) {
	switch tc.Kind {
	case "linear":
		return linear.NewClient(linear.NewTransport(linearKey)), nil
	case "github":
		owner, repo, err := splitRepo(tc.Repo)
		if err != nil {
			return nil, err
		}
		return github.NewClient(github.NewTransport(githubToken), owner, repo, github.Options{
			Ready:      tc.ReadyLabel,
			Blocked:    tc.BlockedLabel,
			InProgress: tc.InProgressLabel,
			Findings:   tc.FindingsLabelID,
			Assignee:   tc.Assignee,
		}), nil
	default:
		return nil, fmt.Errorf("unsupported tracker kind %q (supported: linear, github)", tc.Kind)
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
