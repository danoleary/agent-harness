package viewer

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTailerPollOnAbsentFileReportsNotRunning(t *testing.T) {
	tl := NewTailer(filepath.Join(t.TempDir(), "loop.jsonl"))
	var out bytes.Buffer
	running, err := tl.Poll(&out)
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if running {
		t.Fatalf("an absent file must report not-running")
	}
	if out.Len() != 0 {
		t.Fatalf("poll must not render anything for an absent file, got %q", out.String())
	}
}

func TestTailerPollRendersOnlyNewCompleteLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	tl := NewTailer(path)

	writeLine(t, path, `{"ts":"t1","kind":"ticket-selected","ticket":"BEH-5","message":"selected BEH-5"}`)
	var out bytes.Buffer
	running, err := tl.Poll(&out)
	if err != nil || !running {
		t.Fatalf("poll: running=%v err=%v", running, err)
	}
	if !strings.Contains(out.String(), "BEH-5") {
		t.Fatalf("expected first line rendered, got %q", out.String())
	}

	// A second poll with nothing new renders nothing (offset advanced).
	out.Reset()
	if _, err := tl.Poll(&out); err != nil {
		t.Fatalf("second poll: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a poll with no new lines must render nothing, got %q", out.String())
	}

	// A newly appended line — rendered against the SAME view, so the prior ticket is
	// carried forward into the stage line.
	writeLine(t, path, `{"ts":"t2","kind":"stage-start","ticket":"BEH-5","stage":"review","message":"run X — review BEH-5"}`)
	out.Reset()
	if _, err := tl.Poll(&out); err != nil {
		t.Fatalf("third poll: %v", err)
	}
	if !strings.Contains(out.String(), "review") || !strings.Contains(out.String(), "BEH-5") {
		t.Fatalf("expected only the new stage line surfacing ticket+stage, got %q", out.String())
	}
	if strings.Contains(out.String(), "selected") {
		t.Fatalf("the already-rendered first line must not render again, got %q", out.String())
	}
}

// A partial (not yet newline-terminated) trailing write is held back until it
// completes, so the viewer never renders half a JSON line.
func TestTailerHoldsBackPartialLine(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	tl := NewTailer(path)
	if err := os.WriteFile(path, []byte(`{"ts":"t","kind":"idle","message":"partial"`), 0o644); err != nil {
		t.Fatalf("write: %v", err)
	}
	var out bytes.Buffer
	if _, err := tl.Poll(&out); err != nil {
		t.Fatalf("poll: %v", err)
	}
	if out.Len() != 0 {
		t.Fatalf("a partial line must not render, got %q", out.String())
	}
	// Completing the line makes it render on the next poll.
	if err := os.WriteFile(path, []byte(`{"ts":"t","kind":"idle","message":"complete"}`+"\n"), 0o644); err != nil {
		t.Fatalf("rewrite: %v", err)
	}
	out.Reset()
	if _, err := tl.Poll(&out); err != nil {
		t.Fatalf("poll 2: %v", err)
	}
	if !strings.Contains(out.String(), "complete") {
		t.Fatalf("expected the completed line to render, got %q", out.String())
	}
}

// A truncation (the daemon restarting and emptying loop.jsonl) resets the tail to
// the start of the fresh run rather than getting stuck past the old offset.
func TestTailerResetsOnTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	tl := NewTailer(path)
	writeLine(t, path, `{"ts":"t1","kind":"idle","message":"old run line one"}`)
	writeLine(t, path, `{"ts":"t2","kind":"idle","message":"old run line two"}`)
	var out bytes.Buffer
	tl.Poll(&out)

	// Daemon restarts: truncate, then write a fresh line.
	if err := os.WriteFile(path, []byte(`{"ts":"t3","kind":"ticket-selected","ticket":"BEH-1","message":"fresh run"}`+"\n"), 0o644); err != nil {
		t.Fatalf("truncate-rewrite: %v", err)
	}
	out.Reset()
	if _, err := tl.Poll(&out); err != nil {
		t.Fatalf("poll after truncation: %v", err)
	}
	if !strings.Contains(out.String(), "fresh run") {
		t.Fatalf("expected the fresh run's line after truncation, got %q", out.String())
	}
}

func writeLine(t *testing.T, path, line string) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o644)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer f.Close()
	if _, err := f.WriteString(line + "\n"); err != nil {
		t.Fatalf("write: %v", err)
	}
}
