package jira

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/tracker"
)

// TestClientSatisfiesTrackerPort pins that the Jira adapter is a drop-in behind
// the host-side Tracker port: driving *Client through the interface (not the
// concrete type) proves it covers fetch / select+claim+release / file+dedup, and
// that an opaque PROJ-123 Key round-trips — the harness never assumes a BEH- shape.
func TestClientSatisfiesTrackerPort(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /rest/api/2/issue/PROJ-7?fields=summary,description,subtasks": map[string]any{
			"key":    "PROJ-7",
			"fields": map[string]any{"summary": "A ticket keyed by a Jira issue key"},
		},
	})

	var trk tracker.Tracker = NewClient(tr, baseURL, selectOpts)

	got, err := trk.FetchTicket("PROJ-7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "PROJ-7" {
		t.Errorf("Identifier = %q, want PROJ-7", got.Identifier)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/rest/api/2/issue/PROJ-7?fields=summary,description,subtasks" {
		t.Errorf("expected one fetch keyed by the opaque PROJ-7 Key, got %+v", *calls)
	}
}

// A finding with no ticket of its own (a PR loop's) files into the configured
// project, the container every ticket the adapter fetches already names.
func TestFindingsTeamIsTheConfiguredProject(t *testing.T) {
	tr, calls := fakeTransport(nil)
	var trk tracker.Tracker = NewClient(tr, baseURL, selectOpts)

	got, err := trk.FindingsTeam()
	if err != nil || got != "PROJ" {
		t.Errorf("FindingsTeam() = %q, %v; want PROJ", got, err)
	}
	if len(*calls) != 0 {
		t.Errorf("the project is configured; want no API call, got %+v", *calls)
	}
}
