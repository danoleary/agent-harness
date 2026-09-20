package findings

import (
	"fmt"
	"regexp"
	"strconv"
	"strings"
)

// The finding-key and occurrence markers are the harness's cross-adapter dedup
// protocol: machine-readable HTML comments embedded in a filed finding's body,
// invisible in rendered Markdown. A finding filed under one tracker has to read
// as the same finding under another, so the three adapters must agree on them
// byte for byte — which is why they live here, beside the dropbox shape they
// describe, rather than in any one adapter. Until this file they were three
// byte-identical private copies in internal/github, internal/jira and
// internal/linear, with only the comment wording to tell them apart; the
// "these are deliberately the same" invariant is now structural.

// keyRe matches the dedup marker `<!-- finding-key: <key> -->` the harness
// writes on filing and reads back on search, so dedup is an exact-key lookup
// rather than fuzzy text.
var keyRe = regexp.MustCompile(`<!--\s*finding-key:\s*(\S+)\s*-->`)

// ExtractKey pulls the dedup key out of a filed finding's body, or "" when the
// body carries no marker.
func ExtractKey(body string) string {
	m := keyRe.FindStringSubmatch(body)
	if m == nil {
		return ""
	}
	return m[1]
}

// KeyMarker renders the body marker for a dedup key.
func KeyMarker(key string) string {
	return "<!-- finding-key: " + key + " -->"
}

// occurrenceRe matches the occurrence-count marker a recurrence bump writes into
// a filed finding's body: `<!-- occurrences: <n> -->`. Like the finding-key
// marker it is machine-readable and invisible in rendered Markdown, so the count
// survives round-trips without cluttering the human-facing issue.
var occurrenceRe = regexp.MustCompile(`<!--\s*occurrences:\s*(\d+)\s*-->`)

// occurrenceMarker renders the body marker for an occurrence count.
func occurrenceMarker(n int) string {
	return fmt.Sprintf("<!-- occurrences: %d -->", n)
}

// ExtractOccurrences reads the occurrence count from a finding body, defaulting
// to 1 (the original filing) when the body carries no marker yet.
func ExtractOccurrences(body string) int {
	m := occurrenceRe.FindStringSubmatch(body)
	if m == nil {
		return 1
	}
	n, err := strconv.Atoi(m[1])
	if err != nil || n < 1 {
		return 1
	}
	return n
}

// WithOccurrences returns body with its occurrence marker set to n — replacing
// an existing marker in place, or appending one when the body has none.
func WithOccurrences(body string, n int) string {
	if occurrenceRe.MatchString(body) {
		return occurrenceRe.ReplaceAllString(body, occurrenceMarker(n))
	}
	if strings.TrimSpace(body) == "" {
		return occurrenceMarker(n)
	}
	return body + "\n\n" + occurrenceMarker(n)
}
