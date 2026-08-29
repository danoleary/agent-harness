package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testProjectConfig mirrors herd's committed .agent-harness/config.toml so the
// env-config tests (which don't lay down a real checkout) resolve stable values;
// the real file read is covered by the LoadProject tests in this file.
func testProjectConfig() ProjectConfig {
	return ProjectConfig{
		Image:        "herd-agent-harness:latest",
		Cache:        CacheConfig{Volume: "herd-pnpm-store", Path: "/pnpm-store"},
		BranchPrefix: "feat",
		PostCreate:   "cd web && pnpm install --frozen-lockfile",
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
	if cfg.PostCreate != "cd web && pnpm install --frozen-lockfile" {
		t.Errorf("PostCreate = %q, want it plumbed from project config (BEH-636)", cfg.PostCreate)
	}
	if len(cfg.Gates) != 2 || cfg.Gates[0].Name != "check" {
		t.Errorf("Gates = %+v, want the project-config gate list", cfg.Gates)
	}
	if cfg.Tracker.ReadyLabel != "ready-for-agent" || cfg.Tracker.FindingsLabelID == "" {
		t.Errorf("Tracker = %+v, want project-config tracker names", cfg.Tracker)
	}
}

// The feedback surface is plumbed from the project config through Load so the
// retrospective stage can build the upstream sink (ADR-0011/BEH-640).
func TestLoadSurfacesFeedbackConfig(t *testing.T) {
	orig := projectLoader
	t.Cleanup(func() { projectLoader = orig })
	projectLoader = func(string) (ProjectConfig, error) {
		pc := testProjectConfig()
		pc.Feedback = FeedbackConfig{Upstream: "github", Repo: "example-org/agent-harness", FindingsLabel: "harness-finding", Project: "herd"}
		return pc, nil
	}
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Feedback.Upstream != "github" || cfg.Feedback.Repo != "example-org/agent-harness" {
		t.Errorf("Feedback = %+v, want the project-config feedback surface", cfg.Feedback)
	}
	if cfg.Feedback.Project != "herd" || cfg.Feedback.FindingsLabel != "harness-finding" {
		t.Errorf("Feedback = %+v, want project + label plumbed through", cfg.Feedback)
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
post_create = "cd web && pnpm install --frozen-lockfile"

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
	if pc.PostCreate != "cd web && pnpm install --frozen-lockfile" {
		t.Errorf("PostCreate = %q, want the toolchain-setup command", pc.PostCreate)
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

// A Consumer that commits a `.agent-harness/Dockerfile` (FROM the base) declares
// `dockerfile` instead of naming a prebuilt `image` — the harness BUILDS it on a
// local miss (ADR-0008). So a config with a dockerfile and no image is valid.
func TestLoadProjectAcceptsDockerfileWithoutImage(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
dockerfile = ".agent-harness/Dockerfile"
[tracker]
kind = "linear"
[[gates]]
name = "check"
command = "c"
`)
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject with a dockerfile and no image should succeed, got: %v", err)
	}
	if pc.Dockerfile != ".agent-harness/Dockerfile" {
		t.Errorf("Dockerfile = %q, want the declared path", pc.Dockerfile)
	}
}

// Declaring NEITHER a prebuilt `image` nor a `dockerfile` is a misconfiguration:
// there is no sane cross-language default sandbox, so the harness hard-errors at
// load rather than launching a doomed `docker run` (ADR-0008). The error must name
// both knobs so the operator knows the fix.
func TestLoadProjectRequiresImageOrDockerfile(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
[tracker]
kind = "linear"
[[gates]]
name = "check"
command = "c"
`)
	_, err := LoadProject(dir)
	if err == nil {
		t.Fatal("want error when neither image nor dockerfile is declared, got nil")
	}
	if !strings.Contains(err.Error(), "image") || !strings.Contains(err.Error(), "dockerfile") {
		t.Errorf("error should name both image and dockerfile, got: %v", err)
	}
}

// The cache volume is generalized (BEH-635) from the herd-specific pnpm store to
// a Consumer-declared name + mount path, so a NuGet/GOMODCACHE/pnpm Consumer
// differs only in config.
func TestLoadProjectGeneralizedCacheVolume(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[cache]
volume = "myproj-nuget"
path = "/root/.nuget/packages"
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
	if pc.Cache.Volume != "myproj-nuget" || pc.Cache.Path != "/root/.nuget/packages" {
		t.Errorf("Cache = %+v, want the declared NuGet volume + path", pc.Cache)
	}
}

// Back-compat: the deprecated herd-specific `pnpm_store_volume` key still maps
// onto the generalized cache surface, defaulting to its historical /pnpm-store
// mount so herd's committed config keeps working unchanged (BEH-635).
func TestLoadProjectCacheBackCompatFromPnpmStoreVolume(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
pnpm_store_volume = "herd-pnpm-store"
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
	if pc.Cache.Volume != "herd-pnpm-store" || pc.Cache.Path != "/pnpm-store" {
		t.Errorf("Cache = %+v, want the legacy pnpm store mapped to /pnpm-store", pc.Cache)
	}
}

// A `[cache].volume` with no `path` is a misconfiguration: the whole point of the
// generalization is that the mount path is Consumer-declared, so there is no
// implicit default for the new surface — it fails loud at load (BEH-635).
func TestLoadProjectCachePathRequiredWhenVolumeSet(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[cache]
volume = "v"
[tracker]
kind = "linear"
[[gates]]
name = "check"
command = "c"
`)
	if _, err := LoadProject(dir); err == nil {
		t.Fatal("want error when cache.volume is set without cache.path, got nil")
	}
}

// No cache at all is valid — the cache volume is OPTIONAL (BEH-635); a Consumer
// with no toolchain cache mounts nothing.
func TestLoadProjectCacheOptional(t *testing.T) {
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
	if pc.Cache.Volume != "" || pc.Cache.Path != "" {
		t.Errorf("Cache = %+v, want empty when no cache is declared", pc.Cache)
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

// The opt-in upstream feedback surface (ADR-0011/BEH-640): a `[feedback]` block
// selects where harness findings go — `off` (local artifact, the default) or
// `github` (file to the configured public harness repo).
func TestLoadProjectReadsFeedbackSection(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[feedback]
upstream = "github"
repo = "example-org/agent-harness"
findings_label = "harness-finding"
project = "herd"
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
	if pc.Feedback.Upstream != "github" {
		t.Errorf("Feedback.Upstream = %q, want github", pc.Feedback.Upstream)
	}
	if pc.Feedback.Repo != "example-org/agent-harness" {
		t.Errorf("Feedback.Repo = %q, want the public harness repo", pc.Feedback.Repo)
	}
	if pc.Feedback.FindingsLabel != "harness-finding" {
		t.Errorf("Feedback.FindingsLabel = %q", pc.Feedback.FindingsLabel)
	}
	if pc.Feedback.Project != "herd" {
		t.Errorf("Feedback.Project = %q, want the reporting-project name", pc.Feedback.Project)
	}
}

// The default is local-only: a config with no `[feedback]` block resolves
// upstream to "off" so nothing leaves the repo without an explicit opt-in.
func TestLoadProjectFeedbackDefaultsToOff(t *testing.T) {
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
	if pc.Feedback.Upstream != "off" {
		t.Errorf("Feedback.Upstream = %q, want off (default when omitted)", pc.Feedback.Upstream)
	}
}

// Validation guards the two feedback misconfigurations that would otherwise
// surface only at filing time: an unrecognized upstream mode, and github mode
// with no (or a malformed) public repo to file into.
func TestLoadProjectFeedbackValidation(t *testing.T) {
	base := `
image = "x"
[tracker]
kind = "linear"
[[gates]]
name = "check"
command = "c"
`
	cases := map[string]string{
		"unknown upstream mode": base + `
[feedback]
upstream = "gitlab"
`,
		"github without repo": base + `
[feedback]
upstream = "github"
`,
		"github with malformed repo": base + `
[feedback]
upstream = "github"
repo = "not-a-slug"
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

// off mode needs no repo — it never leaves the repo, so a bare `upstream = "off"`
// (or the default) is valid with nothing else set.
func TestLoadProjectFeedbackOffNeedsNoRepo(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[feedback]
upstream = "off"
[tracker]
kind = "linear"
[[gates]]
name = "check"
command = "c"
`)
	if _, err := LoadProject(dir); err != nil {
		t.Fatalf("off mode with no repo should be valid, got: %v", err)
	}
}
