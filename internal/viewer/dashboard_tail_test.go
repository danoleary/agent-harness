package viewer

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestDashboardTailerOnAbsentFileReportsNotRunning(t *testing.T) {
	dt := NewDashboardTailer(filepath.Join(t.TempDir(), "loop.jsonl"))
	running, err := dt.Poll()
	if err != nil {
		t.Fatalf("poll: %v", err)
	}
	if running {
		t.Fatalf("an absent file must report not-running")
	}
}

// The tailer feeds newly-appended records into the model, so a Render after a Poll
// reflects the file's current ticket and stage.
func TestDashboardTailerFeedsModelFromFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "loop.jsonl")
	dt := NewDashboardTailer(path)

	writeLine(t, path, `{"ts":"2026-06-28T12:00:00Z","kind":"ticket-selected","ticket":"BEH-3","message":"selected BEH-3 (Urgent) — claimed"}`)
	writeLine(t, path, `{"ts":"2026-06-28T12:00:01Z","kind":"stage-start","ticket":"BEH-3","stage":"implementation","message":"run X — implementation BEH-3"}`)

	running, err := dt.Poll()
	if err != nil || !running {
		t.Fatalf("poll: running=%v err=%v", running, err)
	}
	frame := dt.Dashboard().Render(time.Unix(0, 0), true)
	if !strings.Contains(frame, "BEH-3") || !strings.Contains(frame, "implementation") || !strings.Contains(frame, "1 of 3") {
		t.Fatalf("expected the model fed from file, got:\n%s", frame)
	}

	// A second poll with nothing new advances the offset and does not double-count.
	if _, err := dt.Poll(); err != nil {
		t.Fatalf("second poll: %v", err)
	}
}
