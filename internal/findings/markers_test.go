package findings

import (
	"strings"
	"testing"
)

// The key marker round-trips: what KeyMarker writes, ExtractKey reads back. This
// is the whole point of the shared home — the three adapters file and search
// through the same pair, so a finding filed under one tracker dedups under
// another.
func TestKeyMarkerRoundTrips(t *testing.T) {
	body := "some prose\n\n" + KeyMarker("sandbox-playwright-missing-deps")
	if got := ExtractKey(body); got != "sandbox-playwright-missing-deps" {
		t.Errorf("ExtractKey = %q, want sandbox-playwright-missing-deps (body: %q)", got, body)
	}
}

func TestExtractKeyIsEmptyWithoutAMarker(t *testing.T) {
	if got := ExtractKey("a body the harness never keyed"); got != "" {
		t.Errorf("ExtractKey(no marker) = %q, want \"\"", got)
	}
}

func TestExtractOccurrencesDefaultsToOne(t *testing.T) {
	if got := ExtractOccurrences("a body with no marker"); got != 1 {
		t.Errorf("ExtractOccurrences(no marker) = %d, want 1", got)
	}
	if got := ExtractOccurrences("x <!-- occurrences: 5 --> y"); got != 5 {
		t.Errorf("ExtractOccurrences(marker 5) = %d, want 5", got)
	}
}

// A junk or out-of-range count is not a reason to crash or to lose the finding:
// it degrades to the original-filing default (ADR-0001).
func TestExtractOccurrencesDefaultsOnJunk(t *testing.T) {
	if got := ExtractOccurrences("<!-- occurrences: 0 -->"); got != 1 {
		t.Errorf("ExtractOccurrences(0) = %d, want 1", got)
	}
}

func TestWithOccurrencesAppendsAndReplaces(t *testing.T) {
	appended := WithOccurrences("body", 2)
	if !strings.Contains(appended, "body") || !strings.Contains(appended, "<!-- occurrences: 2 -->") {
		t.Errorf("WithOccurrences append = %q", appended)
	}
	replaced := WithOccurrences("body <!-- occurrences: 2 -->", 3)
	if strings.Count(replaced, "occurrences:") != 1 || !strings.Contains(replaced, "<!-- occurrences: 3 -->") {
		t.Errorf("WithOccurrences replace = %q", replaced)
	}
}

// An empty body gets the bare marker, not a leading blank line.
func TestWithOccurrencesOnEmptyBody(t *testing.T) {
	if got := WithOccurrences("   ", 4); got != "<!-- occurrences: 4 -->" {
		t.Errorf("WithOccurrences(empty) = %q", got)
	}
}
