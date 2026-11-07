package fix

import (
	"strings"
	"sync"
	"testing"

	"github.com/danoleary/agent-harness/internal/runlog"
)

// testLog is a run log under a temp dir whose narration is kept from stdout and
// handed to t.Log only if the test fails, so a red package prints each failure
// beside the narration of the runs that caused it, not under every test's gate
// and slot chatter (#225).
func testLog(t *testing.T) *runlog.Logger {
	t.Helper()
	log, err := runlog.New(t.TempDir(), "PROJ-1")
	if err != nil {
		t.Fatal(err)
	}
	console := &lockedBuilder{}
	log.Console = console
	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("run-log narration:\n%s", console.String())
		}
	})
	return log
}

// lockedBuilder is a strings.Builder the gate goroutines of concurrent runs can
// narrate into at once.
type lockedBuilder struct {
	mu sync.Mutex
	b  strings.Builder
}

func (l *lockedBuilder) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedBuilder) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}
