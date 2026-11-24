package runlog

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/loopstream"
	"github.com/danoleary/agent-harness/internal/version"
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
	if want := "run X — implementation BEH-1 · " + version.Build(); r.Message != want {
		t.Errorf("message = %q, want the console line verbatim, %q", r.Message, want)
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

// A Stage-start record is the run's header, and it names the harness build that
// ran it (#458): a retrospective that sees behaviour a commit on main already
// fixed can tell a stale binary from a regression without diffing the transcript
// against the source.
func TestStageStartHeaderNamesTheHarnessBuild(t *testing.T) {
	defer func(v, c string) { version.Version, version.Commit = v, c }(version.Version, version.Commit)
	version.Version, version.Commit = "v0.12.2", "a9e5c7378763"

	root := t.TempDir()
	log, _ := New(root, "BEH-4")
	log.Structured(loopstream.Record{Kind: loopstream.KindStageStart, Stage: "implementation", Message: "run X — implementation BEH-4"})
	log.Structured(loopstream.Record{Kind: loopstream.KindIdle, Message: "idle"})

	want := "run X — implementation BEH-4 · agent-harness v0.12.2 (a9e5c7378763)"
	lines := strings.Split(strings.TrimSpace(readFile(t, filepath.Join(log.Dir, "run.jsonl"))), "\n")
	var header, other struct{ Message string }
	_ = json.Unmarshal([]byte(lines[0]), &header)
	_ = json.Unmarshal([]byte(lines[1]), &other)
	if header.Message != want {
		t.Errorf("run.jsonl header = %q, want %q", header.Message, want)
	}
	if other.Message != "idle" {
		t.Errorf("a non-header line = %q, want it unchanged", other.Message)
	}

	var r loopstream.Record
	_ = json.Unmarshal([]byte(strings.Split(strings.TrimSpace(readFile(t, filepath.Join(root, "loop.jsonl"))), "\n")[0]), &r)
	if r.Message != want {
		t.Errorf("loop.jsonl header = %q, want it to match run.jsonl's %q", r.Message, want)
	}
}
