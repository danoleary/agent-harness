package prompt

import (
	"regexp"
	"strings"
	"testing"

	"github.com/beherd/agent-harness/internal/ci"
)

const sampleCILogs = "FAIL src/foo.test.ts\n  Expected 1, received 2\n##[error]Process completed with exit code 1"

// sampleUnfetchableLogs is what fetchFailedLogs returns when `gh run view
// --log-failed` couldn't retrieve any step output (run cancelled/superseded or
// expired): just the run header plus the folded-in gh error, no real failure
// lines (BEH-560). Built from ci's exported marker constants so a marker rename
// in the driver moves this fixture with the predicate instead of leaving a stale
// literal that reds these tests for the wrong reason (BEH-563).
const sampleUnfetchableLogs = ci.RunHeaderMarker + "456 (failed steps) =====\n\n" + ci.FetchErrorMarker + "456: log not found)\n"

// claimsLogsWereFetched is the assertion the prompt must NOT make when the fetch
// returned nothing fetchable — it framed the empty payload as the real logs and
// sent both BEH-507 cifix sessions blind-reproducing every gate.
const claimsLogsWereFetched = "Here are the failing CI job logs the harness fetched"

func TestBuildCIFixEmptyLogsDoesNotClaimLogsWereFetched(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleUnfetchableLogs)

	if strings.Contains(p, claimsLogsWereFetched) {
		t.Error("prompt asserts the empty/unfetchable payload IS the fetched logs")
	}
}

func TestBuildCIFixNamesWorktreeAndTicket(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	for _, want := range []string{"BEH-362", sampleWorktree, "feat/beh-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

func TestBuildCIFixEmptyLogsFramesRunAsLikelyNotAFailure(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleUnfetchableLogs)

	if !regexp.MustCompile(`(?i)(could not|couldn't|no).{0,40}(fetch|logs)`).MatchString(p) {
		t.Error("prompt does not tell the agent up-front the logs could not be fetched")
	}
	if !regexp.MustCompile(`(?i)cancel|supersed|expired`).MatchString(p) {
		t.Error("prompt does not frame the run as likely cancelled/superseded/expired rather than a code defect")
	}
}

func TestBuildCIFixEmptyLogsSteersOffBlindGateReproduction(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleUnfetchableLogs)

	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,40}(reproduce|re-?run).{0,40}(every|all|gate)`).MatchString(p) {
		t.Error("prompt does not steer the agent off blindly reproducing every gate")
	}
	// The empty-log branch must NOT instruct "read those logs" — there are none.
	if strings.Contains(p, "read those logs") {
		t.Error("prompt tells the agent to read logs that were never fetched")
	}
}

// The no-runs sentinel (no Actions run ids at all) is just as unfetchable as a
// folded-in gh error — both must take the cancelled/superseded framing.
func TestBuildCIFixNoRunSentinelIsUnfetchable(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, "(no GitHub Actions run logs available for the failing checks)")

	if strings.Contains(p, claimsLogsWereFetched) {
		t.Error("no-runs sentinel still framed as fetched logs")
	}
}

// Real step output must keep the original framing untouched — the empty-log
// branch must not swallow a genuine failure.
func TestBuildCIFixRealLogsKeepFetchedFraming(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	if !strings.Contains(p, claimsLogsWereFetched) {
		t.Error("real logs no longer use the 'here are the fetched logs' framing")
	}
	if !strings.Contains(p, "read those logs") {
		t.Error("real logs no longer tell the agent to read the logs")
	}
}

// A partial fetch (one run's logs retrieved, another folded in an error) still
// carries real output, so it must NOT take the unfetchable branch.
func TestBuildCIFixPartialFetchIsFetchable(t *testing.T) {
	partial := "===== run 1 (failed steps) =====\nFAIL src/foo.test.ts\n  Expected 1, received 2\n===== run 2 (failed steps) =====\n\n(could not fully fetch logs for run 2: log not found)\n"
	p := BuildCIFix(sample, "beh-362", sampleWorktree, partial)

	if !strings.Contains(p, claimsLogsWereFetched) {
		t.Error("a partial fetch with real output was wrongly framed as unfetchable")
	}
}

func TestBuildCIFixInjectsFailingLogs(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	if !strings.Contains(p, "src/foo.test.ts") || !strings.Contains(p, "exit code 1") {
		t.Error("prompt does not inject the failing CI logs the agent must diagnose")
	}
	if !regexp.MustCompile(`(?i)(ci|continuous integration|github)`).MatchString(p) {
		t.Error("prompt does not frame the task as a CI failure")
	}
}

func TestBuildCIFixCommitsLocallyAndForbidsRemote(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	if !regexp.MustCompile(`(?i)commit.{0,30}local`).MatchString(p) {
		t.Error("prompt does not tell the agent to commit the fix locally")
	}
	// The harness owns the push (ADR-0002): the agent must not push or run gh.
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,20}push`).MatchString(p) {
		t.Error("prompt does not forbid pushing")
	}
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,20}(run )?gh\b`).MatchString(p) {
		t.Error("prompt does not forbid running gh")
	}
}

func TestBuildCIFixTellsAgentToReRunTheGate(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	// The agent should reproduce/verify locally where it can before handing back.
	if !regexp.MustCompile(`(?i)(re-?run|reproduce|verify).{0,40}(gate|test|lint|check|local)`).MatchString(p) {
		t.Error("prompt does not tell the agent to reproduce/re-run the gate locally")
	}
}

func TestBuildCIFixSteersOffLinearAndFindings(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	if !regexp.MustCompile(`(?i)(do not|don't).*Linear`).MatchString(p) {
		t.Error("prompt does not steer off Linear")
	}
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,30}finding`).MatchString(p) {
		t.Error("prompt does not steer off emitting findings")
	}
}

func TestBuildCIFixCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs), "ci-fix prompt")
}

// The escape hatch (BEH-561): when every gate reproduces green locally and the red
// is a cancelled/superseded/flaky run rather than a code defect, the agent must NOT
// manufacture a speculative diff to satisfy the loop — it should make no commit at
// all; the harness re-triggers CI itself.
func TestBuildCIFixOffersNoOpEscapeHatch(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	// Forbids fabricating a commit just to re-push.
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,60}(speculative|fabricat|manufactur|invent|unrelated)`).MatchString(p) {
		t.Error("prompt does not forbid manufacturing a speculative commit")
	}
	// Tells the agent that making no commit is a valid outcome and the harness re-runs CI.
	if !regexp.MustCompile(`(?i)(no commit|do not commit|don't commit|without a commit|make no)`).MatchString(p) {
		t.Error("prompt does not tell the agent that committing nothing is acceptable")
	}
	if !regexp.MustCompile(`(?i)(re-?trigger|re-?run).{0,40}ci|ci.{0,40}(re-?trigger|re-?run)`).MatchString(p) {
		t.Error("prompt does not say the harness re-triggers CI on a no-op")
	}
}
