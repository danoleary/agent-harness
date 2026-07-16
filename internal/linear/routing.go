package linear

import "strings"

// gatedOnTelemetryRemeasure reports whether an issue's gating acceptance
// criterion is to re-measure a metric against production telemetry (Sentry
// pageload/LCP data) before any code fix is warranted — the "low-confidence
// perf watch/confirm" shape (BEH-757, surfaced during BEH-719).
//
// Such a ticket is structurally un-actionable by the Implementation stage: the
// sandbox owns no telemetry I/O, so the FIRST acceptance criterion (re-pull the
// number over a wider window to confirm-or-refute it) can never be satisfied
// in-session — the best a code stage can do is investigate and recommend
// close/defer, which wastes a full pipeline run. eligible() skips it so it is
// never auto-dispatched; a human does the measurement/triage first. The skip is
// content-based, not label-based, because not every Performance ticket is
// un-actionable — a confirmed regression with a concrete code fix (e.g. BEH-587)
// carries the same label but no re-measure gate, and must stay workable.
//
// Detection is a conservative conjunction of two independent signals, so a
// confirmed-regression ticket that merely mentions Sentry is not swept in:
//  1. a low-confidence / watch-confirm framing, AND
//  2. a re-measure-before-fix gate (re-pull/re-measure the metric).
//
// The natural escape hatch needs no new label: once a human has done the
// measurement and wants the harness to implement the fix, they reframe the
// ticket to a confirmed regression (dropping the "re-measure first" gate), at
// which point the conjunction no longer matches and it becomes eligible.
func gatedOnTelemetryRemeasure(s selectedIssue) bool {
	hay := strings.ToLower(s.Title)
	if s.Description != nil {
		hay += "\n" + strings.ToLower(*s.Description)
	}
	return lowConfidenceWatch(hay) && remeasureBeforeFixGate(hay)
}

// lowConfidenceWatch reports the "not a confirmed regression, watch it" framing
// that marks a perf finding as provisional rather than actionable.
func lowConfidenceWatch(hay string) bool {
	return containsAny(hay,
		"low-confidence",
		"low confidence",
		"watch/confirm",
		"watch / confirm",
		"not a confirmed regression",
	)
}

// remeasureBeforeFixGate reports that the gating first step is to re-pull /
// re-measure the metric (against telemetry the sandbox can't reach) before
// investing in a fix.
func remeasureBeforeFixGate(hay string) bool {
	return containsAny(hay,
		"re-pull",
		"repull",
		"re-measure",
		"remeasure",
	)
}

// containsAny reports whether hay contains any of the needles.
func containsAny(hay string, needles ...string) bool {
	for _, n := range needles {
		if strings.Contains(hay, n) {
			return true
		}
	}
	return false
}
