package trackers

import (
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/config"
	"github.com/beherd/agent-harness/internal/tracker"
)

// New selects the tracker adapter by the config's kind (ADR-0010: "selected by
// .agent-harness/config"). Linear and GitHub Issues both sit behind the port.
func TestNewSelectsLinearAdapter(t *testing.T) {
	var got tracker.Tracker
	got, err := New(config.TrackerConfig{Kind: "linear"}, "lin_api_key", "gh_token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected a Tracker for kind=linear, got nil")
	}
}

// The GitHub adapter is selected purely by config: kind + repo. It uses the host's
// GH_TOKEN, never the Linear key.
func TestNewSelectsGitHubAdapter(t *testing.T) {
	got, err := New(config.TrackerConfig{
		Kind:            "github",
		Repo:            "acme/widgets",
		ReadyLabel:      "ready-for-agent",
		InProgressLabel: "in-progress",
	}, "", "gh_token")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected a Tracker for kind=github, got nil")
	}
}

// A GitHub selection with no repo is a misconfiguration — the adapter is repo-
// scoped, so it must fail loud rather than build a client that can't address any
// issue.
func TestNewGitHubRequiresRepo(t *testing.T) {
	_, err := New(config.TrackerConfig{Kind: "github"}, "", "gh_token")
	if err == nil {
		t.Fatal("expected an error for github with no repo, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "repo") {
		t.Errorf("error should name the missing repo, got: %v", err)
	}
}

// A repo with more than one "/" is malformed — a GitHub repo is exactly
// owner/name, so "a/b/c" would silently address a bogus path. It must fail loud
// like the empty case rather than build a client that 404s on first use.
func TestNewGitHubRejectsMalformedRepo(t *testing.T) {
	_, err := New(config.TrackerConfig{Kind: "github", Repo: "acme/widgets/extra"}, "", "gh_token")
	if err == nil {
		t.Fatal("expected an error for a multi-segment repo, got nil")
	}
	if !strings.Contains(strings.ToLower(err.Error()), "repo") {
		t.Errorf("error should name the malformed repo, got: %v", err)
	}
}

// An unknown kind must fail loud, naming the kind — the harness refuses to launch
// against a tracker it has no adapter for rather than silently doing nothing.
func TestNewRejectsUnknownKind(t *testing.T) {
	_, err := New(config.TrackerConfig{Kind: "bugzilla"}, "key", "gh_token")
	if err == nil {
		t.Fatal("expected an error for an unsupported kind, got nil")
	}
	if !strings.Contains(err.Error(), "bugzilla") {
		t.Errorf("error should name the unsupported kind, got: %v", err)
	}
}
