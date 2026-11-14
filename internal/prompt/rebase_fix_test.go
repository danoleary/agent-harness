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

// ADR-0019: the PR loop fixes a branch it did not name — a human's opt-in branch
// carries no harness prefix — so an explicit Branch replaces `<prefix>/<slug>` in
// both fix prompts.
func TestFixPromptsNameAnExplicitBranch(t *testing.T) {
	ctx := Context{Ticket: sample, Slug: "pr-12", BranchPrefix: "feat", Branch: "alice/widget", WorktreePath: sampleWorktree}
	for name, p := range map[string]string{"RebaseFix": For(RebaseFix, ctx), "CIFix": For(CIFix, ctx)} {
		if !strings.Contains(p, "`alice/widget`") {
			t.Errorf("%s does not name the explicit branch alice/widget", name)
		}
		if strings.Contains(p, "feat/pr-12") {
			t.Errorf("%s still derives feat/pr-12 when a branch was named", name)
		}
	}
}

// ADR-0019: on a branch a human opted in, the conflict is resolved by merging
// origin/main into the branch, never by rewriting its history — the harness pushes
// the result without force.
func TestRebaseFixMergeVariantKeepsHistory(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "pr-12", Branch: "alice/widget", WorktreePath: sampleWorktree, Merge: true})

	if !strings.Contains(p, "git merge --no-ff origin/main") {
		t.Error("merge variant must tell the agent to merge origin/main into the branch")
	}
	if strings.Contains(p, "git reset --hard origin/main") || strings.Contains(p, "cherry-pick origin/main..") {
		t.Error("merge variant must not carry the replay recipe that rewrites history")
	}
	if !regexp.MustCompile(`(?i)must NOT be rewritten`).MatchString(p) {
		t.Error("merge variant must forbid rewriting the human's history")
	}
}

// A loop's fix step may supply a Project body (ADR-0019, ADR-0009). It leads
// every fix prompt and the envelope follows it whole; with no body each prompt is
// the envelope alone, as before.
func TestFixPromptsLeadWithALoopsFixBody(t *testing.T) {
	for _, stage := range []Stage{RebaseFix, CIFix} {
		for _, merge := range []bool{false, true} {
			base := Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, Merge: merge}
			withBody := base
			withBody.Body = "Run `make lint` on {{.Slug}} before you commit."

			bare, led := For(stage, base), For(stage, withBody)

			if !strings.HasPrefix(led, "Run `make lint` on beh-362 before you commit.\n\n---\n\n") {
				t.Errorf("%v merge=%v: the body must lead, rendered:\n%.200s", stage, merge, led)
			}
			if !strings.HasSuffix(led, bare) {
				t.Errorf("%v merge=%v: the whole envelope must follow the body", stage, merge)
			}
		}
	}
}

// #288: a conflict-resolution session that ran the project's full gate before it
// stopped paid that gate twice in a row, because the harness re-gates the same HEAD
// host-side as soon as the session exits. Both variants must say to check only the
// touched packages and leave the full gate to the host.
func TestRebaseFixLeavesTheFullGateToTheHost(t *testing.T) {
	const want = "After resolving, compile and run the tests of the packages you touched, and stop there. Do NOT run the project's full gate command: the harness runs it host-side on the resulting HEAD right after you stop and blocks the push if it is red, so running it here only pays the same gate twice."
	for _, merge := range []bool{false, true} {
		p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, Merge: merge})
		if !strings.Contains(p, want) {
			t.Errorf("merge=%v: prompt must leave the full gate to the host; missing:\n%s", merge, want)
		}
	}
}

// #253: two sessions on the same red origin/main did opposite things — one left
// the base breakage alone and said nothing, the other repaired it in a package the
// branch never touched and folded that into an unrelated feature commit. Both
// variants must say to check origin/main on its own, leave a red base alone and
// report BASE RED with what failed, fixing only what the branch broke.
func TestRebaseFixLeavesARedBaseAloneAndReportsIt(t *testing.T) {
	for _, merge := range []bool{false, true} {
		p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree, Merge: merge})
		for _, want := range []string{
			"git worktree add --detach",
			"the one exception to not creating a new worktree",
			"Do NOT fix the base breakage",
			"`BASE RED`",
			"failing package or test",
			"Fix only what this branch's own changes broke",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("merge=%v: prompt must carry the red-base rule; missing %q", merge, want)
			}
		}
	}
}

// #255: textual conflicts are not the only overlap a replay can surface. main may
// have gained, in the files the branch touches, the very mechanism the branch
// built for itself (an interface method, a Disposition, a helper). The replay
// variant asks the session to list what main added since the old base there,
// switch the branch to main's version when that is small and mechanical, and
// report the rest in the overlap file the host reads. The merge variant is
// unchanged.
func TestRebaseFixReconcilesWhatMainAddedUnderneathTheBranch(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	for _, want := range []string{
		"git diff --name-only origin/main HEAD",
		"git diff _premerge...origin/main --",
		"interface methods",
		"small and mechanical",
		"/findings/" + OverlapsFile,
		`"file"`, `"main_added"`, `"branch_duplicates"`,
	} {
		if !strings.Contains(p, want) {
			t.Errorf("replay prompt lacks %q", want)
		}
	}
	if strings.Index(p, "/findings/"+OverlapsFile) > strings.Index(p, "git branch -D _premerge") {
		t.Error("the overlap step must come before the marker it diffs against is dropped")
	}

	m := For(RebaseFix, Context{Ticket: sample, Slug: "pr-12", Branch: "alice/widget", WorktreePath: sampleWorktree, Merge: true})
	if strings.Contains(m, OverlapsFile) {
		t.Error("the merge variant is unchanged: it writes no overlap file (#255)")
	}
}

// #347: a PR the PR loop replayed three times took an ADR number main had just
// landed each time, and each resolution renumbered it in a NEW commit on top.
// The ADR index merges by line union, so the next replay cherry-picked the
// adding commit's index line, and every earlier renumber's, back in beside the
// current one: dead links, one more per replay, that each session deleted by hand
// (one lost a turn to a sed pattern that never matched). The replay variant must
// say to renumber inside the commit that added the ADR, to skip an earlier
// renumber commit, and, as a backstop, to check every index line names a file
// and delete a stale one with a fixed-string filter. The merge variant replays
// nothing and may not amend, so it carries none of it.
func TestRebaseFixFoldsAnADRRenumberIntoTheCommitThatAddedIt(t *testing.T) {
	p := For(RebaseFix, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: "feat", WorktreePath: sampleWorktree})

	for _, want := range []string{
		"INSIDE the commit that added it, never in a new commit on top",
		"git diff --name-only --diff-filter=A origin/main..._premerge -- docs/adr/",
		"git log --format=%H --diff-filter=A origin/main.._premerge -- 'docs/adr/*-<slug>.md'",
		"git cherry-pick origin/main..<adding commit>",
		"git commit --amend --no-edit",
		"git cherry-pick <adding commit>.._premerge",
		"only renumbered that ADR",
		"every line of the ADR index names a file that exists",
		"grep -vF",
	} {
		if !strings.Contains(p, want) {
			t.Errorf("replay prompt lacks %q", want)
		}
	}
	if strings.Index(p, "git commit --amend --no-edit") > strings.Index(p, "git branch -D _premerge") {
		t.Error("the renumber names _premerge, so it must come before the marker is dropped")
	}

	m := For(RebaseFix, Context{Ticket: sample, Slug: "pr-12", Branch: "alice/widget", WorktreePath: sampleWorktree, Merge: true})
	if strings.Contains(m, "git commit --amend") {
		t.Error("the merge variant must not tell a human's branch to amend (ADR-0019)")
	}
}
