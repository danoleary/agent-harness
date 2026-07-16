package linear

import "testing"

func strptr(s string) *string { return &s }

// A ready-for-agent perf ticket whose gating first step is a Sentry re-measure
// must never be auto-selected — the Implementation stage can't reach telemetry,
// so the first AC is unsatisfiable in-sandbox. The plain eligible ticket wins
// despite lower priority (BEH-757).
func TestSelectNextTicketSkipsTelemetryRemeasureGatedTicket(t *testing.T) {
	remeasure := issueNode{
		identifier: "BEH-719",
		priority:   1,
		labels:     []string{"ready-for-agent", "Performance", "Sentry"},
		title:      "Perf: `/` LCP p75 2.86s — low-confidence, n=3",
		description: "Filed as a *watch/confirm* item, not a confirmed regression. First step: " +
			"**re-pull** `/` LCP over a 7–14d window before investing in a fix.",
	}
	ready := eligibleNode("BEH-WORKABLE", 4)
	tr, _ := selectTransport(t, remeasure, ready)
	got, ok, err := NewClient(tr).SelectNextTicket()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !ok || got.Identifier != "BEH-WORKABLE" {
		t.Errorf("selected = %q (ok=%v), want BEH-WORKABLE — a telemetry-remeasure-gated ticket must be skipped despite higher priority", got.Identifier, ok)
	}
}

// The real BEH-719 shape: a low-confidence perf watch item whose gating first
// step is to re-pull the metric over a wider Sentry window before any fix. The
// tdd sandbox has no telemetry access, so this must be classified as gated on a
// re-measure the sandbox can't perform (BEH-757).
func TestGatedOnTelemetryRemeasureMatchesLowConfidenceWatchItem(t *testing.T) {
	iss := selectedIssue{
		Title: "Perf: `/` LCP p75 2.86s (needs-improvement) — low-confidence, n=3",
		Description: strptr("This is filed as a *watch/confirm* item, not a confirmed regression. " +
			"First step for whoever picks it up: **re-pull** `/` **LCP over a 7–14d window** " +
			"to see if 2.86s holds with a real sample size before investing in a fix."),
	}
	if !gatedOnTelemetryRemeasure(iss) {
		t.Error("a low-confidence watch/confirm item gated on a re-pull over a wider window must be classified as telemetry-remeasure-gated")
	}
}

// A confirmed perf regression with a concrete code fix (the BEH-587 shape)
// carries the same Sentry/Performance labels and cites Sentry, but has no
// re-measure-before-fix gate — it must stay workable, not be swept in.
func TestGatedOnTelemetryRemeasureIgnoresConfirmedRegression(t *testing.T) {
	iss := selectedIssue{
		Title: "Perf: `/call/*` LCP p75 2.83s — slow first paint on the call surface",
		Description: strptr("Confirmed regression from Sentry pageload transactions. rrweb is pulled " +
			"into the entry chunk on the first-paint path. Fix: idle-load it off the entry chunk."),
	}
	if gatedOnTelemetryRemeasure(iss) {
		t.Error("a confirmed regression with a concrete fix must not be classified as telemetry-remeasure-gated")
	}
}

// The conjunction is required: a re-measure verb alone (without the
// low-confidence/watch framing) is not enough to skip a ticket.
func TestGatedOnTelemetryRemeasureRequiresBothSignals(t *testing.T) {
	remeasureOnly := selectedIssue{
		Title:       "Refactor: re-measure the sidebar width after the token change",
		Description: strptr("Adjust the layout and re-measure the sidebar width in the story."),
	}
	if gatedOnTelemetryRemeasure(remeasureOnly) {
		t.Error("a re-measure mention without a low-confidence/watch framing must not match")
	}

	watchOnly := selectedIssue{
		Title:       "Perf: dashboard pageload — low-confidence, n=6",
		Description: strptr("Filed as a watch/confirm item. Investigate the slowest span and split the heavy import."),
	}
	if gatedOnTelemetryRemeasure(watchOnly) {
		t.Error("a low-confidence/watch framing without a re-measure gate must not match")
	}
}
