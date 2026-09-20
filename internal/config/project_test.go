package config

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// testProjectConfig mirrors a Consumer's committed .agent-harness/config.toml so the
// env-config tests (which don't lay down a real checkout) resolve stable values;
// the real file read is covered by the LoadProject tests in this file.
func testProjectConfig() ProjectConfig {
	return ProjectConfig{
		Image:        "myproject-agent-harness:latest",
		Cache:        CacheConfig{Volume: "myproject-cache", Path: "/pnpm-store"},
		BranchPrefix: "feat",
		PostCreate:   "cd web && pnpm install --frozen-lockfile",
		Gates: []Gate{
			{Name: "check", Command: "pnpm run check"},
			{Name: "typecheck", Command: "pnpm run typecheck"},
		},
		Tracker: TrackerConfig{
			Kind: "linear", TeamKey: "BEH",
			FindingsLabelID: "788a5654-a4b3-4ac2-8483-a4d50408ebc0",
			FindingsLabel:   "agent-harness",
			ReadyLabel:      "ready-for-agent", BlockedLabel: "Blocked",
		},
	}
}

func TestMain(m *testing.M) {
	projectLoader = func(string) (ProjectConfig, error) { return testProjectConfig(), nil }
	// Prompt bodies are a required Consumer surface, and fullEnv's PROJECT_PATH is
	// a fixture path with no checkout behind it. Inject usable bodies package-wide
	// so every Load test exercises what it is about; the prompts tests drive the
	// real loader against a temp dir.
	promptsLoader = func(string) (PromptBodies, error) {
		return PromptBodies{Implement: "/implement", Review: "/review", Retro: "/retro"}, nil
	}
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
		pc.Feedback = FeedbackConfig{Upstream: "github", Repo: "example-org/agent-harness", FindingsLabel: "harness-finding", Project: "myproject"}
		return pc, nil
	}
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Feedback.Upstream != "github" || cfg.Feedback.Repo != "example-org/agent-harness" {
		t.Errorf("Feedback = %+v, want the project-config feedback surface", cfg.Feedback)
	}
	if cfg.Feedback.Project != "myproject" || cfg.Feedback.FindingsLabel != "harness-finding" {
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

// consumerConfigTOML mirrors a full Consumer config, exercising every field.
const consumerConfigTOML = `
image = "myproject-agent-harness:latest"
pnpm_store_volume = "myproject-cache"
branch_prefix = "feat"
post_create = "cd web && pnpm install --frozen-lockfile"

[tracker]
kind = "linear"
team_key = "BEH"
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
	writeProjectConfig(t, dir, consumerConfigTOML)

	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if pc.PnpmStoreVolume != "myproject-cache" {
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
`,
		"gate missing command": `
image = "x"
[tracker]
kind = "linear"
team_key = "BEH"
ready_label = "ready-for-agent"
[[gates]]
name = "check"
`,
		"gate missing name": `
image = "x"
[tracker]
kind = "linear"
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
pnpm_store_volume = "myproject-cache"
[tracker]
kind = "linear"
team_key = "BEH"
ready_label = "ready-for-agent"
[[gates]]
name = "check"
command = "c"
`)
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if pc.Cache.Volume != "myproject-cache" || pc.Cache.Path != "/pnpm-store" {
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
project = "myproject"
[tracker]
kind = "linear"
team_key = "BEH"
ready_label = "ready-for-agent"
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
	if pc.Feedback.Project != "myproject" {
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
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
team_key = "BEH"
ready_label = "ready-for-agent"
[[gates]]
name = "check"
command = "c"
`)
	if _, err := LoadProject(dir); err != nil {
		t.Fatalf("off mode with no repo should be valid, got: %v", err)
	}
}

// A Consumer pins the harness version its config is written for, and an older
// binary must refuse rather than silently ignore the keys it does not know.
func TestLoadRejectsAHarnessOlderThanTheConsumerPin(t *testing.T) {
	orig := projectLoader
	t.Cleanup(func() { projectLoader = orig })
	projectLoader = func(string) (ProjectConfig, error) {
		pc := testProjectConfig()
		pc.MinHarnessVersion = "9.9"
		return pc, nil
	}

	_, err := Load(fullEnv(nil), WithHarnessVersion("0.2.0"))
	if err == nil {
		t.Fatal("Load must fail when the harness is older than min_harness_version")
	}
	// The error has to carry both numbers and the key's name, or an operator cannot
	// tell which side to change.
	for _, want := range []string{"9.9", "0.2.0", "min_harness_version"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error must mention %q; got: %v", want, err)
		}
	}
}

func TestLoadAcceptsAHarnessNewerThanTheConsumerPin(t *testing.T) {
	orig := projectLoader
	t.Cleanup(func() { projectLoader = orig })
	projectLoader = func(string) (ProjectConfig, error) {
		pc := testProjectConfig()
		pc.MinHarnessVersion = "0.2"
		return pc, nil
	}

	if _, err := Load(fullEnv(nil), WithHarnessVersion("0.3.1")); err != nil {
		t.Fatalf("Load: %v", err)
	}
}

// A from-source build reports no version, so there is nothing to compare. The
// maintainer working on the harness must not be blocked by a Consumer's pin.
func TestLoadSkipsThePinForADevBuild(t *testing.T) {
	orig := projectLoader
	t.Cleanup(func() { projectLoader = orig })
	projectLoader = func(string) (ProjectConfig, error) {
		pc := testProjectConfig()
		pc.MinHarnessVersion = "9.9"
		return pc, nil
	}

	if _, err := Load(fullEnv(nil), WithHarnessVersion("dev")); err != nil {
		t.Fatalf("a dev build must not be blocked by a pin: %v", err)
	}
}

// An unparseable pin fails loud: a typo must not quietly disable the check.
func TestLoadRejectsAMalformedPin(t *testing.T) {
	orig := projectLoader
	t.Cleanup(func() { projectLoader = orig })
	projectLoader = func(string) (ProjectConfig, error) {
		pc := testProjectConfig()
		pc.MinHarnessVersion = "latest"
		return pc, nil
	}

	if _, err := Load(fullEnv(nil), WithHarnessVersion("0.2.0")); err == nil {
		t.Fatal("Load must reject a malformed min_harness_version")
	}
}

// No pin at all stays valid: the key is optional, and most Consumers will not set
// it until a compatibility break gives them a reason to.
func TestLoadAcceptsNoPin(t *testing.T) {
	if _, err := Load(fullEnv(nil), WithHarnessVersion("0.2.0")); err != nil {
		t.Fatalf("Load without a pin: %v", err)
	}
}

// --- Linear's per-kind required fields (BEH-641) ---
//
// Until the extraction the Linear adapter read its team key and ready label from
// package constants, so a Consumer that declared neither silently inherited the
// harness author's own workspace. They are now Consumer config, and neither has
// a defensible default: an empty team key queries every team the credential can
// see, and an empty ready label drops the human-applied blast-radius gate. Both
// must fail loud at load rather than at the first poll.

func TestLoadProjectLinearRequiresTeamKeyAndReadyLabel(t *testing.T) {
	cases := map[string]struct {
		body string
		want string
	}{
		"missing team key": {body: `
image = "x"
[tracker]
kind = "linear"
ready_label = "ready-for-agent"
[[gates]]
name = "check"
command = "c"
`, want: "team_key"},
		"missing ready label": {body: `
image = "x"
[tracker]
kind = "linear"
team_key = "BEH"
[[gates]]
name = "check"
command = "c"
`, want: "ready_label"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeProjectConfig(t, dir, tc.body)
			_, err := LoadProject(dir)
			if err == nil {
				t.Fatalf("want an error for %s, got nil", name)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("error should name the missing %s field, got: %v", tc.want, err)
			}
		})
	}
}

// The two fields are linear-only: GitHub scopes by repo and Jira by project key,
// so requiring a Linear team key of them would be a new barrier to the very
// Consumers the Tracker port exists to serve.
func TestLoadProjectNonLinearKindsNeedNoTeamKey(t *testing.T) {
	for _, kind := range []string{"github", "jira"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			writeProjectConfig(t, dir, `
image = "x"
[tracker]
kind = "`+kind+`"
[[gates]]
name = "check"
command = "c"
`)
			if _, err := LoadProject(dir); err != nil {
				t.Errorf("kind=%s must not require a Linear team key, got: %v", kind, err)
			}
		})
	}
}

// The Linear adapter needs the findings label in both spellings — a UUID for the
// create API, a name for the issue filter — so both must survive the load.
func TestLoadProjectReadsBothFindingsLabelSpellings(t *testing.T) {
	dir := t.TempDir()
	writeProjectConfig(t, dir, `
image = "x"
[tracker]
kind = "linear"
team_key = "BEH"
ready_label = "ready-for-agent"
findings_label_id = "788a5654-a4b3-4ac2-8483-a4d50408ebc0"
findings_label = "agent-harness"
[[gates]]
name = "check"
command = "c"
`)
	pc, err := LoadProject(dir)
	if err != nil {
		t.Fatalf("LoadProject: %v", err)
	}
	if pc.Tracker.FindingsLabelID != "788a5654-a4b3-4ac2-8483-a4d50408ebc0" {
		t.Errorf("FindingsLabelID = %q", pc.Tracker.FindingsLabelID)
	}
	if pc.Tracker.FindingsLabel != "agent-harness" {
		t.Errorf("FindingsLabel = %q", pc.Tracker.FindingsLabel)
	}
	if pc.Tracker.TeamKey != "BEH" {
		t.Errorf("TeamKey = %q", pc.Tracker.TeamKey)
	}
}
