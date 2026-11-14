package nowork

import (
	"testing"
	"time"

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
