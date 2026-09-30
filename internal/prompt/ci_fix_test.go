package prompt

import (
	"regexp"
	"strings"
	"testing"

	"github.com/danoleary/agent-harness/internal/ci"
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

func TestCIFixEmptyLogsDoesNotClaimLogsWereFetched(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleUnfetchableLogs})

	if strings.Contains(p, claimsLogsWereFetched) {
		t.Error("prompt asserts the empty/unfetchable payload IS the fetched logs")
	}
}

func TestCIFixNamesWorktreeAndTicket(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	for _, want := range []string{"BEH-362", sampleWorktree, "feat/beh-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
}

// BEH-675: the branch the fix session works on was created with the Consumer's
// configured branch prefix (`new-worktree.sh <slug> <prefix>`), not a hardcoded
// `feat`. A non-`feat` prefix must flow into the prompt so it names the real
// branch — both in the top-line worktree framing and the "commit on <branch>"
// steer — instead of a non-existent `feat/<slug>`.
func TestCIFixNamesBranchWithConfiguredPrefix(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "wip", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	if !strings.Contains(p, "wip/beh-362") {
		t.Error("prompt does not name the branch with the configured prefix (wip/beh-362)")
	}
	if strings.Contains(p, "feat/beh-362") {
		t.Error("prompt still hardcodes feat/beh-362 instead of using the configured prefix")
	}
	// With a fetched log the branch is named twice: the top-line worktree framing
	// and the fetchable-case "commit on <branch>" steer. Both must carry the prefix.
	if strings.Count(p, "wip/beh-362") < 2 {
		t.Error("prompt names wip/beh-362 in only one place — the commit steer still hardcodes feat/")
	}
}

func TestCIFixEmptyLogsFramesRunAsLikelyNotAFailure(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleUnfetchableLogs})

	if !regexp.MustCompile(`(?i)(could not|couldn't|no).{0,40}(fetch|logs)`).MatchString(p) {
		t.Error("prompt does not tell the agent up-front the logs could not be fetched")
	}
	if !regexp.MustCompile(`(?i)cancel|supersed|expired`).MatchString(p) {
		t.Error("prompt does not frame the run as likely cancelled/superseded/expired rather than a code defect")
	}
}

func TestCIFixEmptyLogsSteersOffBlindGateReproduction(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleUnfetchableLogs})

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
func TestCIFixNoRunSentinelIsUnfetchable(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: "(no GitHub Actions run logs available for the failing checks)"})

	if strings.Contains(p, claimsLogsWereFetched) {
		t.Error("no-runs sentinel still framed as fetched logs")
	}
}

// Real step output must keep the original framing untouched — the empty-log
// branch must not swallow a genuine failure.
func TestCIFixRealLogsKeepFetchedFraming(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	if !strings.Contains(p, claimsLogsWereFetched) {
		t.Error("real logs no longer use the 'here are the fetched logs' framing")
	}
	if !strings.Contains(p, "read those logs") {
		t.Error("real logs no longer tell the agent to read the logs")
	}
}

// A partial fetch (one run's logs retrieved, another folded in an error) still
// carries real output, so it must NOT take the unfetchable branch.
func TestCIFixPartialFetchIsFetchable(t *testing.T) {
	partial := "===== run 1 (failed steps) =====\nFAIL src/foo.test.ts\n  Expected 1, received 2\n===== run 2 (failed steps) =====\n\n(could not fully fetch logs for run 2: log not found)\n"
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: partial, CILogAvailable: true})

	if !strings.Contains(p, claimsLogsWereFetched) {
		t.Error("a partial fetch with real output was wrongly framed as unfetchable")
	}
}

func TestCIFixInjectsFailingLogs(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	if !strings.Contains(p, "src/foo.test.ts") || !strings.Contains(p, "exit code 1") {
		t.Error("prompt does not inject the failing CI logs the agent must diagnose")
	}
	if !regexp.MustCompile(`(?i)(ci|continuous integration|github)`).MatchString(p) {
		t.Error("prompt does not frame the task as a CI failure")
	}
}

func TestCIFixCommitsLocallyAndForbidsRemote(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

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

func TestCIFixTellsAgentToReRunTheGate(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	// The agent should reproduce/verify locally where it can before handing back.
	if !regexp.MustCompile(`(?i)(re-?run|reproduce|verify).{0,40}(gate|test|lint|check|local)`).MatchString(p) {
		t.Error("prompt does not tell the agent to reproduce/re-run the gate locally")
	}
}

func TestCIFixSteersOffTrackerAndFindings(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	if !regexp.MustCompile(`(?i)do not touch the issue tracker`).MatchString(p) {
		t.Error("prompt does not steer off the tracker")
	}
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,30}finding`).MatchString(p) {
		t.Error("prompt does not steer off emitting findings")
	}
}

func TestCIFixCarriesBashQuirkSteer(t *testing.T) {
	assertCarriesBashQuirkSteer(t, For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true}), "ci-fix prompt")
}

// When the harness could not fetch the failing step's log (it expired, or the
// step was an infra-level kill), that fact is the strongest signal the failure
// is not a deterministic code defect. The prompt must surface it as a warning so
// the agent doesn't assume a real, reproducible failure exists (BEH-558).
func TestCIFixWarnsWhenLogUnavailable(t *testing.T) {
	const unfetchable = "===== run 83755977095 (failed steps) =====\n(could not fully fetch logs for run 83755977095: log not found: 83755977095)"
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: unfetchable})

	if !regexp.MustCompile(`(?i)(could not|couldn't|unable to|un)fetch`).MatchString(p) {
		t.Error("prompt does not warn that the failing-step log was unfetchable")
	}
	if !regexp.MustCompile(`(?i)flake`).MatchString(p) {
		t.Error("prompt does not tell the agent an unfetchable log strongly implies a flake")
	}
}

// With no usable log, the agent must reproduce only the single failing gate and
// early-exit as a flake rather than exhaustively re-running every PR gate — the
// ~95-turn phantom chase BEH-558 was filed against.
func TestCIFixEarlyExitsWhenLogUnavailable(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: "(no log)"})

	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,40}(every|all|each|exhaust)`).MatchString(p) {
		t.Error("prompt does not steer the agent off exhaustively re-running every gate")
	}
	if !regexp.MustCompile(`(?i)flake.{0,30}(no fix|stop)`).MatchString(p) {
		t.Error("prompt does not give the early-exit 'flake — no fix' rule")
	}
}

// The flake warning must NOT appear when the harness fetched a real log — a false
// "this is probably a flake" steer would invite the agent to skip a genuine fix.
func TestCIFixOmitsFlakeWarningWhenLogAvailable(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	if regexp.MustCompile(`(?i)flake`).MatchString(p) {
		t.Error("prompt raises a flake warning even though the failing log was available")
	}
}

// BEH-562: the sandbox has no network and no `gh`, and the harness already
// fetched (or failed to fetch) the logs above. Without saying so, the agent
// burns a turn discovering the dead end (`gh: command not found`) before
// falling back to local reproduction. The prompt must state it up front.
func TestCIFixStatesNoNetworkOrGhInSandbox(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

	if !regexp.MustCompile(`(?i)no network`).MatchString(p) {
		t.Error("prompt does not state the sandbox has no network")
	}
	// Don't make the agent try to fetch the logs itself — `gh`/`git fetch` are
	// unavailable; the logs above are all it gets.
	if !regexp.MustCompile(`(?i)git fetch`).MatchString(p) {
		t.Error("prompt does not forbid trying to fetch logs with git fetch")
	}
	if !regexp.MustCompile(`(?i)(do not|don't|never).{0,60}(fetch|try).{0,40}(log|gh|git fetch)`).MatchString(p) {
		t.Error("prompt does not tell the agent not to try fetching the logs itself")
	}
}

// The escape hatch (BEH-561): when every gate reproduces green locally and the red
// is a cancelled/superseded/flaky run rather than a code defect, the agent must NOT
// manufacture a speculative diff to satisfy the loop — it should make no commit at
// all; the harness re-triggers CI itself.
func TestCIFixOffersNoOpEscapeHatch(t *testing.T) {
	p := For(CIFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, CILogs: sampleCILogs, CILogAvailable: true})

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
