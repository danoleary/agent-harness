package loop

import (
	"fmt"
	"strings"
)

// breaker is the daemon's runaway guard: it counts consecutive tickets that
// failed to reach a pushed PR and trips once that count hits the threshold, so the
// loop winds down rather than burning the whole ready-for-agent queue on an
// identical failure (DESIGN.md §Circuit breaker). The success signal is "did this
// ticket reach a pushed PR?", never the raw pipeline exit code.
type breaker struct {
	// threshold is the consecutive no-PR count that trips the breaker. A
	// non-positive threshold disables it (tripped never fires).
	threshold int
	// streak holds the ticket ids of the current consecutive-failure run, in order,
	// so the trip report can name a repeat offender when they all share one id.
	streak []string
}

func newBreaker(threshold int) *breaker { return &breaker{threshold: threshold} }

// record folds one finished ticket into the counter. ReachedPushedPR wins over
// everything — a shipped PR is a success even if a later stage cap-aborted — and
// resets the streak; a spending-cap abort with no PR is neutral (the breaker is
// deliberately blind to the cap runaway, which the backoff handles); a recommend-close
// is likewise neutral (a correct terminal no-op, neither a ship nor a failure —
// BEH-603); anything else is a no-PR failure that extends the streak.
func (b *breaker) record(identifier string, o TicketOutcome) {
	switch {
	case o.ReachedPushedPR:
		b.streak = nil
	case o.SpendingCapAbort:
		// neutral — blind by design
	case o.RecommendClose:
		// neutral — a correct terminal no-op (BEH-603), not a ship failure
	default:
		b.streak = append(b.streak, identifier)
	}
}

// tripped reports whether the consecutive-failure streak has reached the
// threshold. A non-positive threshold can never trip.
func (b *breaker) tripped() bool {
	return b.threshold > 0 && len(b.streak) >= b.threshold
}

// report is the loud wind-down message. When every failure in the tripping streak
// shares one ticket id, it names that repeat offender ("BEH-NNN failed N× — start
// here") so a poison top-of-queue ticket (released to Todo on a no-worktree crash,
// then re-selected) is immediately visible; otherwise it lists the distinct ids.
func (b *breaker) report() string {
	n := len(b.streak)
	if n > 0 && allSame(b.streak) {
		return fmt.Sprintf("circuit breaker tripped: %s failed %d× — start here", b.streak[0], n)
	}
	return fmt.Sprintf(
		"circuit breaker tripped: %d consecutive tickets failed to reach a pushed PR (%s)",
		n, strings.Join(b.streak, ", "),
	)
}

// allSame reports whether every element of ids equals the first.
func allSame(ids []string) bool {
	for _, id := range ids {
		if id != ids[0] {
			return false
		}
	}
	return true
}
