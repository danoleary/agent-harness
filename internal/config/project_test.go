package config

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// testProjectConfig mirrors herd's committed .agent-harness/config.toml so the
// env-config tests (which don't lay down a real checkout) resolve stable values;
// the real file read is covered by the LoadProject tests in this file.
func testProjectConfig() ProjectConfig {
	return ProjectConfig{
		Image:           "herd-agent-harness:latest",
		PnpmStoreVolume: "herd-pnpm-store",
		BranchPrefix:    "feat",
		Gates: []Gate{
			{Name: "check", Command: "pnpm run check"},
			{Name: "typecheck", Command: "pnpm run typecheck"},
		},
		Tracker: TrackerConfig{
			Kind: "linear", FindingsLabelID: "788a5654-a4b3-4ac2-8483-a4d50408ebc0",
			ReadyLabel: "ready-for-agent", BlockedLabel: "Blocked",
		},
	}
}

func TestMain(m *testing.M) {
	projectLoader = func(string) (ProjectConfig, error) { return testProjectConfig(), nil }
	os.Exit(m.Run())
}

func TestLoadSourcesProjectConfig(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.BranchPrefix != "feat" {
		t.Errorf("BranchPrefix = %q, want feat (from project config)", cfg.BranchPrefix)
	}
	if len(cfg.Gates) != 2 || cfg.Gates[0].Name != "check" {
		t.Errorf("Gates = %+v, want the project-config gate list", cfg.Gates)
	}
	if cfg.Tracker.ReadyLabel != "ready-for-agent" || cfg.Tracker.FindingsLabelID == "" {
		t.Errorf("Tracker = %+v, want project-config tracker names", cfg.Tracker)
	}
}

func TestLoadPropagatesProjectConfigError(t *testing.T) {
	orig := projectLoader
	t.Cleanup(func() { projectLoader = orig })
	projectLoader = func(string) (ProjectConfig, error) {
		return ProjectConfig{}, fmt.Errorf("boom: no config")
	}
	if _, err := Load(fullEnv(nil)); err == nil {
		t.Fatal("want Load to propagate a project-config error, got nil")
	}
}

// writeProjectConfig writes body to <dir>/.agent-harness/config.toml, creating
// the directory, and fails the test on any I/O error.
func writeProjectConfig(t *testing.T, dir, body string) {
	t.Helper()
	ahDir := filepath.Join(dir, ".agent-harness")
	if err := os.MkdirAll(ahDir, 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(ahDir, "config.toml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write config: %v", err)
	}
}

// herdConfigTOML mirrors the values herd commits, exercising every field.
const herdConfigTOML = `
image = "herd-agent-harness:latest"
pnpm_store_volume = "herd-pnpm-store"
branch_prefix = "feat"

[tracker]
kind = "linear"
findings_label_id = "788a5654-a4b3-4ac2-8483-a4d50408ebc0"
ready_label = "ready-for-agent"
blocked_label = "Blocked"

[[gates]]
name = "check"
command = "pnpm run check"

[[gates]]
name = "typecheck"
command = "pnpm run typecheck"
`

func TestLoadProjectReadsAllFields(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, herdConfigTOML)

	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if pc.PnpmStoreVolume != "herd-pnpm-store" {
		t.Errorf("PnpmStoreVolume = %q", pc.PnpmStoreVolume)
	}
	if pc.BranchPrefix != "feat" {
		t.Errorf("BranchPrefix = %q", pc.BranchPrefix)
	}
	if pc.Tracker.Kind != "linear" {
		t.Errorf("Tracker.Kind = %q", pc.Tracker.Kind)
	}
	if pc.Tracker.FindingsLabelID != "788a5654-a4b3-4ac2-8483-a4d50408ebc0" {
		t.Errorf("Tracker.FindingsLabelID = %q", pc.Tracker.FindingsLabelID)
	}
	if pc.Tracker.ReadyLabel != "ready-for-agent" {
		t.Errorf("Tracker.ReadyLabel = %q", pc.Tracker.ReadyLabel)
	}
	if pc.Tracker.BlockedLabel != "Blocked" {
		t.Errorf("Tracker.BlockedLabel = %q", pc.Tracker.BlockedLabel)
	}
	if len(pc.Gates) != 2 {
		t.Fatalf("len(Gates) = %d, want 2", len(pc.Gates))
	}
	if pc.Gates[0].Name != "check" || pc.Gates[0].Command != "pnpm run check" {
		t.Errorf("Gates[0] = %+v", pc.Gates[0])
	}
	if pc.Gates[1].Name != "typecheck" || pc.Gates[1].Command != "pnpm run typecheck" {
		t.Errorf("Gates[1] = %+v", pc.Gates[1])
	}
}

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
}

func TestLoadProjectMissingFileErrors(t *testing.T) {
	_, err := LoadProject(t.TempDir()) // no .agent-harness/config.toml
	if err == nil {
		t.Fatal("want error for missing config file, got nil")
	}
}

func TestLoadProjectMalformedErrors(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, "image = \"x\"\n[tracker\nkind = ") // broken TOML
	_, err := LoadProject(dir)
	if err == nil {
		t.Fatal("want error for malformed TOML, got nil")
	}
}

func TestLoadProjectRequiredFieldsError(t *testing.T) {
	cases := map[string]string{
		"missing image": `
[tracker]
kind = "linear"
[[gates]]
name = "check"
command = "c"
`,
		"missing tracker kind": `
image = "x"
[[gates]]
name = "check"
command = "c"
`,
		"no gates": `
image = "x"
[tracker]
kind = "linear"
`,
		"gate missing command": `
image = "x"
[tracker]
kind = "linear"
[[gates]]
name = "check"
`,
		"gate missing name": `
image = "x"
[tracker]
kind = "linear"
[[gates]]
command = "c"
`,
	}
	for name, body := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeProjectConfig(t, dir, body)
			if _, err := LoadProject(dir); err == nil {
				t.Errorf("want error for %s, got nil", name)
			}
		})
	}
}

func TestLoadProjectBranchPrefixDefaultsToFeat(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[tracker]
kind = "linear"
[[gates]]
name = "check"
command = "c"
`)
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if pc.BranchPrefix != "feat" {
		t.Errorf("BranchPrefix = %q, want feat (default when omitted)", pc.BranchPrefix)
	}
}
