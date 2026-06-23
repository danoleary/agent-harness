package stages

import (
	"strings"
	"testing"
)

func TestParseArgsValidIdentifierUppercases(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"beh-527"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if got.Identifier != "BEH-527" {
		t.Errorf("Identifier = %q, want BEH-527 (lowercase input should uppercase)", got.Identifier)
	}
	if got.DryRun || got.Verbose {
		t.Errorf("flags = {DryRun:%v Verbose:%v}, want both false", got.DryRun, got.Verbose)
	}
}

func TestParseArgsFlags(t *testing.T) {
	got, err := ParseArgs("review", []string{"--verbose", "BEH-1", "--dry-run"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if got.Identifier != "BEH-1" {
		t.Errorf("Identifier = %q, want BEH-1", got.Identifier)
	}
	if !got.DryRun || !got.Verbose {
		t.Errorf("flags = {DryRun:%v Verbose:%v}, want both true", got.DryRun, got.Verbose)
	}
}

func TestParseArgsRejectsMissingIdentifierWithToolNameInUsage(t *testing.T) {
	_, err := ParseArgs("retrospective", []string{"--verbose"})
	if err == nil {
		t.Fatal("ParseArgs accepted argv with no ticket id, want error")
	}
	if want := "retrospective"; !strings.Contains(err.Error(), want) {
		t.Errorf("usage error %q does not name the tool %q", err.Error(), want)
	}
}

func TestParseArgsRejectsNonTicketToken(t *testing.T) {
	if _, err := ParseArgs("implementation", []string{"not-a-ticket"}); err == nil {
		t.Error("ParseArgs accepted a non-ticket token, want error")
	}
}

// BEH-528: --force is the human-confirmation escape hatch for the already-merged
// dispatch guard — it lets an operator override a false positive (a key that only
// coincidentally appears in a follow-up commit) without editing the harness. It
// parses alongside the identifier and defaults off so the guard stays armed.
func TestParseArgsForceFlag(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"BEH-528", "--force"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if !got.Force {
		t.Error("expected --force to set Force=true")
	}
	if got.Identifier != "BEH-528" {
		t.Errorf("Identifier should still parse alongside --force, got %q", got.Identifier)
	}
}

func TestParseArgsForceDefaultsOff(t *testing.T) {
	got, err := ParseArgs("implementation", []string{"BEH-528"})
	if err != nil {
		t.Fatalf("ParseArgs returned error: %v", err)
	}
	if got.Force {
		t.Error("Force must default to false")
	}
}
