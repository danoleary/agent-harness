package fix

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/runlog"
	"github.com/danoleary/agent-harness/internal/runlog/runlogtest"
)

// testLog is a run log under a temp dir whose narration is kept from stdout and
// handed to t.Log only if the test fails, so a red package prints each failure
// beside the narration of the runs that caused it, not under every test's gate
// and slot chatter (#225).
func testLog(t *testing.T) *runlog.Logger {
	t.Helper()
	return runlogtest.New(t, "PROJ-1")
}
