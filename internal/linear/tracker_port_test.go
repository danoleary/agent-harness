package linear

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/tracker"
)

// TestClientSatisfiesTrackerPort pins the ADR-0010 refactor: the Linear client is
// one adapter behind the host-side Tracker port. Driving *Client through the
// interface (not the concrete type) proves the port covers what the harness needs
// and that Linear satisfies it — so a future Jira/GitHub adapter is a drop-in.
func TestClientSatisfiesTrackerPort(t *testing.T) {
	// An opaque, non-BEH Key must round-trip through the port: the harness treats
	// the identifier as a tracker-agnostic string, never a hardcoded BEH-NNN shape.
	tr, calls := transportReturning(t, map[string]any{
		"issue": map[string]any{
			"identifier": "PROJ-7",
			"title":      "A ticket keyed by an arbitrary tracker",
			"team":       map[string]any{"id": "team-uuid"},
		},
	})

	var trk tracker.Tracker = NewClient(tr, testOptions())

	got, err := trk.FetchTicket("PROJ-7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "PROJ-7" {
		t.Errorf("Identifier = %q, want PROJ-7", got.Identifier)
	}
	if len(*calls) != 1 || (*calls)[0].variables["id"] != "PROJ-7" {
		t.Errorf("expected one fetch keyed by the opaque Key PROJ-7, got %+v", *calls)
	}
}

// A finding with no ticket of its own (a PR loop's) files into the configured
// team: the create API takes the team's UUID, so the adapter resolves team_key to
// it (#174: herd's PR-1312 findings were dropped for want of one).
func TestFindingsTeamResolvesTheTeamKey(t *testing.T) {
	tr, calls := transportReturning(t, map[string]any{
		"teams": map[string]any{"nodes": []any{map[string]any{"id": "team-uuid"}}},
	})
	var trk tracker.Tracker = NewClient(tr, testOptions())

	got, err := trk.FindingsTeam()
	if err != nil || got != "team-uuid" {
		t.Errorf("FindingsTeam() = %q, %v; want team-uuid", got, err)
	}
	if len(*calls) != 1 || (*calls)[0].variables["key"] != "BEH" {
		t.Errorf("expected one lookup keyed by the team key BEH, got %+v", *calls)
	}
}

// A team key no Linear team carries is an error, not an empty team.
func TestFindingsTeamFailsOnAnUnknownTeamKey(t *testing.T) {
	tr, _ := transportReturning(t, map[string]any{"teams": map[string]any{"nodes": []any{}}})

	if got, err := NewClient(tr, testOptions()).FindingsTeam(); err == nil {
		t.Errorf("FindingsTeam() = %q, nil; want an error naming the unknown team key", got)
	}
}
