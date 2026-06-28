package runlog

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/loopstream"
)

// Structured mirrors the console line + run.jsonl exactly like Event, and ALSO
// appends a structured record to the global logs/loop.jsonl (ADR-0005). The global
// stream lives one level above the per-ticket dir, so every ticket's sessions feed
// one shared stream the viewer tails.
func TestStructuredAppendsRecordToGlobalStream(t *testing.T) {
	root := t.TempDir()
	log, err := New(root, "BEH-1")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	log.Structured(loopstream.Record{
		Kind:    loopstream.KindStageStart,
		Stage:   "implementation",
		Message: "run X — implementation BEH-1",
	})

	data := readFile(t, filepath.Join(root, "loop.jsonl"))
	line := strings.TrimSpace(data)
	var r loopstream.Record
	if err := json.Unmarshal([]byte(line), &r); err != nil {
		t.Fatalf("unmarshal global record %q: %v", line, err)
	}
	if r.Kind != loopstream.KindStageStart {
		t.Errorf("kind = %q, want stage-start", r.Kind)
	}
	if r.Ticket != "BEH-1" {
		t.Errorf("ticket = %q, want it defaulted to the logger's ticket BEH-1", r.Ticket)
	}
	if r.Stage != "implementation" {
		t.Errorf("stage = %q, want implementation", r.Stage)
	}
	if r.Message != "run X — implementation BEH-1" {
		t.Errorf("message = %q, want the console line verbatim", r.Message)
	}
	if r.TS == "" {
		t.Errorf("expected a stamped timestamp")
	}
}

// Structured must keep run.jsonl identical to what Event writes — the per-ticket
// stream and console are explicitly unchanged by the loop-viewer work.
func TestStructuredStillMirrorsToRunJsonl(t *testing.T) {
	root := t.TempDir()
	log, _ := New(root, "BEH-2")
	log.Structured(loopstream.Record{Kind: loopstream.KindIdle, Message: "hello run.jsonl"})

	run := readFile(t, filepath.Join(log.Dir, "run.jsonl"))
	if !strings.Contains(run, "hello run.jsonl") {
		t.Errorf("run.jsonl should carry the message like Event does, got %q", run)
	}
	// run.jsonl stays the lean {ts,message} shape — no kind/ticket/stage leak.
	if strings.Contains(run, "kind") {
		t.Errorf("run.jsonl must not gain structured fields, got %q", run)
	}
}

// An explicit ticket on the record (e.g. a loop-level event naming the ticket it
// released) is preserved, not overwritten by the logger's own ticket.
func TestStructuredKeepsExplicitTicket(t *testing.T) {
	root := t.TempDir()
	log, _ := New(root, "BEH-3")
	log.Structured(loopstream.Record{Kind: loopstream.KindTicketReleased, Ticket: "BEH-99", Message: "released BEH-99"})

	var r loopstream.Record
	_ = json.Unmarshal([]byte(strings.TrimSpace(readFile(t, filepath.Join(root, "loop.jsonl")))), &r)
	if r.Ticket != "BEH-99" {
		t.Errorf("ticket = %q, want the explicit BEH-99 preserved", r.Ticket)
	}
}
