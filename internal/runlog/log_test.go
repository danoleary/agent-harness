package runlog

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMakeRunIDFormatsZeroPadded(t *testing.T) {
	id := MakeRunID(time.Date(2026, 6, 11, 14, 8, 5, 0, time.UTC))
	if id != "20260611-140805" {
		t.Errorf("MakeRunID = %q, want 20260611-140805", id)
	}
}

func TestMakeRunIDSortsChronologically(t *testing.T) {
	earlier := MakeRunID(time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	later := MakeRunID(time.Date(2026, 12, 31, 23, 59, 59, 0, time.UTC))

	if !(earlier < later) {
		t.Errorf("expected %q < %q", earlier, later)
	}
	if earlier != "20260101-000000" {
		t.Errorf("earlier = %q", earlier)
	}
	if later != "20261231-235959" {
		t.Errorf("later = %q", later)
	}
}

// New keys the log directory by ticket id, not run id — so every session for a
// ticket (implementation, review, retrospective) lands under one dir later tools
// can find by globbing (DESIGN.md "Logging").
func TestNewCreatesTicketKeyedDir(t *testing.T) {
	root := t.TempDir()
	log, err := New(root, "BEH-370")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := filepath.Join(root, "BEH-370")
	if log.Dir != want {
		t.Errorf("Dir = %q, want %q", log.Dir, want)
	}
	if fi, err := os.Stat(want); err != nil || !fi.IsDir() {
		t.Errorf("expected ticket dir %q to exist as a directory (err=%v)", want, err)
	}
}

func TestTranscriptNameIsSessionPrefixedRunIDSuffixed(t *testing.T) {
	got := TranscriptName("implementation", "20260611-140805")
	if want := "implementation-20260611-140805.jsonl"; got != want {
		t.Errorf("TranscriptName = %q, want %q", got, want)
	}
}

// A raw-stdout step log (install/gate) is truncated at the kill point with no
// terminal marker, so a reader can't tell an OOM from a clean finish without
// cross-referencing run.jsonl. StepFooter is the self-describing last line each
// step log gets; a 137 must read as a SIGKILL/OOM, not an opaque code.
func TestStepFooterFlags137AsSigkillOOM(t *testing.T) {
	got := StepFooter(137)
	if !strings.HasPrefix(got, "-- step exited 137") {
		t.Errorf("StepFooter(137) = %q, want a `-- step exited 137 …` marker", got)
	}
	for _, want := range []string{"SIGKILL", "OOM"} {
		if !strings.Contains(got, want) {
			t.Errorf("StepFooter(137) = %q, want it to mention %q", got, want)
		}
	}
}

// A clean exit must read as clean, and an arbitrary non-zero code is surfaced
// verbatim rather than silently dropped.
func TestStepFooterDistinguishesCleanFromFailing(t *testing.T) {
	if got := StepFooter(0); !strings.Contains(got, "0") || !strings.Contains(got, "ok") {
		t.Errorf("StepFooter(0) = %q, want a clean-exit marker mentioning 0/ok", got)
	}
	if got := StepFooter(1); got != "-- step exited 1 --" {
		t.Errorf("StepFooter(1) = %q, want `-- step exited 1 --`", got)
	}
}

// Raw-stdout step logs (install/gate) must NOT carry a .jsonl extension — a
// reader who sees .jsonl expects a parseable event stream (BEH-537).
func TestStepLogNameIsLogNotJsonl(t *testing.T) {
	got := StepLogName("install", "20260611-140805")
	if want := "install-20260611-140805.log"; got != want {
		t.Errorf("StepLogName = %q, want %q", got, want)
	}
	if strings.HasSuffix(got, ".jsonl") {
		t.Errorf("StepLogName must not end in .jsonl, got %q", got)
	}
}

// GateTranscriptName is a raw-stdout step log too — keep its .log naming.
func TestGateTranscriptNameIsRunIDSuffixedLog(t *testing.T) {
	if got := GateTranscriptName("20260611-140805"); got != "gate-20260611-140805.log" {
		t.Errorf("GateTranscriptName = %q, want gate-20260611-140805.log", got)
	}
}

func TestFindingsDirIsUnderTicketDirBySession(t *testing.T) {
	log := &Logger{Dir: "/logs/BEH-370"}
	got := log.FindingsDir("implementation")
	if want := filepath.Join("/logs/BEH-370", "findings", "implementation"); got != want {
		t.Errorf("FindingsDir = %q, want %q", got, want)
	}
}

// A second run of the same ticket must not clobber the first's transcript: each
// run tees to its own run-id-suffixed file, while the shared run.jsonl is
// appended across runs (DESIGN.md: ticket-keyed logs).
func TestSecondRunDoesNotClobberFirstTranscript(t *testing.T) {
	root := t.TempDir()
	log, err := New(root, "BEH-370")
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first := TranscriptName("implementation", "20260611-100000")
	second := TranscriptName("implementation", "20260611-120000")

	log.TeeLine(first, `{"run":1}`)
	log.Event("first run event")
	log.TeeLine(second, `{"run":2}`)
	log.Event("second run event")

	wantFirst := filepath.Join(log.Dir, first)
	wantSecond := filepath.Join(log.Dir, second)
	if got := readFile(t, wantFirst); got != "{\"run\":1}\n" {
		t.Errorf("first transcript = %q, want the first run's line intact", got)
	}
	if got := readFile(t, wantSecond); got != "{\"run\":2}\n" {
		t.Errorf("second transcript = %q, want the second run's line", got)
	}

	// The shared event stream accumulates both runs (one file per ticket).
	events := readFile(t, filepath.Join(log.Dir, "run.jsonl"))
	if !strings.Contains(events, "first run event") || !strings.Contains(events, "second run event") {
		t.Errorf("run.jsonl should carry both runs' events, got: %q", events)
	}
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}
