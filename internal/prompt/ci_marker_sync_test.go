package prompt

import (
	"testing"

	"github.com/beherd/agent-harness/internal/ci"
)

// logsUnfetchable keys off the marker prefixes ci.fetchFailedLogs / ci.truncateLogs
// emit when no real step output was retrievable. The two sides used to be coupled
// only by "keep in sync" comments; now they share ci's exported marker constants,
// and this test pins the predicate to those constants so a marker rename in the
// driver can't silently desync the predicate (the BEH-560 regression).
func TestLogsUnfetchableMatchesDriverMarkers(t *testing.T) {
	unfetchable := []struct {
		name string
		logs string
	}{
		{"no-runs sentinel", ci.NoRunsSentinel},
		{
			"run header + folded fetch error only",
			ci.RunHeaderMarker + "456 (failed steps) =====\n\n" + ci.FetchErrorMarker + "456: log not found)\n",
		},
		{
			"truncated, still no real output",
			ci.TruncationHeadMarker + " 1000 earlier bytes " + ci.TruncationWord + " …]\n" + ci.NoRunsSentinel,
		},
	}
	for _, tc := range unfetchable {
		if !logsUnfetchable(tc.logs) {
			t.Errorf("%s: logsUnfetchable(%q) = false, want true", tc.name, tc.logs)
		}
	}

	// A run header followed by genuine step output is fetchable — one real line
	// anywhere flips it, so the empty-log framing must not swallow a real failure.
	real := ci.RunHeaderMarker + "1 (failed steps) =====\nFAIL src/foo.test.ts\n"
	if logsUnfetchable(real) {
		t.Errorf("logsUnfetchable(%q) = true, want false (real step output present)", real)
	}
}
