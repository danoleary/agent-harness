package prompt

import (
	"regexp"
	"strings"
	"testing"
)

const sampleCILogs = "FAIL src/foo.test.ts\n  Expected 1, received 2\n##[error]Process completed with exit code 1"

func TestBuildCIFixNamesWorktreeAndTicket(t *testing.T) {
	p := BuildCIFix(sample, "beh-362", sampleWorktree, sampleCILogs)

	for _, want := range []string{"BEH-362", sampleWorktree, "feat/beh-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
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
