package prompt

import (
	"strings"
	"testing"
)

// #86: the harness never updates local `main`, so once a sibling ticket merges
// mid-run `main...HEAD` drags that sibling's commits into the session's scope. The
// review and retrospective envelopes name the exact commit the branch was cut from
// (or last rebased onto) and scope the diff to it.
func TestReviewAndRetrospectiveScopeTheDiffToTheBranchBase(t *testing.T) {
	const base = "0123456789abcdef0123456789abcdef01234567"
	for name, p := range map[string]string{
		"review":        For(Review, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix, Body: exampleBody(t, "review"), Base: base}),
		"retrospective": For(Retrospective, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix, Body: exampleBody(t, "retro"), Base: base}),
	} {
		for _, want := range []string{"git diff " + base + "...HEAD", "git log " + base + "..HEAD", "stale"} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt missing %q", name, want)
			}
		}
	}
}

// A base the host could not resolve still steers off the stale local `main`: the
// session falls back to origin/main, whose three-dot diff finds the same fork point.
func TestBranchBaseFallsBackToOriginMainWhenUnresolved(t *testing.T) {
	for name, p := range map[string]string{
		"review":        For(Review, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
		"retrospective": For(Retrospective, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
	} {
		if !strings.Contains(p, "git diff origin/main...HEAD") {
			t.Errorf("%s prompt does not fall back to origin/main", name)
		}
	}
}

// No harness-owned envelope text points a session at local `main`.
func TestNoEnvelopeDiffsAgainstLocalMain(t *testing.T) {
	for name, p := range map[string]string{
		"review":        For(Review, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
		"retrospective": For(Retrospective, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix}),
		"resumed":       For(Implement, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix, Resume: ResumedBranch}),
	} {
		for _, stale := range []string{"`git log main..HEAD`", "`git diff main...HEAD`"} {
			if strings.Contains(p, stale) {
				t.Errorf("%s prompt still names %s", name, stale)
			}
		}
	}
}

// #220: a Review or Retrospective session's cwd is the primary checkout, which can
// sit on an unrelated branch that also descends from the base, so a bare
// `git log <base>..HEAD` there quietly shows a different change. The envelope names
// the worktree in the range commands, and names it and its real branch on a line of
// its own, so neither depends on a Consumer body that may not name it or render.
func TestReviewAndRetrospectiveRunTheBranchRangeInTheWorktree(t *testing.T) {
	const base = "0123456789abcdef0123456789abcdef01234567"
	const wt = "/repo/.claude/worktrees/pr-42"
	c := Context{Ticket: sample, Slug: "pr-42", BranchPrefix: testPrefix, Branch: "fix/login-redirect", WorktreePath: wt, Base: base, Body: ""}
	for name, p := range map[string]string{
		"review":        For(Review, c),
		"retrospective": For(Retrospective, c),
	} {
		for _, want := range []string{
			"git -C " + wt + " log " + base + "..HEAD",
			"git -C " + wt + " diff " + base + "...HEAD",
			"worktree is `" + wt + "` on branch `fix/login-redirect`",
		} {
			if !strings.Contains(p, want) {
				t.Errorf("%s prompt with an empty body missing %q", name, want)
			}
		}
		if strings.Contains(p, "`git log "+base) {
			t.Errorf("%s prompt still names a bare branch range that runs in the primary checkout", name)
		}
	}
}

// A worktree under a path with a space still yields a range command that runs.
func TestBranchRangeQuotesAWorktreePathTheShellWouldSplit(t *testing.T) {
	p := For(Review, Context{Ticket: sample, Slug: "beh-362", BranchPrefix: testPrefix, WorktreePath: "/Users/dan/My Projects/herd/.claude/worktrees/beh-362", Base: "abc123"})
	if want := "git -C '/Users/dan/My Projects/herd/.claude/worktrees/beh-362' log abc123..HEAD"; !strings.Contains(p, want) {
		t.Errorf("prompt missing %q", want)
	}
}
