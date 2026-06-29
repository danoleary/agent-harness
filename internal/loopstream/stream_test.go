package loopstream

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAppendWritesOneJSONLineRecordPerEvent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	s := NewStream(path)

	if err := s.Append(Record{Kind: KindTicketSelected, Ticket: "BEH-1", Message: "selected BEH-1"}); err != nil {
		t.Fatalf("append 1: %v", err)
	}
	if err := s.Append(Record{Kind: KindStageStart, Ticket: "BEH-1", Stage: "implementation", Message: "run X — implementation BEH-1"}); err != nil {
		t.Fatalf("append 2: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	lines := strings.Split(strings.TrimRight(string(data), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 JSONL lines, got %d: %q", len(lines), string(data))
	}

	var first Record
	if err := json.Unmarshal([]byte(lines[0]), &first); err != nil {
		t.Fatalf("unmarshal line 1: %v", err)
	}
	if first.Kind != KindTicketSelected || first.Ticket != "BEH-1" || first.Message != "selected BEH-1" {
		t.Fatalf("round-trip mismatch: %+v", first)
	}
	if first.TS == "" {
		t.Fatalf("expected Append to stamp a timestamp, got empty TS")
	}
}

func TestAppendStampsTSWhenAbsentButKeepsCallerTS(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	s := NewStream(path)
	if err := s.Append(Record{TS: "2026-06-28T00:00:00Z", Kind: KindIdle, Message: "idle"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	data, _ := os.ReadFile(path)
	var r Record
	_ = json.Unmarshal([]byte(strings.TrimSpace(string(data))), &r)
	if r.TS != "2026-06-28T00:00:00Z" {
		t.Fatalf("expected caller TS preserved, got %q", r.TS)
	}
}

func TestRecordJSONFieldNames(t *testing.T) {
	b, _ := json.Marshal(Record{TS: "t", Kind: KindToolUse, Ticket: "BEH-2", Stage: "review", Message: "m", Detail: "d"})
	got := string(b)
	for _, want := range []string{`"ts":"t"`, `"kind":"tool-use"`, `"ticket":"BEH-2"`, `"stage":"review"`, `"message":"m"`, `"detail":"d"`} {
		if !strings.Contains(got, want) {
			t.Fatalf("expected %s in %s", want, got)
		}
	}
}

func TestTruncateEmptiesAnExistingStream(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	s := NewStream(path)
	_ = s.Append(Record{Kind: KindIdle, Message: "stale from a prior run"})

	if err := s.Truncate(); err != nil {
		t.Fatalf("truncate: %v", err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read after truncate: %v", err)
	}
	if len(data) != 0 {
		t.Fatalf("expected empty file after truncate, got %q", string(data))
	}
}

func TestTruncateAbsentFileIsNotAnError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "never-existed.jsonl")
	s := NewStream(path)
	if err := s.Truncate(); err != nil {
		t.Fatalf("truncate of absent file should be a no-op success, got %v", err)
	}
}

// BEH-613: the daemon must emit a definitive terminal record when it stops, so the
// logs and the viewer animation can mark the stop unambiguously. KindLoopStopped is
// that terminal kind — it must be a valid, listed member of the closed enum.
func TestLoopStoppedKindIsValidAndListed(t *testing.T) {
	if !KindLoopStopped.Valid() {
		t.Fatal("KindLoopStopped must be a valid member of the closed enum")
	}
	found := false
	for _, k := range AllKinds() {
		if k == KindLoopStopped {
			found = true
		}
	}
	if !found {
		t.Fatal("KindLoopStopped must be listed in AllKinds()")
	}
}
