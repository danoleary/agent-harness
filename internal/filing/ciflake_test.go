package filing

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/tracker"
)

// flakeRecord renders a ci-flake-<run-id>.log as the CI watch's flake washer
// writes it (#371): a run header, each check's failing step, then the tail of
// each job's `--log-failed` log, whose lines gh prefixes "<job>\t<step>\t".
func flakeRecord(check, step string, n int) string {
	var b strings.Builder
	fmt.Fprintf(&b, "===== run 19590 attempt 1 (flake washed out by the re-run) =====\n")
	fmt.Fprintf(&b, "check %s — failing step: %s\n", check, step)
	fmt.Fprintf(&b, "----- %s: last 100 of %d failed-log lines -----\n", check, n)
	for i := 1; i <= n; i++ {
		fmt.Fprintf(&b, "%s\t%s\t2026-11-22T05:41:07.0000000Z log line %d\n", check, step, i)
	}
	fmt.Fprintf(&b, "%s\t%s\tfatal: unable to access 'https://github.com/o/r/': Could not resolve host: github.com\n\n", check, step)
	return b.String()
}

func writeFlakeRecord(t *testing.T, logDir, runID, record string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(logDir, "ci-flake-"+runID+".log"), []byte(record), 0o644); err != nil {
		t.Fatalf("write flake record: %v", err)
	}
}

// #378: the implementation sandbox has no gh and no network (ADR-0002), so a
// filed ci-flaky-check finding has to carry the recorded failed attempt itself:
// the check's failing step and the key failure line from the flake washer's
// record in the worked item's log dir. Replays the incident: the
// "Check, build" check flaked, the retrospective keyed it on the check, and its
// body named no step.
func TestRouteCopiesTheRecordedFlakeLogIntoACIFlakyCheckFinding(t *testing.T) {
	logDir := t.TempDir()
	dir := filepath.Join(logDir, "findings", "retrospective")
	writeFlakeRecord(t, logDir, "20261122-054000-abcd", flakeRecord("Check, build", "Fetch origin/main", 150))
	writeDropbox(t, dir, `[{"title":"Fix the flaky Check, build CI check","body":"Check, build went green only on a re-run.","key":"ci-flaky-check-check-build","audience":"harness"}]`)
	upstreamFiler := &fakeFiler{results: []fakeResult{{issue: tracker.CreatedIssue{Identifier: "#378"}}}}
	up := &Upstream{Sink: sink(upstreamFiler, noExisting(), nil), Container: "o/agent-harness", Project: "agent-harness"}

	Router{Project: sink(&fakeFiler{}, noExisting(), nil), Upstream: up, LogDir: logDir, Log: &recorder{}}.Route(dir, "team", "PR-371")

	if len(upstreamFiler.calls) != 1 {
		t.Fatalf("filed %d findings, want 1", len(upstreamFiler.calls))
	}
	body := upstreamFiler.calls[0].Body
	for _, want := range []string{
		"Check, build went green only on a re-run.",
		"Fetch origin/main",
		"Could not resolve host: github.com",
		"log line 150",
		"ci-flake-20261122-054000-abcd.log",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("filed body is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "log line 100\n") {
		t.Errorf("filed body copied more than a bounded excerpt of the log:\n%s", body)
	}
}

// #378: with no record of the failed attempt, the body says so outright, so the
// session does not plan around a gh step it cannot run.
func TestRouteSaysTheFailingStepIsUnknownWithoutAFlakeRecord(t *testing.T) {
	logDir := t.TempDir()
	dir := filepath.Join(logDir, "findings", "retrospective")
	writeFlakeRecord(t, logDir, "r1", flakeRecord("lint", "Run golangci-lint", 3))
	writeDropbox(t, dir, `[{"title":"Fix the flaky test check","body":"test flaked.","key":"ci-flaky-check-test","audience":"project"}]`)
	filer := &fakeFiler{}

	Router{Project: sink(filer, noExisting(), nil), LogDir: logDir, Log: &recorder{}}.Route(dir, "team", "PR-9")

	if len(filer.calls) != 1 {
		t.Fatalf("filed %d findings, want 1", len(filer.calls))
	}
	body := filer.calls[0].Body
	for _, want := range []string{"failing step is unknown", "cannot be fetched from the sandbox"} {
		if !strings.Contains(body, want) {
			t.Errorf("filed body is missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "golangci-lint") {
		t.Errorf("filed body copied another check's record:\n%s", body)
	}
}

// Only a ci-flaky-check finding gets the record: any other finding files as the
// session wrote it.
func TestRouteLeavesOtherFindingsBodiesAlone(t *testing.T) {
	logDir := t.TempDir()
	dir := filepath.Join(logDir, "findings", "retrospective")
	writeFlakeRecord(t, logDir, "r1", flakeRecord("lint", "Run golangci-lint", 3))
	writeDropbox(t, dir, `[{"title":"sandbox missing deps","body":"libnss3","key":"sandbox-deps","audience":"project"}]`)
	filer := &fakeFiler{}

	Router{Project: sink(filer, noExisting(), nil), LogDir: logDir, Log: &recorder{}}.Route(dir, "team", "PR-9")

	if len(filer.calls) != 1 || filer.calls[0].Body != "libnss3" {
		t.Errorf("filed %+v, want the body untouched", filer.calls)
	}
}
