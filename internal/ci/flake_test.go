package ci

import (
	"errors"
	"fmt"
	"strings"
	"testing"
)

// scriptedRunView answers `gh run view <args>` from a table keyed by the joined
// args; an unscripted call errors, as gh would on a run it cannot read.
func scriptedRunView(answers map[string]string) func(args ...string) ([]byte, error) {
	return func(args ...string) ([]byte, error) {
		if out, ok := answers[strings.Join(args, " ")]; ok {
			return []byte(out), nil
		}
		return nil, errors.New("HTTP 404: Not Found")
	}
}

// failedLogLines renders n `gh run view --log-failed` lines for one job's step:
// gh prefixes each line with the job and step names, tab-separated.
func failedLogLines(job, step string, n int) string {
	var b strings.Builder
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s\t%s\t2026-11-22T10:00:00.0000000Z line %d\n", job, step, i)
	}
	return b.String()
}

const flakeJobsJSON = `{"jobs":[
 {"databaseId":2,"name":"lint","conclusion":"failure","steps":[
  {"name":"Set up job","conclusion":"success","number":1},
  {"name":"Run golangci-lint","conclusion":"failure","number":2},
  {"name":"Post Run actions/checkout","conclusion":"skipped","number":3}]},
 {"databaseId":3,"name":"build","conclusion":"success","steps":[
  {"name":"Build","conclusion":"success","number":1}]}]}`

// #371: when a re-run washes a red check out, the driver reads the failed
// attempt — the one before the run's latest — and records its failing step and
// the tail of its failed log, capped per check, to the ticket's log dir. The step
// comes back keyed by check name for the flake: reason.
func TestRecordFlakeKeepsTheFailedAttemptsStepAndLogTail(t *testing.T) {
	var kept strings.Builder
	d := &GhDriver{
		viewRun: scriptedRunView(map[string]string{
			"100 --json attempt":           `{"attempt":2}`,
			"100 --attempt 1 --json jobs":  flakeJobsJSON,
			"100 --attempt 1 --log-failed": failedLogLines("lint", "Run golangci-lint", 150),
		}),
		keepFlakeLog: func(s string) { kept.WriteString(s) },
	}
	steps := d.RecordFlake([]Check{{Name: "lint", Bucket: BucketFail, Link: "https://github.com/o/r/actions/runs/100/job/2"}})

	if got := steps["lint"]; got != "Run golangci-lint" {
		t.Fatalf("steps = %v, want lint's failing step %q", steps, "Run golangci-lint")
	}
	log := kept.String()
	for _, want := range []string{"run 100 attempt 1", "lint", "Run golangci-lint", "line 150", "line 51"} {
		if !strings.Contains(log, want) {
			t.Errorf("flake log is missing %q:\n%s", want, log)
		}
	}
	if strings.Contains(log, "line 50\n") {
		t.Errorf("flake log kept more than the last %d lines of the check's failed log:\n%s", flakeLogTailLines, log)
	}
}

// #371: the record is best-effort. A run gh cannot read leaves no step to name
// and a note in the log, but RecordFlake still returns and never panics, so the
// green verdict the re-run earned stands.
func TestRecordFlakeIsBestEffortWhenGhFails(t *testing.T) {
	var kept strings.Builder
	d := &GhDriver{
		viewRun:      scriptedRunView(nil),
		keepFlakeLog: func(s string) { kept.WriteString(s) },
	}
	steps := d.RecordFlake([]Check{{Name: "lint", Bucket: BucketFail, Link: "https://github.com/o/r/actions/runs/100/job/2"}})
	if len(steps) != 0 {
		t.Fatalf("steps = %v, want none when gh cannot read the run", steps)
	}
	if !strings.Contains(kept.String(), "HTTP 404") {
		t.Errorf("flake log does not say why the record is empty:\n%s", kept.String())
	}
}

// #371: a check with no Actions run behind it (an external status) has nothing
// to fetch, and the driver does not shell out for it.
func TestRecordFlakeSkipsChecksWithoutAnActionsRun(t *testing.T) {
	d := &GhDriver{
		viewRun: func(...string) ([]byte, error) {
			t.Fatal("RecordFlake called gh for a check with no Actions run")
			return nil, nil
		},
		keepFlakeLog: func(string) {},
	}
	if steps := d.RecordFlake([]Check{{Name: "external", Bucket: BucketFail, Link: "https://example.com/status"}}); len(steps) != 0 {
		t.Fatalf("steps = %v, want none", steps)
	}
}
