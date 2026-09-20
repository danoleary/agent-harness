package loop

import (
	"regexp"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/stages"
)

// A single no-PR ticket increments the counter but does not trip below threshold.
func TestBreakerIncrementsOnNoPRButHoldsBelowThreshold(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", stages.Result{Disposition: stages.NoPR})
	if b.tripped() {
		t.Error("one no-PR failure must not trip a threshold-3 breaker")
	}
}

// A pushed-PR ticket resets the counter — even one accumulated failure is cleared,
// so the next failures start a fresh streak.
func TestBreakerResetsOnPushedPR(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", stages.Result{Disposition: stages.NoPR})
	b.record("BEH-2", stages.Result{Disposition: stages.NoPR})
	b.record("BEH-3", stages.Result{Disposition: stages.Shipped}) // shipped → reset
	b.record("BEH-4", stages.Result{Disposition: stages.NoPR})
	b.record("BEH-5", stages.Result{Disposition: stages.NoPR})
	if b.tripped() {
		t.Error("a pushed PR must reset the counter, so two later failures (post-reset) must not trip")
	}
}

// Reaching the threshold of consecutive no-PR tickets trips the breaker.
func TestBreakerTripsAtThreshold(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", stages.Result{})
	b.record("BEH-2", stages.Result{})
	if b.tripped() {
		t.Error("breaker tripped at 2 failures, want it to hold until the 3rd")
	}
	b.record("BEH-3", stages.Result{})
	if !b.tripped() {
		t.Error("3 consecutive no-PR failures must trip a threshold-3 breaker")
	}
}

// A spending-cap abort is neutral: it neither increments nor resets, so the
// breaker is blind to the cap runaway (which the daemon's backoff handles).
func TestBreakerNeutralOnSpendingCapAbort(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", stages.Result{})
	b.record("BEH-2", stages.Result{Disposition: stages.CapAborted}) // neutral
	b.record("BEH-3", stages.Result{Disposition: stages.CapAborted}) // neutral
	if b.tripped() {
		t.Error("cap aborts must not advance the counter; only 1 real failure recorded, must not trip")
	}
	b.record("BEH-4", stages.Result{})
	b.record("BEH-5", stages.Result{})
	if !b.tripped() {
		t.Error("the two real failures around the neutral cap aborts make 3 — must trip")
	}
}

// BEH-603: a recommend-close disposition (a zero-net-diff branch correctly concluded
// to be a duplicate/superseded) is neutral — like a cap abort it neither increments
// nor resets. It is a correct terminal outcome, not a ship failure, so a run of
// legitimate duplicates must never trip the breaker; but it is no proof the pipeline
// shipped anything, so it must not clear prior real failures either.
func TestBreakerNeutralOnRecommendClose(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", stages.Result{})
	b.record("BEH-2", stages.Result{Disposition: stages.RecommendClose}) // neutral
	b.record("BEH-3", stages.Result{Disposition: stages.RecommendClose}) // neutral
	if b.tripped() {
		t.Error("recommend-close must not advance the counter; only 1 real failure recorded, must not trip")
	}
	b.record("BEH-4", stages.Result{})
	b.record("BEH-5", stages.Result{})
	if !b.tripped() {
		t.Error("the two real failures around the neutral recommend-closes make 3 — must trip")
	}
}

// A Docker-preflight abort is neutral: like a cap abort it did no ticket work (the
// host couldn't launch a sandbox before any work began), so it must neither increment
// nor reset. This is the fix for the observed pattern where the same poison
// top-of-queue ticket racked up three ~2s preflight failures and tripped the breaker
// in seconds — with this the breaker stays blind and the loop's disk-reclaim + backoff
// handles the environment instead.
func TestBreakerNeutralOnPreflightAbort(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", stages.Result{})
	b.record("BEH-1", stages.Result{Disposition: stages.PreflightAborted}) // neutral
	b.record("BEH-1", stages.Result{Disposition: stages.PreflightAborted}) // neutral
	b.record("BEH-1", stages.Result{Disposition: stages.PreflightAborted}) // neutral
	if b.tripped() {
		t.Error("preflight aborts must not advance the counter; only 1 real failure recorded, must not trip")
	}
	b.record("BEH-2", stages.Result{})
	b.record("BEH-3", stages.Result{})
	if !b.tripped() {
		t.Error("the two real failures around the neutral preflight aborts make 3 — must trip")
	}
}

// A non-positive threshold disables the breaker entirely — it never trips no matter
// how many failures accumulate.
func TestBreakerDisabledWhenThresholdNonPositive(t *testing.T) {
	b := newBreaker(0)
	for i := 0; i < 100; i++ {
		b.record("BEH-1", stages.Result{})
	}
	if b.tripped() {
		t.Error("a non-positive threshold must disable the breaker")
	}
}

// When every failure in the tripping streak shares one ticket id, the report names
// that repeat offender so a poison top-of-queue ticket is immediately visible.
func TestBreakerReportNamesRepeatOffender(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-42", stages.Result{})
	b.record("BEH-42", stages.Result{})
	b.record("BEH-42", stages.Result{})
	rep := b.report()
	if !strings.Contains(rep, "BEH-42") {
		t.Errorf("report %q does not name the repeat offender BEH-42", rep)
	}
	if !regexp.MustCompile(`3×|3 times|failed 3`).MatchString(rep) {
		t.Errorf("report %q does not state the ticket failed 3×", rep)
	}
	if !regexp.MustCompile(`(?i)start here`).MatchString(rep) {
		t.Errorf("report %q does not point the human at the offender ('start here')", rep)
	}
}

// When the failures are spread across distinct tickets, the report lists them all
// rather than misattributing the trip to one id.
func TestBreakerReportListsDistinctTickets(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", stages.Result{})
	b.record("BEH-2", stages.Result{})
	b.record("BEH-3", stages.Result{})
	rep := b.report()
	for _, id := range []string{"BEH-1", "BEH-2", "BEH-3"} {
		if !strings.Contains(rep, id) {
			t.Errorf("report %q omits failed ticket %s", rep, id)
		}
	}
	if regexp.MustCompile(`failed 3×`).MatchString(rep) {
		t.Errorf("distinct-ticket report %q must not claim a single ticket failed 3×", rep)
	}
}
