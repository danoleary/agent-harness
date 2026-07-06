package github

import (
	"testing"

	"github.com/beherd/agent-harness/internal/tracker"
)

// TestClientSatisfiesTrackerPort pins that the GitHub adapter is a drop-in behind
// the host-side Tracker port: driving *Client through the interface (not the
// concrete type) proves it covers fetch / select+claim+release / file+dedup, and
// that an opaque #number Key round-trips — the harness never assumes a BEH- shape.
func TestClientSatisfiesTrackerPort(t *testing.T) {
	tr, calls := fakeTransport(map[string]any{
		"GET /repos/acme/widgets/issues/7": map[string]any{
			"number":   7,
			"title":    "A ticket keyed by a bare issue number",
			"html_url": "https://github.com/acme/widgets/issues/7",
		},
	})

	var trk tracker.Tracker = NewClient(tr, "acme", "widgets", readyLabels)

	got, err := trk.FetchTicket("#7")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got.Identifier != "#7" {
		t.Errorf("Identifier = %q, want #7", got.Identifier)
	}
	if len(*calls) != 1 || (*calls)[0].path != "/repos/acme/widgets/issues/7" {
		t.Errorf("expected one fetch keyed by the opaque #7 Key, got %+v", *calls)
	}
}
