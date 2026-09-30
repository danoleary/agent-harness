package prompt

import (
	"regexp"
	"strings"
	"testing"
)

// BEH-581: the RebaseFix prompt steers the sandboxed session that resolves a genuine
// pre-push content conflict — the branch passed every gate but won't rebase onto
// the origin/main a sibling PR advanced underneath it. It must name the worktree +
// ticket and tell the agent to work in the existing worktree, not create one.
func TestRebaseFixNamesWorktreeAndTicket(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	for _, want := range []string{"BEH-362", sampleWorktree, "feat/beh-362"} {
		if !strings.Contains(p, want) {
			t.Errorf("prompt missing %q", want)
		}
	}
	if !regexp.MustCompile(`(?i)do not create a new worktree`).MatchString(p) {
		t.Error("prompt should steer the agent to the existing worktree, not a new one")
	}
}

// BEH-675: the worktree this session rebases was created with the Consumer's
// configured branch prefix, not a hardcoded `feat`. A non-`feat` prefix must flow
// into the prompt so it names the real branch instead of a non-existent
// `feat/<slug>`.
func TestRebaseFixNamesBranchWithConfiguredPrefix(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "wip", WorktreePath: sampleWorktree})

	if !strings.Contains(p, "wip/beh-362") {
		t.Error("prompt does not name the branch with the configured prefix (wip/beh-362)")
	}
	if strings.Contains(p, "feat/beh-362") {
		t.Error("prompt still hardcodes feat/beh-362 instead of using the configured prefix")
	}
}

// BEH-618: the core job is to replay onto origin/main, resolve the conflicts,
// continue the replay, and commit — leaving a clean, rebased worktree. It must steer
// the agent AWAY from bare `git rebase`, which false-fails ("local changes would be
// overwritten" / "could not detach HEAD") in a linked worktree even on a clean tree.
// The known-good recipe is `reset --hard origin/main` + `cherry-pick` (continued with
// `cherry-pick --continue`), which the session runs in the same linked worktree.
func TestRebaseFixSteersTheCherryPickReplay(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	if !strings.Contains(p, "git reset --hard origin/main") {
		t.Error("prompt should instruct the agent to move onto the fresh base with `git reset --hard origin/main`")
	}
	if !strings.Contains(p, "git cherry-pick") {
		t.Error("prompt should instruct the agent to replay the feature commits via `git cherry-pick`")
	}
	if !strings.Contains(p, "git cherry-pick --continue") {
		t.Error("prompt should instruct the agent to continue the cherry-pick to completion")
	}
	if !regexp.MustCompile(`(?i)resolve`).MatchString(p) {
		t.Error("prompt should instruct the agent to resolve the conflicts")
	}
	// It must explain WHY not bare `git rebase` so the agent doesn't fall back to it
	// and re-derive the linked-worktree false-fail from scratch (the BEH-618 cost).
	if !regexp.MustCompile(`(?i)linked worktree`).MatchString(p) {
		t.Error("prompt should explain that `git rebase` false-fails in a linked worktree")
	}
	if strings.Contains(p, "git rebase --continue") {
		t.Error("prompt must not steer the agent into a bare `git rebase` (BEH-618: it false-fails in a linked worktree)")
	}
}

// The resolution must preserve BOTH intents — the ticket's change AND the
// incoming changes from main — not blindly take one side.
func TestRebaseFixSteersToPreserveBothIntents(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	if !regexp.MustCompile(`(?i)both`).MatchString(p) {
		t.Error("prompt should tell the agent to preserve both the ticket's and main's intent")
	}
	// The ticket description is injected so the agent knows what intent to preserve.
	if !strings.Contains(p, sample.Description) {
		t.Error("prompt should inject the ticket description as the intent to preserve")
	}
}

// The harness owns all remote I/O (ADR-0002) + all tracker I/O (ADR-0010): the
// session commits the resolution locally and stops — no push, no gh, no tracker,
// no findings. It must NOT abort the rebase as an escape hatch (that would strand
// the branch on its stale base — the very thing this session exists to fix).
func TestRebaseFixForbidsRemoteTrackerAndAbortEscape(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	if !regexp.MustCompile(`(?i)do not push`).MatchString(p) {
		t.Error("prompt must forbid pushing (the harness owns remote I/O)")
	}
	if !regexp.MustCompile(`(?i)do not touch the issue tracker`).MatchString(p) {
		t.Error("prompt must forbid touching the tracker")
	}
	if !regexp.MustCompile(`(?i)findings`).MatchString(p) {
		t.Error("prompt must forbid emitting findings")
	}
	if !regexp.MustCompile(`(?i)do not .*abort`).MatchString(p) {
		t.Error("prompt must forbid `git rebase --abort` — aborting strands the branch on its stale base")
	}
}

// BEH-617: the untracked .worktree-ready sentinel (BEH-549) blocks `git rebase`'s
// checkout phase ("untracked working tree files would be overwritten by checkout").
// The host-side rebase now strips it first, but if a resolution session does still
// run, the prompt must tell the agent to `rm -f .worktree-ready` rather than
// rediscover the abort by hand.
func TestRebaseFixWarnsAboutReadySentinel(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	if !strings.Contains(p, ".worktree-ready") {
		t.Error("prompt should name the .worktree-ready sentinel that can block the rebase checkout")
	}
	if !strings.Contains(p, "rm -f .worktree-ready") {
		t.Error("prompt should tell the agent to remove the sentinel with `rm -f .worktree-ready`")
	}
}

func TestRebaseFixCarriesBashQuirkSteer(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})
	if !strings.Contains(p, bashQuirkSteer) {
		t.Error("prompt missing the shared bash-quirk steer")
	}
}

// BEH-622: a feature commit whose diff is already present identically in
// origin/main makes the replay "now empty", and `git cherry-pick --continue`
// reports "the previous cherry-pick is now empty" — NOT a content conflict. The
// prompt must steer the agent to `git cherry-pick --skip` in that case (dropping
// the redundant commit and continuing), so it doesn't misread the empty state as
// an unresolvable conflict and give up. Distinct from the genuine-conflict steer.
func TestRebaseFixSteersEmptyCommitSkip(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	if !regexp.MustCompile(`(?i)now empty`).MatchString(p) {
		t.Error("prompt should name the \"the previous cherry-pick is now empty\" state that a redundant commit produces")
	}
	if !strings.Contains(p, "git cherry-pick --skip") {
		t.Error("prompt should tell the agent to `git cherry-pick --skip` when a commit is already present in origin/main (not treat it as a conflict)")
	}
}
