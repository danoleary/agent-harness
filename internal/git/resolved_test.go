package git

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeSrc seeds a temp source root with one file holding the given content —
// the minimal stand-in for the `web/src` tree the advisory greps.
func writeSrc(t *testing.T, content string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "useLiveKitConnection.ts"), []byte(content), 0o644); err != nil {
		t.Fatalf("seed: %v", err)
	}
	return root
}

// The whole point of BEH-544: a ticket that cites a symbol which no longer
// exists in source (its dead-code cleanup already merged, here under a sibling
// ticket) gets an advisory naming the missing symbol, so the session isn't spent
// confirming a no-op.
func TestResolvedAdvisoryWarnsWhenCitedSymbolAbsent(t *testing.T) {
	root := writeSrc(t, "export function useLiveKitConnection() {}\n")

	msg := ResolvedAdvisory(root, "BEH-315", "delete the dead `onParticipantConnected` prop")
	if msg == "" {
		t.Fatal("expected an advisory for a cited symbol absent from source")
	}
	if !strings.Contains(msg, "onParticipantConnected") {
		t.Fatalf("advisory should name the missing symbol, got: %s", msg)
	}
	if !strings.Contains(msg, "BEH-315") {
		t.Fatalf("advisory should name the ticket, got: %s", msg)
	}
}

// The guard must stay quiet when the cited code is still present — a ticket
// whose premise holds is real work and must dispatch without noise.
func TestResolvedAdvisorySilentWhenCitedSymbolPresent(t *testing.T) {
	root := writeSrc(t, "function useLiveKitConnection() { onParticipantConnected() }\n")

	if msg := ResolvedAdvisory(root, "BEH-315", "wire up `onParticipantConnected`"); msg != "" {
		t.Fatalf("present symbol must not warn, got: %s", msg)
	}
}

// The advisory must fail QUIET: a missing srcRoot (or any grep error short of a
// clean "no match") is ambiguous, so symbolPresent treats it as present and the
// guard stays silent — it must never imply a ticket is resolved off a broken
// read. Without this the whole feature's safety asymmetry inverts: a refactor
// that defaulted to "absent" would emit false advisories on every dispatch.
func TestResolvedAdvisorySilentWhenSrcRootMissing(t *testing.T) {
	if msg := ResolvedAdvisory(filepath.Join(t.TempDir(), "does-not-exist"), "BEH-315", "delete the dead `onParticipantConnected` prop"); msg != "" {
		t.Fatalf("a missing srcRoot must fail quiet (no advisory), got: %s", msg)
	}
}

// A ticket that cites no code symbols (a pure prose/process ticket) yields no
// advisory — nothing to check, nothing to say.
func TestResolvedAdvisorySilentWhenNoSymbolsCited(t *testing.T) {
	root := writeSrc(t, "anything")

	if msg := ResolvedAdvisory(root, "BEH-544", "tidy up the onboarding copy"); msg != "" {
		t.Fatalf("no cited symbols must not warn, got: %s", msg)
	}
}

// BEH-544: a ticket that points at dead code names the symbol to fix in a
// backtick span (here `onParticipantConnected`). The advisory guard extracts
// those code symbols so it can later check whether they still exist in source.
func TestExtractCitedSymbolsPullsCamelCaseIdentifier(t *testing.T) {
	got := extractCitedSymbols("delete the dead `onParticipantConnected` prop")
	if !contains(got, "onParticipantConnected") {
		t.Fatalf("expected onParticipantConnected to be extracted, got %v", got)
	}
}

// The extractor must be precise: only lower-camelCase identifiers inside
// backticks, never the noise tokens a finding quotes alongside them — plain
// words (`grep`), commit SHAs (`aab6cc98f`), ticket keys (`BEH-435`) — or it
// would chase symbols that were never code and warn on healthy tickets. Dotted
// member access splits into its camelCase segments so each is grep-checkable.
func TestExtractCitedSymbolsIsPreciseAcrossTicketNoise(t *testing.T) {
	desc := "the dead `onParticipantConnected` prop, the `syncLog.participantJoined` entry " +
		"(`useLiveKitConnection.ts:39-42, 88, 181-202`) landed in `aab6cc98f` (`BEH-435`); a `grep` returned zero hits"
	got := extractCitedSymbols(desc)

	for _, want := range []string{"onParticipantConnected", "syncLog", "participantJoined", "useLiveKitConnection"} {
		if !contains(got, want) {
			t.Errorf("expected %q to be extracted, got %v", want, got)
		}
	}
	for _, noise := range []string{"grep", "aab6cc98f", "BEH-435", "ts", "BEH"} {
		if contains(got, noise) {
			t.Errorf("noise token %q must not be extracted, got %v", noise, got)
		}
	}
}

func contains(xs []string, want string) bool {
	for _, x := range xs {
		if x == want {
			return true
		}
	}
	return false
}
