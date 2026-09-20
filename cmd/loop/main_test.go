package main

import (
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
)

// TestParsePrunedCount reads the removed-worktree count from the prune script's
// authoritative summary line ("Pruned N worktree(s).") — the count the loop narrates
// — tolerating the per-removal and tip lines around it, and degrading to 0 when the
// script pruned nothing or printed no summary.
func TestParsePrunedCount(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   int
	}{
		{
			name: "two removed",
			output: "Merged + clean worktree(s) eligible for removal:\n" +
				"  • /h/.claude/worktrees/a\n" +
				"  • /h/.claude/worktrees/b\n" +
				"removed /h/.claude/worktrees/a\n" +
				"removed /h/.claude/worktrees/b\n" +
				"Pruned 2 worktree(s). Tip: 'pnpm store prune' frees orphaned content in the global pnpm store too.\n",
			want: 2,
		},
		{
			name:   "nothing to prune",
			output: "Nothing to prune — no merged + clean worktree under /h/.claude/worktrees.\n",
			want:   0,
		},
		{
			name:   "empty",
			output: "",
			want:   0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := parsePrunedCount(tc.output); got != tc.want {
				t.Errorf("parsePrunedCount(%q) = %d, want %d", tc.output, got, tc.want)
			}
		})
	}
}

// --- the cache prune is Consumer-declared (BEH-641) ---
//
// The secondary disk-reclaim rung ran a hardcoded `pnpm store prune` against
// every Consumer, whatever its toolchain. It is `cache.prune_command` now, the
// missing third piece of the cache surface BEH-635 generalized. Nil is the
// loop's own "skip this rung" signal, so a Consumer with nothing to prune must
// produce nil rather than a thunk that shells out to someone else's package
// manager and logs a warning every reclaim cycle.
func TestCachePruneForSkipsWhenNoCommandDeclared(t *testing.T) {
	if got := cachePruneFor(config.Config{ProjectPath: "/p"}); got != nil {
		t.Error("a Consumer that declares no cache.prune_command must skip the rung, got a thunk")
	}
}

func TestCachePruneForRunsTheDeclaredCommand(t *testing.T) {
	if got := cachePruneFor(config.Config{ProjectPath: "/p", CachePruneCommand: "go clean -modcache"}); got == nil {
		t.Error("a declared cache.prune_command must produce a prune thunk")
	}
}
