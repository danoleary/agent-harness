package git

import (
	"fmt"
	"os/exec"
	"regexp"
	"strconv"
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

// gitTrailerKeyword is the set of Git close-verb keywords a ticket quotes in a
// `Fixes HERD-2T` / `Closes …` / `Resolves …` line. They are mixed-case English
// (leading capital + lowercase tail), so isMixedCaseIdentifier admits them, yet
// they are never repo symbols — `Fixes` was one of the three false positives that
// made BEH-626 read as "already resolved — recommend close". Matched
// case-insensitively so `fixes`/`FIXES` are dropped too. Suppressing them only
// ever removes an advisory (the safe direction).
var gitTrailerKeyword = map[string]bool{
	"fix": true, "fixes": true, "fixed": true,
	"close": true, "closes": true, "closed": true,
	"resolve": true, "resolves": true, "resolved": true,
}

// isProseWord reports whether a mixed-case token is prose the extractor must not
// chase — currently the Git close-verb keywords above.
func isProseWord(sym string) bool {
	return gitTrailerKeyword[strings.ToLower(sym)]
}

// extractCitedSymbols returns the distinct mixed-case code identifiers a ticket
// description names inside backtick spans — lower-camelCase and PascalCase alike —
// the symbols whose continued existence the advisory guard checks (both the
// expected-present and to-be-created buckets, combined and order-preserving).
func extractCitedSymbols(description string) []string {
	c := classifyCitedSymbols(description)
	return append(append([]string{}, c.expected...), c.proposed...)
}

// citedSymbols partitions the code identifiers a ticket names into two classes
// whose absence from source means opposite things:
//   - expected: symbols the ticket implies already exist. Their absence is the
//     BEH-544 "likely already resolved" signal → strong "recommend close" verdict.
//   - proposed: symbols named in an add/introduce/extraction context — the ones
//     the ticket exists to CREATE. Their absence is expected for new work, never a
//     resolved signal, so they get a soft "verify premise" verdict (BEH-629/674).
type citedSymbols struct {
	expected []string
	proposed []string
}

// classifyCitedSymbols walks each backtick span, drops prose keywords and known
// third-party package symbols (BEH-629), and sorts each surviving mixed-case
// identifier into expected vs proposed by whether an add/introduce/extraction verb
// sits immediately before its span. Order-preserving and de-duplicated across buckets.
func classifyCitedSymbols(description string) citedSymbols {
	var c citedSymbols
	seen := map[string]bool{}
	for _, loc := range backtickSpan.FindAllStringSubmatchIndex(description, -1) {
		spanStart, inner := loc[0], description[loc[2]:loc[3]]
		proposed := inAddContext(description[:spanStart])
		for _, sym := range wordIdentifier.FindAllString(inner, -1) {
			if !isMixedCaseIdentifier(sym) || isProseWord(sym) || hasVendorPrefix(sym) {
				continue
			}
			if seen[sym] {
				continue
			}
			seen[sym] = true
			if proposed {
				c.proposed = append(c.proposed, sym)
			} else {
				c.expected = append(c.expected, sym)
			}
		}
	}
	return c
}

// vendorPrefix is the set of third-party package roots whose internal symbols a
// ticket cites only for context — never expected in repo source (BEH-629). Kept
// deliberately narrow to SDK roots that don't double as repo-local identifier
// stems (Supabase is intentionally absent: `supabaseAdmin` & friends are our own
// code). Dropping a match only ever suppresses an advisory (the safe direction).
var vendorPrefix = []string{"sentry", "livekit", "posthog", "mapbox"}

// hasVendorPrefix reports whether a symbol's leading camelCase word is a known
// third-party package root (BEH-629). The vendor word must be a whole leading
// segment — followed by an uppercase letter, a digit, or the end of the string —
// so `sentryFunctionMiddlewareHandler` (→ `sentry` + `Function…`) is dropped while
// `sentryishHelper` (the vendor letters bleed into a lowercase tail) is kept.
func hasVendorPrefix(sym string) bool {
	lower := strings.ToLower(sym)
	for _, v := range vendorPrefix {
		if !strings.HasPrefix(lower, v) {
			continue
		}
		rest := sym[len(v):]
		if rest == "" {
			return true
		}
		r := rest[0]
		if r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' {
			return true
		}
	}
	return false
}

// addVerb is the set of verbs that, sitting just before a backtick span, frame
// the symbol inside as one the ticket exists to CREATE — `Add a `beforeSend“,
// `introduce `foo“, `a new `bar“ (BEH-629) — plus the extraction/decomposition
// verbs of a refactoring ticket (BEH-674): "split the pane into a `NewsStoryViewer`",
// "collapse the ladder into `advanceStage`", "extract `foo`". For an extraction
// refactor the destination symbol's absence from source is the PRECONDITION for
// the work, not proof it landed — so it must sort into the proposed bucket exactly
// like an add-target, keeping the dangerous "recommend close" verdict off it. The
// `into` preposition is the reliable framing word: it sits immediately before the
// destination span in both "split … into a `X`" and "collapse … into a single `Y`",
// where the leading verb ("Split", "Collapse") often falls outside the word window.
var addVerb = map[string]bool{
	"add": true, "adds": true, "added": true,
	"introduce": true, "introduces": true, "introducing": true,
	"create": true, "creates": true, "creating": true,
	"new":   true,
	"split": true, "splits": true, "splitting": true,
	"extract": true, "extracts": true, "extracting": true,
	"collapse": true, "collapses": true, "collapsing": true,
	"decompose": true, "decomposes": true, "decomposing": true,
	"into": true,
}

// addContextWindow bounds how far back inAddContext looks: an add verb only
// frames the symbol as to-be-created if it sits within this many words of the
// span. A wider window would misfire — one bullet ("Add a `beforeSend` … drops a
// TanStack `notFound`") holds an already-present symbol many words after the same
// "Add", which must stay in the expected bucket.
const addContextWindow = 6

// inAddContext reports whether an add/introduce verb appears within the last few
// words of the text immediately preceding a backtick span. Word-based (not raw
// character offsets) so a truncated window boundary can't split a verb mid-token.
func inAddContext(preceding string) bool {
	words := strings.Fields(preceding)
	if lo := len(words) - addContextWindow; lo > 0 {
		words = words[lo:]
	}
	for _, w := range words {
		if addVerb[strings.ToLower(strings.Trim(w, ".,;:*/-()`\"'"))] {
			return true
		}
	}
	return false
}

// symbolPresent reports whether ANY declared source root contains a whole-word
// occurrence of the symbol. Any-root rather than all-roots keeps the fail-quiet
// contract: a symbol found anywhere in the Consumer's source is present, so a
// second root can only ever suppress the advisory, never manufacture one.
func symbolPresent(srcRoots []string, symbol string) bool {
	for _, root := range srcRoots {
		if symbolPresentIn(root, symbol) {
			return true
		}
	}
	return false
}

// symbolPresentIn reports whether one srcRoot contains a whole-word occurrence
// of the symbol. `-w` is the precision knob: `syncLog` must not match
// `syncLogger`, so a member whose container survives but whose own definition
// was deleted still reads as absent. `-F` treats the symbol as a literal
// (camelCase identifiers carry no regex metachars, but it costs nothing and
// documents intent). A missing srcRoot or a grep that errors is treated as
// "present" so the advisory fails QUIET — it must never imply a ticket is
// resolved off a broken read.
func symbolPresentIn(srcRoot, symbol string) bool {
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
//
// The verdict is graded (BEH-629). A missing *expected* symbol — one the ticket
// implies already exists — keeps the strong "recommend close" wording: that's the
// genuine "already resolved" signal. When the only missing symbols are *proposed*
// ones (named in an add/introduce context, the ones the ticket exists to create),
// their absence is expected for new work, so it downgrades to a soft "verify
// premise" note that never says "recommend close" — that dangerous verdict on a
// to-be-added symbol (`beforeSend` in BEH-626) is exactly backwards and could get
// a valid, unstarted ticket wrongly closed.
func ResolvedAdvisory(srcRoots []string, identifier, description string) string {
	// No declared source root means no signal to read: skip the advisory rather
	// than scan a guessed path. Omitting `source_roots` disables the check, the
	// safe direction — a missed nudge costs a session, a wrong one risks closing a
	// valid ticket (BEH-641).
	if len(srcRoots) == 0 {
		return ""
	}
	cited := classifyCitedSymbols(description)
	missing := func(syms []string) []string {
		var out []string
		for _, sym := range syms {
			if !symbolPresent(srcRoots, sym) {
				out = append(out, sym)
			}
		}
		return out
	}
	missingExpected := missing(cited.expected)
	if len(missingExpected) > 0 {
		return fmt.Sprintf(
			"⚠ %s cites symbol(s) absent from source: %s — likely already resolved (possibly by a sibling ticket's merge). Verify the premise still holds before implementing; recommend close if the work has already landed.",
			identifier, strings.Join(missingExpected, ", "),
		)
	}
	if missingProposed := missing(cited.proposed); len(missingProposed) > 0 {
		return fmt.Sprintf(
			"ℹ %s names symbol(s) it proposes to add that aren't in source yet: %s — this is expected for new work, not a sign the ticket is resolved. Verify the premise, then implement.",
			identifier, strings.Join(missingProposed, ", "),
		)
	}
	return ""
}

// ResumedBranchAdvisory returns a one-line warning when the ticket's OWN feature
// branch `feat/<slug>` already carries commit(s) ahead of origin/main whose
// subject references the ticket key — the BEH-554 signal that a *resumed*
// worktree's branch already holds a complete, un-merged fix for this exact
// ticket. It returns "" when the branch doesn't exist, has no commits ahead, or
// none of those commits reference the key.
//
// This is the third, distinct dispatch guard, keyed on the branch's own history:
//   - TicketAlreadyOnMain skips when the work merged TO origin/main (own key).
//   - ResolvedAdvisory warns when cited symbols vanished from source (often a
//     *sibling* ticket's merge).
//   - [Worktree.ResumedBranchAdvisory] warns when the fix lives on the SAME branch as
//     un-merged commits — the merge-base with main is stale, so a "contained in
//     main?" check (TicketAlreadyOnMain) sees nothing and the symbol check
//     (ResolvedAdvisory) stays quiet because the fix *added* code rather than
//     deleting any.
//
// ADVISORY, never a skip — same safety asymmetry as ResolvedAdvisory: a resumed
// branch can legitimately hold *incomplete* work (a recovery checkpoint commit, a
// half-finished slice), so the host only logs this and the prompt steers the
// in-session agent to verify-and-handoff rather than re-implement. Dropping the
// dispatch outright would risk discarding a genuinely unfinished ticket. Reads
// origin/main as-is (no fetch): a slightly stale ref can only over-report commits
// as "ahead", which at worst yields a verify-first nudge — never a false skip.
func (w Worktree) ResumedBranchAdvisory(identifier string) string {
	out, err := w.output("git", "-C", w.repo, "log", "--oneline", "-"+strconv.Itoa(mainHistoryLookback), "origin/main.."+w.branch)
	if err != nil {
		// Branch absent, no upstream, or any git error → nothing to advise on. Fail
		// quiet: an unreadable range must never imply work is already done.
		return ""
	}
	if !mainHistoryReferences(string(out), identifier) {
		return ""
	}
	branch := w.branch
	return fmt.Sprintf(
		"⚠ %s already has commit(s) on %s ahead of origin/main referencing it — a resumed worktree likely already holds a complete, un-merged fix. Before re-implementing, verify the work is done (`git log origin/main..%s` + the diff, run the gates) and prefer verify-and-handoff; recommend opening the PR / close if it's already fixed.",
		identifier, branch, branch,
	)
}
