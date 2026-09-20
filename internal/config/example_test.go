// Tests for the worked example Consumer in `example/`.
//
// The example is what docs/CONSUMER.md walks through and what a new project
// copies, so it must parse and pass validation against the real loader. An
// example that drifts fails CI rather than misleading the next person.
//
// Unlike herd_committed_test.go, these resolve a path INSIDE this module, so they
// keep working once the harness is extracted to its own repo (ADR-0007).
package config

import (
	"path/filepath"
	"testing"
)

// exampleConsumer is the worked example's checkout root, as a Consumer checkout
// path — the same shape the harness passes at run time.
func exampleConsumer() string { return filepath.Join("..", "..", "example") }

func TestExampleConsumerConfigLoads(t *testing.T) {
	pc, err := LoadProject(exampleConsumer())
	if err != nil {
		t.Fatalf("example Consumer config failed to load: %v", err)
	}

	if pc.Image == "" || pc.Tracker.Kind == "" || len(pc.Gates) == 0 {
		t.Errorf("example config is under-populated: %+v", pc)
	}
	// The example demonstrates the BUILD path, so it must declare `dockerfile`.
	// Dropping it would flip the example to a doomed pull of an unpublished tag.
	if pc.Dockerfile == "" {
		t.Error("example config must declare `dockerfile` to demonstrate the build path")
	}
	if pc.PostCreate == "" {
		t.Error("example config must declare a `post_create` toolchain-setup hook")
	}
	if pc.Cache.Volume == "" || pc.Cache.Path == "" {
		t.Errorf("example config must resolve a cache volume + path, got %+v", pc.Cache)
	}
	// Asserted through the LOADER, not by reading the file. `docs_only_excluded_roots`
	// is a bare key, so writing it below a `[table]` header silently makes it a member
	// of that table and the loader sees nothing — which reads exactly like a Consumer
	// that opted out, quietly disabling the docs-only short-circuit. An example that
	// taught that placement would propagate the bug to every project that copied it.
	if len(pc.DocsOnlyExcludedRoots) == 0 {
		t.Error("example config must declare docs_only_excluded_roots ABOVE the first " +
			"[table] header, or the short-circuit is silently disabled")
	}
}

func TestExampleConsumerPromptsLoad(t *testing.T) {
	pb, err := LoadPrompts(exampleConsumer())
	if err != nil {
		t.Fatalf("example Consumer prompts failed to load: %v", err)
	}

	for name, body := range map[string]string{
		"implement": pb.Implement,
		"review":    pb.Review,
		"retro":     pb.Retro,
	} {
		if body == "" {
			t.Errorf("example %s body is empty", name)
		}
	}
}

// The two path-shaped Consumer declarations BEH-641 extracted from the harness.
// Both default to "disabled" when omitted, so an example that failed to
// demonstrate them would teach a Consumer to silently lose the behaviour.
func TestExampleConsumerDeclaresTheExtractedPaths(t *testing.T) {
	pc, err := LoadProject(exampleConsumer())
	if err != nil {
		t.Fatalf("example Consumer config failed to load: %v", err)
	}
	// Asserted through the LOADER for the same reason as docs_only_excluded_roots:
	// both are bare keys, so writing them below a [table] header silently makes
	// them members of that table and the loader sees nothing.
	if len(pc.HandoffStripPaths) == 0 {
		t.Error("example config must declare handoff_strip_paths ABOVE the first " +
			"[table] header, or the handoff strip is silently disabled")
	}
	if len(pc.SourceRoots) == 0 {
		t.Error("example config must declare source_roots ABOVE the first " +
			"[table] header, or the resolved-symbol advisory is silently disabled")
	}
}

// The cache's reclaim command, the third piece of the `[cache]` surface. Like
// the volume and path before it, it was a hardcoded `pnpm store prune` until
// BEH-641 — run against every Consumer whatever its toolchain.
func TestExampleConsumerDeclaresACachePruneCommand(t *testing.T) {
	pc, err := LoadProject(exampleConsumer())
	if err != nil {
		t.Fatalf("example Consumer config failed to load: %v", err)
	}
	if pc.Cache.PruneCommand == "" {
		t.Error("example config must demonstrate cache.prune_command alongside volume + path")
	}
}
