package trackers

import (
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/tracker"
)

// New selects the tracker adapter by the config's kind (ADR-0010: "selected by
// .agent-harness/config"). Only Linear exists today; Jira/GitHub are BEH-637/638.
func TestNewSelectsLinearAdapter(t *testing.T) {
	var got tracker.Tracker
	got, err := New("linear", "lin_api_key")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got == nil {
		t.Fatal("expected a Tracker for kind=linear, got nil")
	}
}

// An unknown kind must fail loud, naming the kind — the harness refuses to launch
// against a tracker it has no adapter for rather than silently doing nothing.
func TestNewRejectsUnknownKind(t *testing.T) {
	_, err := New("bugzilla", "key")
	if err == nil {
		t.Fatal("expected an error for an unsupported kind, got nil")
	}
	if !strings.Contains(err.Error(), "bugzilla") {
		t.Errorf("error should name the unsupported kind, got: %v", err)
	}
}
