package runlogtest

import (
	"fmt"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/runlog"
)

// fakeT is the testing.TB a quieted Logger reports to, with its failure state
// and cleanups under the test's control.
type fakeT struct {
	testing.TB
	failed   bool
	cleanups []func()
	logged   []string
}

func (f *fakeT) Helper()                      {}
func (f *fakeT) Failed() bool                 { return f.failed }
func (f *fakeT) Cleanup(fn func())            { f.cleanups = append(f.cleanups, fn) }
func (f *fakeT) Logf(format string, a ...any) { f.logged = append(f.logged, fmt.Sprintf(format, a...)) }
func (f *fakeT) finish() {
	for _, fn := range f.cleanups {
		fn()
	}
}

// A quieted Logger's narration reaches t.Log only when its test fails, beside
// that failure; a passing test logs none of it.
func TestQuietLogsTheNarrationOnlyForAFailedTest(t *testing.T) {
	for _, failed := range []bool{false, true} {
		ft := &fakeT{failed: failed}
		log := Quiet(ft, &runlog.Logger{Dir: t.TempDir()})

		log.Event("review ✗ gate \"check\" is red: --- FAIL: TestTheReviewFix")
		ft.finish()

		said := strings.Join(ft.logged, "\n")
		if failed != strings.Contains(said, "--- FAIL: TestTheReviewFix") {
			t.Errorf("failed=%v: t.Log got %q", failed, said)
		}
	}
}
