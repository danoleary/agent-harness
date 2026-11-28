package prompt

import (
	"strings"
	"testing"
)

// #479: a conflict-resolution session whose conflict an earlier run already
// resolved onto an older origin/main is told, before its job, which commit holds
// that resolution and how to read it, so it carries the same hunks over rather
// than re-deriving them. With no prior resolution neither variant mentions one.
func TestRebaseFixNamesAPriorResolutionToReuse(t *testing.T) {
	prior := &PriorResolution{Commit: "9a1b2c3d4e5f60718293a4b5c6d7e8f901234567", Base: "3cd1e9b0aa11bb22cc33dd44ee55ff6677889900"}
	for name, ctx := range map[string]Context{
		"rebase": {Conflicted: []string{"internal/x.go"}},
		"merge":  {Merge: true, Conflicted: []string{"internal/x.go"}},
	} {
		t.Run(name, func(t *testing.T) {
			ctx.Ticket, ctx.Slug, ctx.BranchPrefix, ctx.WorktreePath, ctx.Branch = sample, "pr-1384", "feat", sampleWorktree, "feat/beh-1198"

			if p := For(RebaseFix, ctx); strings.Contains(p, "PRIOR RESOLUTION") {
				t.Errorf("a prompt with no prior resolution must not mention one:\n%s", p)
			}
			ctx.PriorResolution = prior
			p := For(RebaseFix, ctx)
			for _, want := range []string{"PRIOR RESOLUTION", "9a1b2c3d4e5f", "3cd1e9b0aa11", "git diff 3cd1e9b0aa11 9a1b2c3d4e5f", "git show 9a1b2c3d4e5f:<path>"} {
				if !strings.Contains(p, want) {
					t.Errorf("prompt missing %q:\n%s", want, p)
				}
			}
			if job := strings.Index(p, "Your job"); job < 0 || strings.Index(p, "PRIOR RESOLUTION") > job {
				t.Error("the prior resolution must be named before the session reads its job")
			}
		})
	}
}
