// Consumer-side smoke tests for HERD's committed .agent-harness/ surface.
//
// They live in their own file because they are the Consumer's tests, not the
// harness's: they resolve herd's checkout two directories above this module and
// can only run while the harness is vendored inside it. The extraction (ADR-0007)
// deletes this file wholesale and herd re-expresses these checks as its own guard,
// so nothing here may be depended on by a test that stays.
package config

import (
	"path/filepath"
	"testing"
)

// TestHerdCommittedConfigLoads pins herd's own .agent-harness/config.toml: it
// must parse and pass validation, so a bad edit to the committed file is caught
// here rather than at the next pipeline launch. The repo root is three levels up
// from this package (agent-harness/internal/config → repo root).
func TestHerdCommittedConfigLoads(t *testing.T) {
	pc, err := LoadProject(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("herd committed config failed to load: %v", err)
	}
	if pc.Image == "" || pc.Tracker.Kind == "" || len(pc.Gates) == 0 {
		t.Errorf("herd committed config is under-populated: %+v", pc)
	}
	// herd stays on the BUILD path (it commits a Dockerfile), so a bad edit that
	// dropped `dockerfile` — which would flip herd to a doomed pull of an
	// unpublished tag — is caught here (BEH-635).
	if pc.Dockerfile == "" {
		t.Error("herd committed config must declare `dockerfile` to stay on the build path")
	}
	// The harness now owns worktree/branch creation host-side; herd's per-worktree
	// toolchain setup (env links + pnpm install + Playwright) moved into post_create
	// (BEH-636). A committed config that dropped it would launch sessions against an
	// un-provisioned worktree, so pin its presence here.
	if pc.PostCreate == "" {
		t.Error("herd committed config must declare a `post_create` toolchain-setup hook (BEH-636)")
	}
	// The generalized cache must resolve to herd's pnpm store at its historical
	// mount, whether declared via `[cache]` or the deprecated pnpm_store_volume.
	if pc.Cache.Volume == "" || pc.Cache.Path == "" {
		t.Errorf("herd committed config must resolve a cache volume + path, got %+v", pc.Cache)
	}
	// Asserted through the LOADER, not by reading the file: `docs_only_excluded_roots`
	// is a bare key, so writing it below a `[table]` header silently makes it a member
	// of that table and the loader sees nothing — which reads exactly like a Consumer
	// that opted out, quietly disabling the docs-only short-circuit. A text-level check
	// cannot tell the two apart.
	if len(pc.DocsOnlyExcludedRoots) == 0 {
		t.Error("herd committed config must declare docs_only_excluded_roots ABOVE the first " +
			"[table] header, or the short-circuit is silently disabled")
	}
}

// TestHerdCommittedPromptsLoad pins herd's own .agent-harness/prompts/: each
// stage body must be present and carry its skill invocation, so a bad edit to a
// committed body is caught here rather than at the next pipeline launch. The repo
// root is three levels up from this package.
func TestHerdCommittedPromptsLoad(t *testing.T) {
	pb, err := LoadPrompts(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatalf("herd committed prompts failed to load: %v", err)
	}
	for name, body := range map[string]string{
		"implement": pb.Implement,
		"review":    pb.Review,
		"retro":     pb.Retro,
	} {
		if body == "" {
			t.Errorf("herd committed %s body is empty", name)
		}
	}
}
