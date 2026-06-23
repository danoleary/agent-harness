package main

import "testing"

func TestParseArgsIdentifierIsUppercased(t *testing.T) {
	a, err := parseArgs([]string{"beh-528"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.identifier != "BEH-528" {
		t.Fatalf("identifier = %q, want BEH-528", a.identifier)
	}
}

// BEH-528: --force is the human-confirmation escape hatch for the already-merged
// dispatch guard — it lets an operator override a false positive (e.g. a key that
// only coincidentally appears in a follow-up commit) without editing the harness.
func TestParseArgsForceFlag(t *testing.T) {
	a, err := parseArgs([]string{"BEH-528", "--force"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !a.force {
		t.Fatalf("expected --force to set force=true")
	}
	if a.identifier != "BEH-528" {
		t.Fatalf("identifier should still parse alongside --force, got %q", a.identifier)
	}
}

// Absent the flag, the guard stays armed (force defaults off) — the safe default
// is to skip an already-merged ticket, not to dispatch it.
func TestParseArgsForceDefaultsOff(t *testing.T) {
	a, err := parseArgs([]string{"BEH-528"})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if a.force {
		t.Fatalf("force must default to false")
	}
}
