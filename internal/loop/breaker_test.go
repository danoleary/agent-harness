package loop

import (
	"regexp"
	"strings"
	"testing"
)

// A single no-PR ticket increments the counter but does not trip below threshold.
func TestBreakerIncrementsOnNoPRButHoldsBelowThreshold(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", TicketOutcome{ReachedPushedPR: false})
	if b.tripped() {
		t.Error("one no-PR failure must not trip a threshold-3 breaker")
	}
}

// A pushed-PR ticket resets the counter — even one accumulated failure is cleared,
// so the next failures start a fresh streak.
func TestBreakerResetsOnPushedPR(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", TicketOutcome{ReachedPushedPR: false})
	b.record("BEH-2", TicketOutcome{ReachedPushedPR: false})
	b.record("BEH-3", TicketOutcome{ReachedPushedPR: true}) // shipped → reset
	b.record("BEH-4", TicketOutcome{ReachedPushedPR: false})
	b.record("BEH-5", TicketOutcome{ReachedPushedPR: false})
	if b.tripped() {
		t.Error("a pushed PR must reset the counter, so two later failures (post-reset) must not trip")
	}
}

// Reaching the threshold of consecutive no-PR tickets trips the breaker.
func TestBreakerTripsAtThreshold(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", TicketOutcome{})
	b.record("BEH-2", TicketOutcome{})
	if b.tripped() {
		t.Error("breaker tripped at 2 failures, want it to hold until the 3rd")
	}
	b.record("BEH-3", TicketOutcome{})
	if !b.tripped() {
		t.Error("3 consecutive no-PR failures must trip a threshold-3 breaker")
	}
}

// A spending-cap abort is neutral: it neither increments nor resets, so the
// breaker is blind to the cap runaway (which the daemon's backoff handles).
func TestBreakerNeutralOnSpendingCapAbort(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-1", TicketOutcome{})
	b.record("BEH-2", TicketOutcome{SpendingCapAbort: true}) // neutral
	b.record("BEH-3", TicketOutcome{SpendingCapAbort: true}) // neutral
	if b.tripped() {
		t.Error("cap aborts must not advance the counter; only 1 real failure recorded, must not trip")
	}
	b.record("BEH-4", TicketOutcome{})
	b.record("BEH-5", TicketOutcome{})
	if !b.tripped() {
		t.Error("the two real failures around the neutral cap aborts make 3 — must trip")
	}
}

// A pushed PR that also carried a spending-cap abort (e.g. a CI auto-fix cap abort
// after the PR shipped) still resets — the shipped PR wins over the cap signal.
func TestBreakerPushedPRWinsOverCapAbort(t *testing.T) {
	b := newBreaker(2)
	b.record("BEH-1", TicketOutcome{})
	b.record("BEH-2", TicketOutcome{ReachedPushedPR: true, SpendingCapAbort: true}) // shipped → reset
	b.record("BEH-3", TicketOutcome{})
	if b.tripped() {
		t.Error("a shipped PR must reset even when the run also cap-aborted; one later failure must not trip")
	}
}

// A non-positive threshold disables the breaker entirely — it never trips no matter
// how many failures accumulate.
func TestBreakerDisabledWhenThresholdNonPositive(t *testing.T) {
	b := newBreaker(0)
	for i := 0; i < 100; i++ {
		b.record("BEH-1", TicketOutcome{})
	}
	if b.tripped() {
		t.Error("a non-positive threshold must disable the breaker")
	}
}

// When every failure in the tripping streak shares one ticket id, the report names
// that repeat offender so a poison top-of-queue ticket is immediately visible.
func TestBreakerReportNamesRepeatOffender(t *testing.T) {
	b := newBreaker(3)
	b.record("BEH-42", TicketOutcome{})
	b.record("BEH-42", TicketOutcome{})
	b.record("BEH-42", TicketOutcome{})
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
	b.record("BEH-1", TicketOutcome{})
	b.record("BEH-2", TicketOutcome{})
	b.record("BEH-3", TicketOutcome{})
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
