package nowork

import (
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/findings"
	"github.com/danoleary/agent-harness/internal/ticket"
)

var at = time.Date(2026, 11, 14, 9, 30, 0, 0, time.UTC)

func records(t *testing.T) Records {
	return In(t.TempDir(), func() time.Time { return at })
}

// A recorded run reads back as written, stamped with when it was recorded.
func TestReadReturnsTheRecordedRun(t *testing.T) {
	r := records(t)
	if err := r.Write("#330", "abc123", "fp"); err != nil {
		t.Fatalf("Write: %v", err)
	}

	got, ok := r.Read("#330")

	if !ok || got != (Record{Base: "abc123", Ticket: "fp", At: at}) {
		t.Errorf("Read = %+v, %v; want the recorded base and ticket at %s", got, ok, at)
	}
}

// A ticket nothing was recorded for, or one cleared since, has no record.
func TestReadWithoutARecordIsNone(t *testing.T) {
	r := records(t)
	if _, ok := r.Read("#1"); ok {
		t.Error("a ticket with no record must read as none")
	}
	if err := r.Write("#1", "abc123", "fp"); err != nil {
		t.Fatalf("Write: %v", err)
	}
	r.Clear("#1")
	if _, ok := r.Read("#1"); ok {
		t.Error("a cleared record must read as none")
	}
	r.Clear("#2") // clearing nothing is not an error
}

// The fingerprint changes with the title, the body or the labels, and with
// nothing else: priority, URL and label order do not make the ticket new.
func TestOfChangesWithTheBodyAndTheLabels(t *testing.T) {
	base := ticket.Ticket{Identifier: "#330", Title: "Stop re-dispatching", Description: "body", Labels: []string{"agent-ready", "bug"}}
	same := base
	same.Priority, same.URL, same.Labels = "Urgent", "https://example.test/330", []string{"bug", "agent-ready"}
	if Of(base) != Of(same) {
		t.Error("priority, URL and label order must not change the fingerprint")
	}
	for name, edit := range map[string]func(*ticket.Ticket){
		"title":  func(t *ticket.Ticket) { t.Title += "!" },
		"body":   func(t *ticket.Ticket) { t.Description += "\nmore" },
		"labels": func(t *ticket.Ticket) { t.Labels = append(t.Labels, "needs-retry") },
	} {
		changed := base
		changed.Labels = append([]string(nil), base.Labels...)
		edit(&changed)
		if Of(base) == Of(changed) {
			t.Errorf("a changed %s must change the fingerprint", name)
		}
	}
}

// issue480 is #480 as the retrospective filed it: a harness finding, carrying its
// dedup key.
var issue480 = ticket.Ticket{
	Identifier:  "#480",
	Title:       "Harness re-dispatched a ticket it had just marked already-resolved on main, with an identical prompt",
	Description: "**Kind:** harness-dispatch\n\nThe first implementation run ended with the harness recording the ticket as already resolved on main…\n\n_Observed by agent-harness v0.15.0 (c03b2dda68cf)._\n\n_Surfaced during herd by the agent harness._\n\n<!-- finding-key: redispatch-after-already-fixed-handoff -->",
	Labels:      []string{"agent-ready", "harness-dispatch"},
}

// A retrospective that sees a filed finding recur bumps the occurrence count in
// its body. That edit is the harness's own, not a human's, so it gives the next
// run nothing the last one lacked and leaves the fingerprint as it was (#480).
func TestOfIgnoresTheHarnessesOccurrenceBump(t *testing.T) {
	for _, n := range []int{2, 3} {
		bumped := issue480
		bumped.Description = findings.WithOccurrences(issue480.Description, n)
		if Of(bumped) != Of(issue480) {
			t.Errorf("occurrence %d: a recurrence bump must not change the fingerprint", n)
		}
	}
}

// A resolved record carries the session's reason beside the commit, so a later
// run can be told the verdict (#480).
func TestWriteResolvedRecordsTheVerdict(t *testing.T) {
	r := records(t)
	if err := r.WriteResolved("#480", "base", "fp", "fix", "the fix already holds"); err != nil {
		t.Fatalf("WriteResolved: %v", err)
	}

	got, ok := r.Read("#480")

	if want := (Record{Base: "base", Ticket: "fp", At: at, ResolvedBy: "fix", Why: "the fix already holds"}); !ok || got != want {
		t.Errorf("Read = %+v, %v; want %+v", got, ok, want)
	}
}

// Once the loop's blocked label lands, the label holds the ticket. A resolved
// record is kept, marked held, so a later run still knows the verdict; a plain
// record is cleared, as it says nothing a later run needs (#480).
func TestHoldKeepsAResolvedVerdictAndClearsAPlainRecord(t *testing.T) {
	r := records(t)
	if err := r.WriteResolved("#480", "base", "fp", "fix", "why"); err != nil {
		t.Fatal(err)
	}
	if err := r.Write("#330", "base", "fp"); err != nil {
		t.Fatal(err)
	}

	r.Hold("#480")
	r.Hold("#330")
	r.Hold("#1") // holding nothing is not an error

	if got, ok := r.Read("#480"); !ok || !got.Held || got.ResolvedBy != "fix" || got.Why != "why" {
		t.Errorf("resolved record = %+v, %v; want it kept and held", got, ok)
	}
	if got, ok := r.Read("#330"); ok {
		t.Errorf("plain record = %+v; want it cleared", got)
	}
}
