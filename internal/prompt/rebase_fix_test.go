package prompt

import (
	"regexp"
	"strings"
	"testing"
)

// BEH-581: BuildRebaseFix steers the sandboxed session that resolves a genuine
// pre-push content conflict — the branch passed every gate but won't rebase onto
// the origin/main a sibling PR advanced underneath it. It must name the worktree +
// ticket and tell the agent to work in the existing worktree, not create one.
func TestBuildRebaseFixNamesWorktreeAndTicket(t *testing.T) {
	p := BuildRebaseFix(sample, "beh-362", sampleWorktree)

	for _, want := range []string{"BEH-362", sampleWorktree, "feat/beh-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if !regexp.MustCompile(`(?i)do not create a new worktree`).MatchString(p) {
		t.Error("prompt should steer the agent to the existing worktree, not a new one")
	}
}

// The core job: rebase onto origin/main, resolve the conflicts, continue the
// rebase, and commit — leaving a clean, rebased worktree.
func TestBuildRebaseFixSteersTheRebaseAndResolve(t *testing.T) {
	p := BuildRebaseFix(sample, "beh-362", sampleWorktree)

	if !strings.Contains(p, "git rebase origin/main") {
		t.Error("prompt should instruct the agent to rebase onto origin/main")
	}
	if !regexp.MustCompile(`(?i)resolve`).MatchString(p) {
		t.Error("prompt should instruct the agent to resolve the conflicts")
	}
	if !strings.Contains(p, "git rebase --continue") {
		t.Error("prompt should instruct the agent to continue the rebase to completion")
	}
}

// The resolution must preserve BOTH intents — the ticket's change AND the
// incoming changes from main — not blindly take one side.
func TestBuildRebaseFixSteersToPreserveBothIntents(t *testing.T) {
	p := BuildRebaseFix(sample, "beh-362", sampleWorktree)

	if !regexp.MustCompile(`(?i)both`).MatchString(p) {
		t.Error("prompt should tell the agent to preserve both the ticket's and main's intent")
	}
	// The ticket description is injected so the agent knows what intent to preserve.
	if !strings.Contains(p, sample.Description) {
		t.Error("prompt should inject the ticket description as the intent to preserve")
	}
}

// The harness owns all remote I/O (ADR-0002) + all Linear I/O (ADR-0001): the
// session commits the resolution locally and stops — no push, no gh, no Linear,
// no findings. It must NOT abort the rebase as an escape hatch (that would strand
// the branch on its stale base — the very thing this session exists to fix).
func TestBuildRebaseFixForbidsRemoteLinearAndAbortEscape(t *testing.T) {
	p := BuildRebaseFix(sample, "beh-362", sampleWorktree)

	if !regexp.MustCompile(`(?i)do not push`).MatchString(p) {
		t.Error("prompt must forbid pushing (the harness owns remote I/O)")
	}
	if !strings.Contains(p, "mcp__linear-server__") {
		t.Error("prompt must forbid touching Linear")
	}
	if !regexp.MustCompile(`(?i)findings`).MatchString(p) {
		t.Error("prompt must forbid emitting findings")
	}
	if !regexp.MustCompile(`(?i)do not .*abort`).MatchString(p) {
		t.Error("prompt must forbid `git rebase --abort` — aborting strands the branch on its stale base")
	}
}

func TestBuildRebaseFixCarriesBashQuirkSteer(t *testing.T) {
	p := BuildRebaseFix(sample, "beh-362", sampleWorktree)
	if !strings.Contains(p, bashQuirkSteer) {
		t.Error("prompt missing the shared bash-quirk steer")
	}
}
