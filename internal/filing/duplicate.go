package filing

import (
	"strings"

	"github.com/danoleary/agent-harness/internal/findings"
)

// duplicateOverlap is the Jaccard similarity of normalized tokens at or above
// which two finding keys (or two titles) read as the same failure class under
// drifted wording (#65). Set so a slug that only swaps one qualifier
// ("gate-rerun-…" vs "host-gate-…") matches while two classes sharing a generic
// word ("sandbox-…") do not.
const duplicateOverlap = 0.6

// fillerTokens carry no failure-class meaning, so they neither make two
// findings look alike nor dilute a real match.
var fillerTokens = map[string]bool{
	"a": true, "an": true, "the": true, "as": true, "of": true, "to": true, "in": true,
	"on": true, "for": true, "is": true, "are": true, "and": true, "or": true, "by": true,
	"with": true, "it": true, "be": true, "when": true, "from": true, "its": true,
}

// probablyDuplicate is the harness's backstop for key drift (#65): it reports
// whether f closely matches an open finding with the given key and title, by
// normalized token overlap of the keys or of the titles. It is cheap and local —
// unlike the semantic matcher it needs no model — and it only FLAGS a filing as
// a probable duplicate; it never suppresses one, since token overlap is a
// heuristic and a missed finding is worse than a duplicate.
func probablyDuplicate(f findings.Finding, key, title string) bool {
	return overlaps(f.Key, key) || overlaps(f.Title, title)
}

// overlaps reports whether a and b share at least duplicateOverlap of their
// meaningful tokens. Either side empty never overlaps.
func overlaps(a, b string) bool {
	ta, tb := tokens(a), tokens(b)
	if len(ta) == 0 || len(tb) == 0 {
		return false
	}
	shared := 0
	for t := range ta {
		if tb[t] {
			shared++
		}
	}
	union := len(ta) + len(tb) - shared
	return float64(shared)/float64(union) >= duplicateOverlap
}

// tokens is the normalized token set of s: lowercased alphanumeric runs, minus
// filler words.
func tokens(s string) map[string]bool {
	out := map[string]bool{}
	for _, t := range strings.Split(slugify(s), "-") {
		if t != "" && !fillerTokens[t] {
			out[t] = true
		}
	}
	return out
}
