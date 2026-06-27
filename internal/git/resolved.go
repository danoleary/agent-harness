package git

import (
	"fmt"
	"os/exec"
	"regexp"
	"strings"
)

// backtickSpan matches an inline-code span in the ticket markdown — the only
// place the harness trusts a token to be a literal code reference rather than
// prose. Restricting extraction to backtick spans is what keeps the advisory
// guard quiet: an English sentence that merely happens to contain a camelCase
// word is never scanned.
var backtickSpan = regexp.MustCompile("`([^`]+)`")

// wordIdentifier matches a whole letter-led word token (anchored at a word
// boundary by RE2's greedy left-to-right scan, so it never starts mid-word). The
// mixed-case precision filter is applied in Go afterwards rather than baked into
// the pattern: anchoring it on a leading *lowercase* letter was the BEH-557 bug —
// a PascalCase identifier like `GiphyGrid` then matched starting at its first
// inner lowercase char (`iphyGrid`), silently dropping the leading capital.
var wordIdentifier = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]*`)

// isMixedCaseIdentifier is the precision filter: a code symbol the advisory will
// grep for must carry both a lowercase and an uppercase letter. That admits both
// lower-camelCase (`syncLog`) and PascalCase (`GiphyGrid`) while excluding the
// noise a finding quotes alongside them — plain words (`grep`, `delete`, `ts`),
// commit SHAs (`aab6cc98f`), and all-caps ticket keys (`BEH-435`), none of which
// a source grep should chase.
func isMixedCaseIdentifier(s string) bool {
	var hasLower, hasUpper bool
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
		}
	}
	return hasLower && hasUpper
}

// extractCitedSymbols returns the distinct mixed-case code identifiers a ticket
// description names inside backtick spans — lower-camelCase and PascalCase alike —
// the symbols whose continued existence the advisory guard checks.
// Order-preserving and de-duplicated.
func extractCitedSymbols(description string) []string {
	var out []string
	seen := map[string]bool{}
	for _, span := range backtickSpan.FindAllStringSubmatch(description, -1) {
		for _, sym := range wordIdentifier.FindAllString(span[1], -1) {
			if !isMixedCaseIdentifier(sym) {
				continue
			}
			if !seen[sym] {
				seen[sym] = true
				out = append(out, sym)
			}
		}
	}
	return out
}

// symbolPresent reports whether srcRoot contains a whole-word occurrence of the
// symbol. `-w` is the precision knob: `syncLog` must not match `syncLogger`, so
// a member whose container survives but whose own definition was deleted still
// reads as absent. `-F` treats the symbol as a literal (camelCase identifiers
// carry no regex metachars, but it costs nothing and documents intent). A
// missing srcRoot or a grep that errors is treated as "present" so the advisory
// fails QUIET — it must never imply a ticket is resolved off a broken read.
func symbolPresent(srcRoot, symbol string) bool {
	err := exec.Command("grep", "-rwqF", "--", symbol, srcRoot).Run()
	if err == nil {
		return true
	}
	// grep exits 1 only for "no match found" — that, and only that, is a true
	// absence. Any other error (exit ≥2: unreadable path, bad invocation) is
	// ambiguous, so fail quiet by reporting the symbol present.
	if ee, ok := err.(*exec.ExitError); ok && ee.ExitCode() == 1 {
		return false
	}
	return true
}

// ResolvedAdvisory returns a one-line warning when a ticket cites code symbols
// that no longer exist in srcRoot — the BEH-544 signal that the work likely
// already merged (often under a *sibling* ticket, which TicketAlreadyOnMain's
// own-key scan can't catch). It returns "" when the ticket cites no symbols or
// every cited symbol is still present.
//
// This is ADVISORY, never a skip: unlike TicketAlreadyOnMain (a high-confidence
// exact-key match in a merge subject), the symbol signal is heuristic — a cited
// symbol can be absent because the ticket asks to *create* it, or because the
// extractor mis-read prose. Per the maintainer's stance a false skip that drops
// real work is far worse than a wasted session, so the host only logs this and
// lets the in-session agent (steered by the prompt) make the final call.
func ResolvedAdvisory(srcRoot, identifier, description string) string {
	var missing []string
	for _, sym := range extractCitedSymbols(description) {
		if !symbolPresent(srcRoot, sym) {
			missing = append(missing, sym)
		}
	}
	if len(missing) == 0 {
		return ""
	}
	return fmt.Sprintf(
		"⚠ %s cites symbol(s) absent from source: %s — likely already resolved (possibly by a sibling ticket's merge). Verify the premise still holds before implementing; recommend close if the work has already landed.",
		identifier, strings.Join(missing, ", "),
	)
}
