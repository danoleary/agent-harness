// Package runlogtest builds run logs for tests whose narration stays out of the
// test binary's stdout.
//
// A Logger with no Console narrates to os.Stdout, and go test prints a failed
// package's whole stdout. Stage tests feed canned red-gate logs through the
// flows that narrate a red gate's tail, so a package with one real failure used
// to print a dozen fake `--- FAIL:` blocks from tests that passed (#460). A
// Logger built here keeps its narration and hands it to t.Log only if its own
// test fails, beside that test's failure (#225).
package runlogtest

import (
	"strings"
	"sync"
	"testing"

	"github.com/danoleary/agent-harness/internal/runlog"
)

// New opens a run log for key under a temp dir, quieted as [Quiet] does.
func New(t testing.TB, key string) *runlog.Logger {
	t.Helper()
	log, err := runlog.New(t.TempDir(), key)
	if err != nil {
		t.Fatalf("runlog.New: %v", err)
	}
	return Quiet(t, log)
}

// Quiet points log's narration at a buffer t.Log prints only if t fails, and
// returns log.
func Quiet(t testing.TB, log *runlog.Logger) *runlog.Logger {
	t.Helper()
	console := &lockedBuilder{}
	log.Console = console
	t.Cleanup(func() {
		if t.Failed() && console.String() != "" {
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
