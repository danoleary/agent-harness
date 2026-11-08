package prompt

import (
	"strings"
	"testing"
)

// #246, herd PR-1384: a fix session that starts from an earlier attempt's saved
// diff is told, before its job, that the checkpoint exists, which commit it is on
// top of which tip, that it is unverified and can be wrong, and how to drop it.
// Every fix envelope says so; with no checkpoint none of them mentions one.
func TestFixPromptsNameAResumedCheckpointAsUnverified(t *testing.T) {
	cp := &Checkpoint{Commit: "9a1b2c3d4e5f60718293a4b5c6d7e8f901234567", Base: "3cd1e9b0aa11bb22cc33dd44ee55ff6677889900"}
	for name, ctx := range map[string]Context{
		"ci fix": {CILogs: "FAIL more-tray.stories.tsx", CILogAvailable: true},
		"rebase": {},
		"merge":  {Merge: true},
	} {
		t.Run(name, func(t *testing.T) {
			stage := RebaseFix
			if name == "ci fix" {
				stage = CIFix
			}
			ctx.Ticket, ctx.Slug, ctx.BranchPrefix, ctx.WorktreePath, ctx.Branch = sample, "pr-1384", "feat", sampleWorktree, "feat/beh-1198"

			if p := For(stage, ctx); strings.Contains(p, "checkpoint") {
				t.Errorf("a prompt with no checkpoint must not mention one:\n%s", p)
			}
			ctx.Checkpoint = cp
			p := For(stage, ctx)
			for _, want := range []string{"UNVERIFIED", "9a1b2c3d4e5f", "3cd1e9b0aa11", "can be wrong", "git show 9a1b2c3d4e5f", "git reset --hard 3cd1e9b0aa11"} {
				if !strings.Contains(p, want) {
					t.Errorf("prompt missing %q:\n%s", want, p)
				}
			}
			if job := strings.Index(p, "Your job"); job < 0 || strings.Index(p, "UNVERIFIED") > job {
				t.Error("the checkpoint must be named before the session reads its job")
			}
		})
	}
}
