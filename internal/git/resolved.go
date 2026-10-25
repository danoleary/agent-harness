package git

import (
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
)

// codeSpans returns the inline-code spans in ticket markdown — the only place the
// harness trusts a token to be a literal code reference rather than prose — as
// [spanStart, innerStart, innerEnd] offsets. Restricting extraction to code spans
// is what keeps the advisory guard quiet: an English sentence that merely happens
// to contain a camelCase word is never scanned.
//
// A span opens on a run of N backticks and closes on the next run of exactly N,
// as in CommonMark; a run with no partner is literal text. Pairing single
// backticks blindly (the old "`([^`]+)`" pattern) let one odd backtick — a fence,
// or a double-backtick span quoting a backtick — shift every later pair by
// one, so the prose BETWEEN spans was read as code (#66). Callers strip fenced
// blocks first (stripFencedCode).
func codeSpans(s string) [][3]int {
	var spans [][3]int
	for i := 0; i < len(s); {
		if s[i] != '`' {
			i++
			continue
		}
		n := backtickRun(s, i)
		end := -1
		for j := i + n; j < len(s); {
			if s[j] != '`' {
				j++
				continue
			}
			m := backtickRun(s, j)
			if m == n {
				end = j
				break
			}
			j += m
		}
		if end < 0 {
			i += n
			continue
		}
		spans = append(spans, [3]int{i, i + n, end})
		i = end + n
	}
	return spans
}

// backtickRun returns the length of the run of backticks starting at s[i].
func backtickRun(s string, i int) int {
	n := 0
	for i+n < len(s) && s[i+n] == '`' {
		n++
	}
	return n
}

// stripFencedCode blanks every fenced code block (``` or ~~~, three or more,
// indented at most three spaces) out of ticket markdown, leaving an empty line in
// its place. A fence holds a code sample or a log, not symbols the ticket expects
// in source, and its backticks would otherwise be paired as inline spans (#66).
// A fence closes on a line of the same character at least as long as the opener;
// an unclosed fence runs to the end of the body, as in CommonMark.
func stripFencedCode(s string) string {
	var out []string
	var fence string
	for _, line := range strings.Split(s, "\n") {
		marker := fenceMarker(line)
		switch {
		case fence == "" && marker != "":
			fence = marker
			out = append(out, "")
		case fence == "":
			out = append(out, line)
		case marker != "" && marker[0] == fence[0] && len(marker) >= len(fence) &&
			strings.TrimSpace(strings.TrimLeft(line, " ")[len(marker):]) == "":
			fence = ""
		}
	}
	return strings.Join(out, "\n")
}

// fenceMarker returns the run of three or more backticks or tildes that opens a
// fence line, or "" when the line is not a fence. As in CommonMark, a backtick run
// followed by more backticks on the same line (```x``` is the syntax) is an inline
// span, not a fence — treating it as one would swallow the rest of the body.
func fenceMarker(line string) string {
	trimmed := strings.TrimLeft(line, " ")
	if len(line)-len(trimmed) > 3 || len(trimmed) < 3 || (trimmed[0] != '`' && trimmed[0] != '~') {
		return ""
	}
	n := 0
	for n < len(trimmed) && trimmed[n] == trimmed[0] {
		n++
	}
	if n < 3 || (trimmed[0] == '`' && strings.ContainsRune(trimmed[n:], '`')) {
		return ""
	}
	return trimmed[:n]
}

// wordIdentifier matches a whole letter-led word token (anchored at a word
// boundary by RE2's greedy left-to-right scan, so it never starts mid-word). The
// isCodeIdentifier precision filter is applied in Go afterwards rather than baked into
// the pattern: anchoring it on a leading *lowercase* letter was the BEH-557 bug —
// a PascalCase identifier like `GiphyGrid` then matched starting at its first
// inner lowercase char (`iphyGrid`), silently dropping the leading capital.
var wordIdentifier = regexp.MustCompile(`[A-Za-z][A-Za-z0-9]*`)

// isCodeIdentifier is the precision filter: a code symbol the advisory will grep
// for must carry both a lowercase and an uppercase letter, and either an inner
// capital or a digit. That admits lower-camelCase (`syncLog`), PascalCase with two
// humps (`GiphyGrid`) and digit-bearing names (`Base64`) while excluding the noise
// a finding quotes alongside them — plain words (`grep`, `delete`, `ts`), commit
// SHAs (`aab6cc98f`), all-caps ticket keys (`BEH-435`), and a single capitalised
// English word (`Notes`, `Fixes`), whose one capital only starts a sentence or
// heading (#66). Underscores never reach here: wordIdentifier splits on them.
func isCodeIdentifier(s string) bool {
	var hasLower, hasUpper, innerUpper, hasDigit bool
	for i, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			hasLower = true
		case r >= 'A' && r <= 'Z':
			hasUpper = true
			innerUpper = innerUpper || i > 0
		case r >= '0' && r <= '9':
			hasDigit = true
		}
	}
	return hasLower && hasUpper && (innerUpper || hasDigit)
}

// extractCitedSymbols returns the distinct code identifiers a ticket description
// names inside inline-code spans — lower-camelCase and PascalCase alike —
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
//
// qualifier records, for a symbol cited package-qualified (`tea.Model`), the
// lowercase package name in front of it, so the advisory can drop the symbol
// when that package isn't one the repo owns (#69).
type citedSymbols struct {
	expected  []string
	proposed  []string
	qualifier map[string]string
}

// qualifiedChain matches a dotted selector inside a backtick span —
// `tea.Model`, `pgx.Conn.Close`, `syncLog.participantJoined`. Only a chain
// whose head is shaped like a package name (all lowercase) is treated as
// package-qualified; a mixed-case head is a local value, not a package.
var qualifiedChain = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_]*(?:\.[A-Za-z_][A-Za-z0-9_]*)+`)

// isPackageName reports whether a selector head is shaped like a Go package
// name: lowercase letters and digits, letter-led.
func isPackageName(s string) bool {
	for i, r := range s {
		if !(r >= 'a' && r <= 'z' || i > 0 && r >= '0' && r <= '9') {
			return false
		}
	}
	return s != ""
}

// qualifiedRange is the byte range of a package-qualified selector tail within
// a span, and the package that qualifies it.
type qualifiedRange struct {
	start, end int
	pkg        string
}

// spanQualifiers finds a span's package-qualified selector tails: in
// `*tea.Program` the range of `Program` maps to `tea`.
func spanQualifiers(inner string) []qualifiedRange {
	var out []qualifiedRange
	for _, loc := range qualifiedChain.FindAllStringIndex(inner, -1) {
		chain := inner[loc[0]:loc[1]]
		dot := strings.IndexByte(chain, '.')
		if head := chain[:dot]; isPackageName(head) {
			out = append(out, qualifiedRange{start: loc[0] + dot + 1, end: loc[1], pkg: head})
		}
	}
	return out
}

// classifyCitedSymbols walks each inline-code span outside fenced blocks, drops
// known third-party package symbols (BEH-629), and sorts each surviving code
// identifier into expected vs proposed by whether an add/introduce/extraction verb
// sits immediately before its span. A symbol cited behind a package-shaped
// qualifier has that qualifier recorded (#69). Order-preserving and de-duplicated
// across buckets.
func classifyCitedSymbols(description string) citedSymbols {
	c := citedSymbols{qualifier: map[string]string{}}
	seen := map[string]bool{}
	description = stripFencedCode(description)
	for _, span := range codeSpans(description) {
		inner := description[span[1]:span[2]]
		proposed := inAddContext(description[:span[0]])
		qualified := spanQualifiers(inner)
		for _, w := range wordIdentifier.FindAllStringIndex(inner, -1) {
			sym := inner[w[0]:w[1]]
			if !isCodeIdentifier(sym) || hasVendorPrefix(sym) {
				continue
			}
			if seen[sym] {
				continue
			}
			seen[sym] = true
			for _, q := range qualified {
				if w[0] >= q.start && w[1] <= q.end {
					c.qualifier[sym] = q.pkg
				}
			}
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

// localPackage reports whether any declared source root holds a directory named
// pkg (the root itself included) — the shape of a package the repo owns, so a
// symbol qualified by it is repo code. A qualifier with no such directory names
// a dependency or the standard library (`tea`, `http`), whose symbols can never
// appear under the source roots, so the advisory drops them (#69). Vendored and
// installed dependency trees don't count as local. An unreadable root counts as
// "not local", which only ever drops a symbol (the safe direction).
func localPackage(srcRoots []string, pkg string) bool {
	found := false
	for _, root := range srcRoots {
		_ = filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
			if err != nil || !d.IsDir() {
				return nil
			}
			name := d.Name()
			if path != root && (name == "vendor" || name == "node_modules" || strings.HasPrefix(name, ".")) {
				return filepath.SkipDir
			}
			if name == pkg {
				found = true
				return filepath.SkipAll
			}
			return nil
		})
		if found {
			return true
		}
	}
	return false
}

// dependencyModules returns the module paths the go.mod governing the source
// roots requires — found by walking up from each root to the nearest go.mod. A
// root with no go.mod above it (a non-Go Consumer) contributes nothing.
func dependencyModules(srcRoots []string) []string {
	var mods []string
	seen := map[string]bool{}
	for _, root := range srcRoots {
		for dir := filepath.Clean(root); ; dir = filepath.Dir(dir) {
			data, err := os.ReadFile(filepath.Join(dir, "go.mod"))
			if err == nil {
				for _, m := range goModRequires(string(data)) {
					if !seen[m] {
						seen[m] = true
						mods = append(mods, m)
					}
				}
				break
			}
			if filepath.Dir(dir) == dir {
				break
			}
		}
	}
	return mods
}

// goModRequires extracts the module paths from a go.mod's require directives,
// both the single-line and the parenthesized block form.
func goModRequires(gomod string) []string {
	var mods []string
	inBlock := false
	for _, line := range strings.Split(gomod, "\n") {
		if i := strings.Index(line, "//"); i >= 0 {
			line = line[:i]
		}
		fields := strings.Fields(line)
		switch {
		case len(fields) == 0:
		case inBlock && fields[0] == ")":
			inBlock = false
		case inBlock:
			mods = append(mods, fields[0])
		case fields[0] == "require" && len(fields) > 1 && fields[1] == "(":
			inBlock = true
		case fields[0] == "require" && len(fields) > 1:
			mods = append(mods, fields[1])
		}
	}
	return mods
}

// namedDependency returns the first go.mod dependency whose module path the
// ticket text names, or "" when it names none.
func namedDependency(srcRoots []string, description string) string {
	for _, m := range dependencyModules(srcRoots) {
		if strings.Contains(description, m) {
			return m
		}
	}
	return ""
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

// neverInHistory reports whether a symbol is absent from the git history of
// every source root — added and removed in no commit, so it never existed and
// can't have been deleted by a fix that already landed (#116). The pickaxe
// (`git log -S`) matches a substring, not a whole word, so a longer name that
// once held it (`NewThingy` for `NewThing`) counts as history; that errs toward
// keeping the strong verdict, the behaviour before this check. A root that isn't
// in a git repo, or any git error, also counts as history for the same reason.
func neverInHistory(srcRoots []string, symbol string) bool {
	for _, root := range srcRoots {
		out, err := exec.Command("git", "-C", root, "log", "-S"+symbol, "--oneline", "-1", "--", ".").Output()
		if err != nil || strings.TrimSpace(string(out)) != "" {
			return false
		}
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
//
// Dependency symbols are not repo code (#69). A symbol cited behind a package
// qualifier the repo doesn't own (`tea.Model`) is dropped before the check: it
// can never appear under the source roots. And when the ticket names one of the
// go.mod dependencies by module path, a missing bare symbol may be that
// dependency's, so the verdict downgrades to the soft "verify premise" note.
//
// The add-verb classifier can't catch every wording a ticket uses for a symbol
// it means to create ("one composition root, `X`, that both callers use"), so
// a missing expected symbol is also checked against git history (#116): one
// that never appeared in any commit was never removed, so it joins the
// proposed symbols under the soft note. Only a symbol that existed once and is
// now gone keeps "recommend close".
func ResolvedAdvisory(srcRoots []string, identifier, description string) string {
	// No declared source root means no signal to read: skip the advisory rather
	// than scan a guessed path. Omitting `source_roots` disables the check, the
	// safe direction — a missed nudge costs a session, a wrong one risks closing a
	// valid ticket (BEH-641).
	if len(srcRoots) == 0 {
		return ""
	}
	cited := classifyCitedSymbols(description)
	local := map[string]bool{}
	missing := func(syms []string) []string {
		var out []string
		for _, sym := range syms {
			if pkg := cited.qualifier[sym]; pkg != "" {
				if _, ok := local[pkg]; !ok {
					local[pkg] = localPackage(srcRoots, pkg)
				}
				if !local[pkg] {
					continue
				}
			}
			if !symbolPresent(srcRoots, sym) {
				out = append(out, sym)
			}
		}
		return out
	}
	// A missing "expected" symbol that never appeared in history was never
	// removed, so it can't be the BEH-544 signal: whatever the wording around it,
	// it is one the ticket proposes to create (#116).
	var missingExpected, neverExisted []string
	for _, sym := range missing(cited.expected) {
		if neverInHistory(srcRoots, sym) {
			neverExisted = append(neverExisted, sym)
		} else {
			missingExpected = append(missingExpected, sym)
		}
	}
	if len(missingExpected) > 0 {
		if dep := namedDependency(srcRoots, description); dep != "" {
			return fmt.Sprintf(
				"ℹ %s cites symbol(s) not in source: %s — the ticket names the dependency %s, so these may be its symbols rather than repo code; not a sign the ticket is resolved. Verify the premise, then implement.",
				identifier, strings.Join(missingExpected, ", "), dep,
			)
		}
		return fmt.Sprintf(
			"⚠ %s cites symbol(s) absent from source: %s — likely already resolved (possibly by a sibling ticket's merge). Verify the premise still holds before implementing; recommend close if the work has already landed.",
			identifier, strings.Join(missingExpected, ", "),
		)
	}
	if missingProposed := append(neverExisted, missing(cited.proposed)...); len(missingProposed) > 0 {
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
