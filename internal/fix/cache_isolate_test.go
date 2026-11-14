package fix

import (
	"slices"
	"testing"

	"github.com/danoleary/agent-harness/internal/config"
)

// A gate's cache_isolate list travels with its run, so every host gate run —
// the base gate, the review gate and a PR loop re-gate all build theirs here —
// isolates the same paths (#314).
func TestGateRunCarriesTheGatesCacheIsolatePaths(t *testing.T) {
	g := config.Gate{Name: "storybook", Command: "pnpm test-storybook", CacheIsolate: []string{"web/node_modules/.vite"}}

	run := GateRun(config.Config{}, "/wt", "gate", g)

	if !slices.Equal(run.Isolate, g.CacheIsolate) {
		t.Errorf("run.Isolate = %q, want %q", run.Isolate, g.CacheIsolate)
	}
}
