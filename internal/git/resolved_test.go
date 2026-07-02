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

// A PascalCase component name (`GiphyGrid`, `CallTool`) is a code symbol too,
// and the advisory must round-trip it whole. The regression: a lower-camelCase
// extractor anchored on a leading *lowercase* letter starts matching at the
// first inner lowercase char, swallowing the leading capital — `GiphyGrid`
// surfaced as `iphyGrid`, a token that exists nowhere in source, so the advisory
// both mis-named the symbol and would self-confirm a false absence on a verbatim
// grep. Extraction must keep the leading `G`.
func TestExtractCitedSymbolsKeepsPascalCaseLeadingCapital(t *testing.T) {
	got := extractCitedSymbols("the lazy `GiphyGrid` never renders")
	if !contains(got, "GiphyGrid") {
		t.Fatalf("expected PascalCase GiphyGrid extracted whole, got %v", got)
	}
	if contains(got, "iphyGrid") {
		t.Fatalf("leading capital was stripped — got truncated iphyGrid in %v", got)
	}
}

// End-to-end: the emitted advisory must carry the exact, untruncated symbol so an
// agent can grep it verbatim and trust the result. A source tree missing the
// cited PascalCase symbol must warn naming `GiphyGrid`, never `iphyGrid`.
func TestResolvedAdvisoryNamesPascalCaseSymbolUntruncated(t *testing.T) {
	root := writeSrc(t, "export function useLiveKitConnection() {}\n")

	msg := ResolvedAdvisory(root, "BEH-318", "the lazy `GiphyGrid` never renders")
	if !strings.Contains(msg, "GiphyGrid") {
		t.Fatalf("advisory must name the untruncated symbol GiphyGrid, got: %s", msg)
	}
}

// BEH-629 (prose word): a git-trailer keyword like `Fixes` (from a `Fixes HERD-2T`
// line the ticket quotes) is English, not a code identifier — it is mixed-case so
// it slips past isMixedCaseIdentifier, but it must never be extracted as a symbol
// to grep. It was one of the three false positives that made BEH-626 look "already
// resolved — recommend close".
func TestExtractCitedSymbolsDropsGitTrailerKeyword(t *testing.T) {
	got := extractCitedSymbols("issues don't auto-close without a `Fixes HERD-2T`")
	if contains(got, "Fixes") {
		t.Fatalf("git-trailer keyword Fixes must not be extracted as a symbol, got %v", got)
	}
}

// BEH-629 (third-party internal): a Sentry-package symbol like
// `sentryFunctionMiddlewareHandler`, referenced only for context, is never
// expected in repo source — extracting it and finding it absent falsely reads as
// "already resolved". A symbol whose leading camelCase word is a known vendor
// (sentry, livekit, …) must be dropped. Repo-local identifiers that merely start
// with those letters (`sentryish`, where the vendor word isn't a full segment)
// must survive.
func TestExtractCitedSymbolsDropsThirdPartyVendorSymbol(t *testing.T) {
	got := extractCitedSymbols("captured by the `sentryFunctionMiddlewareHandler`")
	if contains(got, "sentryFunctionMiddlewareHandler") {
		t.Fatalf("third-party vendor symbol must not be extracted, got %v", got)
	}

	kept := extractCitedSymbols("our own `sentryishHelper` wrapper")
	if !contains(kept, "sentryishHelper") {
		t.Fatalf("a repo-local identifier that only shares the vendor's letters must survive, got %v", kept)
	}
}

// BEH-629 (to-be-created symbol): a symbol named right after an add/introduce
// verb — `Add a `beforeSend“ — is one the ticket exists to CREATE, so its absence
// is expected, not a "resolved" signal. Classification must sort it into the
// proposed bucket, not expected. A symbol cited in a plain descriptive context
// (`notFound`, further along the same sentence, no adjacent add-verb) stays
// expected.
func TestClassifyCitedSymbolsSortsAddContextAsProposed(t *testing.T) {
	c := classifyCitedSymbols("Add a `beforeSend` guard that drops a TanStack `notFound`")
	if !contains(c.proposed, "beforeSend") {
		t.Fatalf("beforeSend follows an add verb — expected in proposed bucket, got proposed=%v expected=%v", c.proposed, c.expected)
	}
	if contains(c.expected, "beforeSend") {
		t.Fatalf("beforeSend must not be in the expected bucket, got %v", c.expected)
	}
	if !contains(c.expected, "notFound") {
		t.Fatalf("notFound is descriptive, not add-context — expected in expected bucket, got expected=%v proposed=%v", c.expected, c.proposed)
	}
}

// BEH-629 (verdict downgrade): when the ONLY missing symbols are ones the ticket
// proposes to create (add-context), their absence is expected for new work. The
// advisory must NOT claim the ticket is "already resolved" nor "recommend close" —
// that dangerous verdict could get a valid, unstarted ticket wrongly closed. It
// downgrades to a soft "verify premise" note that still names the symbol.
func TestResolvedAdvisoryDowngradesForProposedSymbols(t *testing.T) {
	root := writeSrc(t, "export function useLiveKitConnection() {}\n")

	msg := ResolvedAdvisory(root, "BEH-626", "Add a `beforeSend` guard")
	if msg == "" {
		t.Fatal("expected a soft advisory for a to-be-created symbol, got none")
	}
	if strings.Contains(msg, "recommend close") || strings.Contains(msg, "already resolved") {
		t.Fatalf("a to-be-created symbol must not read as already resolved / recommend close, got: %s", msg)
	}
	if !strings.Contains(msg, "beforeSend") {
		t.Fatalf("advisory should still name the proposed symbol, got: %s", msg)
	}
}

// BEH-629 (strong verdict preserved): a genuinely-missing expected symbol still
// gets the strong "recommend close" verdict even when a proposed symbol is missing
// alongside it — the expected-absent signal is the real "already resolved" case.
func TestResolvedAdvisoryKeepsStrongVerdictForMissingExpectedSymbol(t *testing.T) {
	root := writeSrc(t, "export function useLiveKitConnection() {}\n")

	msg := ResolvedAdvisory(root, "BEH-315", "delete the dead `onParticipantConnected`; then add a `beforeSend`")
	if !strings.Contains(msg, "recommend close") {
		t.Fatalf("a missing expected symbol must keep the strong recommend-close verdict, got: %s", msg)
	}
	if !strings.Contains(msg, "onParticipantConnected") {
		t.Fatalf("advisory should name the missing expected symbol, got: %s", msg)
	}
}

// BEH-629 regression, grounded in the real BEH-626 body: the three tokens that
// made a valid, unstarted ticket read as "already resolved — recommend close" —
// `sentryFunctionMiddlewareHandler` (third-party internal), `beforeSend` (the hook
// the ticket exists to add), and `Fixes` (prose from a `Fixes HERD-2T` line) —
// must no longer drive that verdict. The genuinely-present symbols the ticket
// cites (`notFound`, `captureException`, `isNotFound`) are seeded, so nothing
// expected is missing and the advisory never recommends closing the ticket.
func TestResolvedAdvisoryNoFalseRecommendCloseOnBeh626Body(t *testing.T) {
	root := writeSrc(t, "notFound(); captureException(err); if (isNotFound(x)) {}\n")

	desc := "`notFound()` is captured by the `sentryFunctionMiddlewareHandler`, which calls `captureException`.\n" +
		"Add a `beforeSend` guard that drops a TanStack `notFound` (`isNotFound: true`).\n" +
		"Notes: issues don't auto-close without a `Fixes HERD-2T`."

	msg := ResolvedAdvisory(root, "BEH-626", desc)
	if strings.Contains(msg, "recommend close") || strings.Contains(msg, "already resolved") {
		t.Fatalf("valid ticket must not read as already resolved / recommend close, got: %s", msg)
	}
	for _, falsePos := range []string{"sentryFunctionMiddlewareHandler", "Fixes"} {
		if strings.Contains(msg, falsePos) {
			t.Fatalf("filtered token %q must not appear in the advisory, got: %s", falsePos, msg)
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
