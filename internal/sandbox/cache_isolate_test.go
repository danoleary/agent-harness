package sandbox

import (
	"slices"
	"testing"
)

// A gate's cache_isolate paths are each covered by a fresh tmpfs at their path
// in the worktree, so the gate container never reads or writes the copy the
// sandbox session shares through the checkout mount (#314). herd's Vite
// dependency cache is the case: a host gate that rebuilt it under the session's
// feet could fail every story file.
func TestWorktreeCommandArgsMountATmpfsOverEachIsolatedPath(t *testing.T) {
	args := BuildWorktreeCommandArgs(GateConfig{
		Image:        "herd-agent-harness:latest",
		ProjectPath:  "/Users/dan/herd",
		WorktreePath: "/Users/dan/herd/.claude/worktrees/beh-371",
		IsolatePaths: []string{"web/node_modules/.vite", "web/node_modules/.cache/storybook/"},
	}, "pnpm test-storybook")

	want := []string{
		"type=tmpfs,dst=/Users/dan/herd/.claude/worktrees/beh-371/web/node_modules/.vite,tmpfs-mode=1777",
		"type=tmpfs,dst=/Users/dan/herd/.claude/worktrees/beh-371/web/node_modules/.cache/storybook,tmpfs-mode=1777",
	}
	if got := valuesForFlag(args, "--mount"); !slices.Equal(got, want) {
		t.Errorf("--mount values = %q, want %q", got, want)
	}
	// Docker applies mounts in argv order, so the tmpfs must follow the checkout
	// bind it sits inside.
	if slices.Index(args, "--mount") < slices.Index(args, "/Users/dan/herd:/Users/dan/herd") {
		t.Errorf("tmpfs mount precedes the checkout mount: %v", args)
	}
}

// With no cache_isolate the gate's argv is what it always was.
func TestWorktreeCommandArgsMountNothingExtraWithoutIsolatedPaths(t *testing.T) {
	args := BuildWorktreeCommandArgs(GateConfig{Image: "img", ProjectPath: "/p", WorktreePath: "/p/wt"}, "make check")
	if got := valuesForFlag(args, "--mount"); got != nil {
		t.Errorf("--mount values = %q, want none", got)
	}
}
