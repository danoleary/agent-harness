package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// fullEnv returns a getenv backed by a complete env, with overrides applied.
// Setting an override to "" deletes that key (getenv returns "" for absent keys).
func fullEnv(overrides map[string]string) Getenv {
	base := map[string]string{
		"ANTHROPIC_API_KEY": "sk-ant-x",
		"GH_TOKEN":          "ghp_x",
		"LINEAR_API_KEY":    "lin_x",
		"PROJECT_PATH":      "/Users/dan/myproject",
	}
	for k, v := range overrides {
		base[k] = v
	}
	return func(k string) string { return base[k] }
}

func TestLoadDefaults(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LinearAPIKey != "lin_x" {
		t.Errorf("LinearAPIKey = %q, want lin_x", cfg.LinearAPIKey)
	}
	if cfg.ProjectPath != "/Users/dan/myproject" {
		t.Errorf("ProjectPath = %q", cfg.ProjectPath)
	}
	if cfg.Image != "myproject-agent-harness:latest" {
		t.Errorf("Image = %q", cfg.Image)
	}
	if cfg.CacheVolume != "myproject-cache" {
		t.Errorf("CacheVolume = %q", cfg.CacheVolume)
	}
	if cfg.CacheMountPath != "/pnpm-store" {
		t.Errorf("CacheMountPath = %q, want /pnpm-store from the legacy pnpm_store_volume mapping", cfg.CacheMountPath)
	}
	if cfg.TddTimeout != 30*time.Minute {
		t.Errorf("TddTimeout = %v, want 30m", cfg.TddTimeout)
	}
	// Every stage pins the exact Opus snapshot, not the floating alias.
	for stage, got := range map[string]string{
		"implementation": cfg.ImplementationModel,
		"review":         cfg.ReviewModel,
		"retrospective":  cfg.RetrospectiveModel,
	} {
		if got != "claude-opus-5-5" {
			t.Errorf("%s model = %q, want claude-opus-5-5", stage, got)
		}
	}
	// The review family's cap is kept above the 20m idle window so the idle
	// watchdog can reap a stalled session before this hard cap (BEH-535/538).
	if cfg.ReviewTimeout != 25*time.Minute {
		t.Errorf("ReviewTimeout = %v, want 25m", cfg.ReviewTimeout)
	}
	if cfg.SessionIdleTimeout != 20*time.Minute {
		t.Errorf("SessionIdleTimeout = %v, want 20m", cfg.SessionIdleTimeout)
	}
	// The retrospective is a long, read-heavy step (it parses several large jsonl
	// transcripts) and must NOT borrow the tdd cap — it gets a larger cap of its
	// own so a slow read pass isn't killed before it can write findings (BEH-536).
	if cfg.RetrospectiveTimeout != 45*time.Minute {
		t.Errorf("RetrospectiveTimeout = %v, want 45m", cfg.RetrospectiveTimeout)
	}
	// A multi-file extract-and-rewire refactor is inherently sequential and
	// overruns the 30m tdd cap mid-surgery; it gets a larger cap of its own
	// (BEH-688 Symptom 2).
	if cfg.TddLargeRefactorTimeout != 60*time.Minute {
		t.Errorf("TddLargeRefactorTimeout = %v, want 60m", cfg.TddLargeRefactorTimeout)
	}
	if cfg.TddLargeRefactorTimeout <= cfg.TddTimeout {
		t.Errorf("TddLargeRefactorTimeout = %v must exceed TddTimeout = %v", cfg.TddLargeRefactorTimeout, cfg.TddTimeout)
	}
}

// Every per-session hard cap must stay strictly above the idle/no-progress window.
// The idle watchdog only reaps a dead-stream session if the cap leaves it room to
// fire first; a cap at or below the idle window is always reached first, silently
// disabling dead-stream detection for that session class. The review family had a
// 15m cap under a 20m idle window, so its idle watchdog never fired and a stalled
// review session burned to the hard cap instead of being reaped early (BEH-535/538).
func TestHardCapsExceedIdleWindow(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	caps := map[string]time.Duration{
		"TddTimeout":           cfg.TddTimeout,
		"ReviewTimeout":        cfg.ReviewTimeout,
		"RetrospectiveTimeout": cfg.RetrospectiveTimeout,
	}
	for name, hardCap := range caps {
		if hardCap <= cfg.SessionIdleTimeout {
			t.Errorf(
				"%s = %v must exceed SessionIdleTimeout = %v, else the idle/no-progress watchdog can never fire before the hard cap",
				name, hardCap, cfg.SessionIdleTimeout,
			)
		}
	}
}

// The cap>idle invariant must hold for the resolved config, not just the
// hand-picked defaults: a stalled session is reaped early only if the idle
// watchdog fires before the hard cap, so any env override that lifts the idle
// window to or above a hard cap silently disables dead-stream detection for that
// session class — exactly the inert-watchdog regression that let a stalled
// retrospective burn to the hard cap (BEH-535/538). Load must reject it loudly
// (before any sandbox launches), naming the offending cap and both durations.
func TestLoadRejectsIdleWindowAtOrAboveAnyCap(t *testing.T) {
	// idle = 5m sits above the 2m review / 3m retrospective / 1m tdd caps — the
	// idle watchdog could never fire before any of those caps.
	_, err := Load(fullEnv(map[string]string{
		"TDD_TIMEOUT_MS":           "60000",  // 1m
		"REVIEW_TIMEOUT_MS":        "120000", // 2m
		"RETROSPECTIVE_TIMEOUT_MS": "180000", // 3m
		"SESSION_IDLE_TIMEOUT_MS":  "300000", // 5m — above every cap
	}))
	if err == nil {
		t.Fatal("expected an error when the idle window is at or above a hard cap (idle watchdog would be inert)")
	}
	if !strings.Contains(err.Error(), "idle") {
		t.Errorf("error %q should explain the idle-window inversion", err.Error())
	}

	// Boundary: idle EQUAL to a cap is still a failure — the cap is reached first
	// (watchReason uses >=), so the idle watchdog never gets to fire.
	_, err = Load(fullEnv(map[string]string{
		"REVIEW_TIMEOUT_MS":       "300000", // 5m
		"SESSION_IDLE_TIMEOUT_MS": "300000", // 5m — equal to the review cap
	}))
	if err == nil {
		t.Fatal("expected an error when the idle window equals a hard cap")
	}

	// The large-refactor cap is a per-session hard cap too, so the idle window
	// must stay below it as well — a mis-set large cap under the idle window would
	// silently disable the idle watchdog for large-refactor sessions.
	_, err = Load(fullEnv(map[string]string{
		"TDD_LARGE_REFACTOR_TIMEOUT_MS": "60000",  // 1m
		"SESSION_IDLE_TIMEOUT_MS":       "120000", // 2m — above the large cap
	}))
	if err == nil {
		t.Fatal("expected an error when the idle window is above the large-refactor cap")
	}
}

func TestLoadHonoursOverrides(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"HARNESS_IMAGE":                 "custom:tag",
		"PNPM_STORE_VOLUME":             "my-store",
		"TDD_TIMEOUT_MS":                "60000",
		"TDD_LARGE_REFACTOR_TIMEOUT_MS": "120000",
		"REVIEW_TIMEOUT_MS":             "120000",
		"RETROSPECTIVE_TIMEOUT_MS":      "180000",
		"SESSION_IDLE_TIMEOUT_MS":       "30000", // 30s — kept below the 1m tdd cap (idle must stay under every cap)
		"TDD_MODEL":                     "sonnet",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ReviewTimeout != 2*time.Minute {
		t.Errorf("ReviewTimeout = %v, want 2m", cfg.ReviewTimeout)
	}
	if cfg.RetrospectiveTimeout != 3*time.Minute {
		t.Errorf("RetrospectiveTimeout = %v, want 3m (RETROSPECTIVE_TIMEOUT_MS override)", cfg.RetrospectiveTimeout)
	}
	if cfg.SessionIdleTimeout != 30*time.Second {
		t.Errorf("SessionIdleTimeout = %v, want 30s", cfg.SessionIdleTimeout)
	}
	if cfg.Image != "custom:tag" {
		t.Errorf("Image = %q", cfg.Image)
	}
	if cfg.CacheVolume != "my-store" {
		t.Errorf("CacheVolume = %q", cfg.CacheVolume)
	}
	if cfg.TddTimeout != time.Minute {
		t.Errorf("TddTimeout = %v, want 1m", cfg.TddTimeout)
	}
	if cfg.TddLargeRefactorTimeout != 2*time.Minute {
		t.Errorf("TddLargeRefactorTimeout = %v, want 2m (TDD_LARGE_REFACTOR_TIMEOUT_MS override)", cfg.TddLargeRefactorTimeout)
	}
	if cfg.ImplementationModel != "sonnet" {
		t.Errorf("ImplementationModel = %q, want sonnet (TDD_MODEL override)", cfg.ImplementationModel)
	}
}

// The deprecated PNPM_STORE_VOLUME env override is the host twin of the
// pnpm_store_volume key: when it supplies the cache volume against a project that
// declared no `[cache]` path, the resolved mount must still default to
// /pnpm-store — otherwise a pathless volume produces an invalid `-v <vol>:`
// docker arg that dies at run (exit 125) after the ticket is already claimed.
// HARNESS_MODEL moves every stage; a per-stage variable overrides only its stage.
func TestLoadPicksAModelPerStage(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"HARNESS_MODEL":       "claude-sonnet-5",
		"RETROSPECTIVE_MODEL": "claude-haiku-4-5-20251001",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.ImplementationModel != "claude-sonnet-5" {
		t.Errorf("ImplementationModel = %q, want claude-sonnet-5 (HARNESS_MODEL)", cfg.ImplementationModel)
	}
	if cfg.ReviewModel != "claude-sonnet-5" {
		t.Errorf("ReviewModel = %q, want claude-sonnet-5 (HARNESS_MODEL)", cfg.ReviewModel)
	}
	if cfg.RetrospectiveModel != "claude-haiku-4-5-20251001" {
		t.Errorf("RetrospectiveModel = %q, want claude-haiku-4-5-20251001 (RETROSPECTIVE_MODEL override)", cfg.RetrospectiveModel)
	}
}

func TestLoadEnvCacheVolumeWithoutPathDefaultsMount(t *testing.T) {
	orig := projectLoader
	t.Cleanup(func() { projectLoader = orig })
	projectLoader = func(string) (ProjectConfig, error) {
		pc := testProjectConfig()
		pc.Cache = CacheConfig{} // a Consumer that declares no cache
		return pc, nil
	}
	cfg, err := Load(fullEnv(map[string]string{"PNPM_STORE_VOLUME": "env-store"}))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.CacheVolume != "env-store" {
		t.Errorf("CacheVolume = %q, want the env override", cfg.CacheVolume)
	}
	if cfg.CacheMountPath != "/pnpm-store" {
		t.Errorf("CacheMountPath = %q, want /pnpm-store default so the mount is not pathless", cfg.CacheMountPath)
	}
}

// GH_TOKEN and PROJECT_PATH are required whatever the tracker is: the harness
// pushes branches and opens PRs with `gh` regardless (ADR-0002), and it must know
// which checkout it is working. The TRACKER credential is deliberately absent
// from this list — see TestLoadDoesNotRequireATrackerCredential.
func TestLoadMissingRequired(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "PROJECT_PATH"} {
		_, err := Load(fullEnv(map[string]string{key: ""}))
		if err == nil {
			t.Errorf("expected error when %s missing", key)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q does not mention %s", err.Error(), key)
		}
	}
}

// A subscription OAuth token alone (no ANTHROPIC_API_KEY) is a valid credential.
func TestLoadAcceptsOAuthTokenOnly(t *testing.T) {
	_, err := Load(fullEnv(map[string]string{
		"ANTHROPIC_API_KEY":       "",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-x",
	}))
	if err != nil {
		t.Errorf("OAuth token alone should satisfy the Claude credential requirement, got: %v", err)
	}
}

// The Anthropic API key is held host-side for the semantic dedup model call, and
// the cheap dedup model defaults to a small model (BEH-573).
func TestLoadExposesAnthropicKeyAndDedupModel(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicAPIKey != "sk-ant-x" {
		t.Errorf("AnthropicAPIKey = %q, want sk-ant-x", cfg.AnthropicAPIKey)
	}
	if cfg.DedupModel != "claude-haiku-4-5-20251001" {
		t.Errorf("DedupModel = %q, want the cheap default", cfg.DedupModel)
	}
}

// The Jira Basic-auth triple is surfaced from env as host-only secrets (never
// committed config), read only when tracker.kind=jira. They are optional at Load
// — a non-jira consumer sets none — so their absence must not fail the load.
func TestLoadExposesJiraSecrets(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"JIRA_BASE_URL":  "https://acme.atlassian.net",
		"JIRA_EMAIL":     "bot@acme.co",
		"JIRA_API_TOKEN": "jira_tok",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JiraBaseURL != "https://acme.atlassian.net" || cfg.JiraEmail != "bot@acme.co" || cfg.JiraAPIToken != "jira_tok" {
		t.Errorf("Jira secrets = %q/%q/%q, want the env values", cfg.JiraBaseURL, cfg.JiraEmail, cfg.JiraAPIToken)
	}
}

// The Jira secrets are optional: a consumer on Linear/GitHub sets none, and Load
// must still succeed with them empty (trackers.New is what fails loud on a jira
// selection missing its auth, not Load).
func TestLoadJiraSecretsOptional(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.JiraBaseURL != "" || cfg.JiraEmail != "" || cfg.JiraAPIToken != "" {
		t.Errorf("expected empty Jira secrets by default, got %q/%q/%q", cfg.JiraBaseURL, cfg.JiraEmail, cfg.JiraAPIToken)
	}
}

// With only an OAuth token (no API key), AnthropicAPIKey is empty — the x-api-key
// header rejects an OAuth token (BEH-316), so the matcher is left unwired and
// filing degrades to exact-match dedup. The credential check still passes.
func TestLoadLeavesAnthropicKeyEmptyForOAuthOnly(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"ANTHROPIC_API_KEY":       "",
		"CLAUDE_CODE_OAUTH_TOKEN": "sk-ant-oat01-x",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.AnthropicAPIKey != "" {
		t.Errorf("AnthropicAPIKey = %q, want empty for OAuth-only", cfg.AnthropicAPIKey)
	}
}

// Neither Claude credential set → a clear error naming both vars.
func TestLoadRequiresAClaudeCredential(t *testing.T) {
	_, err := Load(fullEnv(map[string]string{
		"ANTHROPIC_API_KEY":       "",
		"CLAUDE_CODE_OAUTH_TOKEN": "",
	}))
	if err == nil {
		t.Fatal("expected error when no Claude credential is set")
	}
	for _, want := range []string{"ANTHROPIC_API_KEY", "CLAUDE_CODE_OAUTH_TOKEN"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %s", err.Error(), want)
		}
	}
}

// LoadDotEnv seeds the process env from a KEY=VALUE file for any key not already
// set (mirrors `node --env-file-if-exists`), so the shared cmd entrypoints get
// .env loading from one place.
func TestLoadDotEnvSeedsUnsetKeysOnly(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, ".env")
	contents := "# a comment\n" +
		"HARNESS_TEST_FRESH=\"from-file\"\n" +
		"\n" +
		"HARNESS_TEST_PRESET=should-not-win\n"
	if err := os.WriteFile(path, []byte(contents), 0o644); err != nil {
		t.Fatalf("write .env: %v", err)
	}

	// A key already in the env must win over the file (file fills gaps only).
	t.Setenv("HARNESS_TEST_PRESET", "already-set")

	LoadDotEnv(path)

	if got := os.Getenv("HARNESS_TEST_FRESH"); got != "from-file" {
		t.Errorf("HARNESS_TEST_FRESH = %q, want from-file (quotes stripped, unset key seeded)", got)
	}
	if got := os.Getenv("HARNESS_TEST_PRESET"); got != "already-set" {
		t.Errorf("HARNESS_TEST_PRESET = %q, want already-set (existing value preserved)", got)
	}
}

// An absent file is a no-op, never an error (the common case in CI where config
// comes from real env vars).
func TestLoadDotEnvAbsentFileIsNoOp(t *testing.T) {
	LoadDotEnv(filepath.Join(t.TempDir(), "does-not-exist.env"))
}

// The cmd/loop config knobs default to the values in DESIGN.md §cmd/loop config
// knobs: the prior slices' hardcoded values, now env-overridable. The two run
// ceilings default to unlimited (0) because the loop is deliberately long-running.
func TestLoadLoopDefaults(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LoopPollInterval != time.Minute {
		t.Errorf("LoopPollInterval = %v, want 60s", cfg.LoopPollInterval)
	}
	if cfg.LoopCapBackoff != 45*time.Minute {
		t.Errorf("LoopCapBackoff = %v, want 45m", cfg.LoopCapBackoff)
	}
	if cfg.LoopMaxConsecutiveFailures != 3 {
		t.Errorf("LoopMaxConsecutiveFailures = %d, want 3", cfg.LoopMaxConsecutiveFailures)
	}
	if cfg.LoopMaxTickets != 0 {
		t.Errorf("LoopMaxTickets = %d, want 0 (unlimited)", cfg.LoopMaxTickets)
	}
	if cfg.LoopMaxRuntime != 0 {
		t.Errorf("LoopMaxRuntime = %v, want 0 (unlimited)", cfg.LoopMaxRuntime)
	}
	if cfg.StopFile != ".agent-harness/STOP" {
		t.Errorf("StopFile = %q, want .agent-harness/STOP", cfg.StopFile)
	}
	if cfg.LoopDiskReclaimThreshold != 8<<30 {
		t.Errorf("LoopDiskReclaimThreshold = %d, want %d (8 GiB — above the 5 GiB sandbox floor)", cfg.LoopDiskReclaimThreshold, 8<<30)
	}
	if cfg.LoopClaimTTL != 30*time.Minute {
		t.Errorf("LoopClaimTTL = %v, want 30m (grace past the claim→first-push window)", cfg.LoopClaimTTL)
	}
}

// The disk-reclaim threshold is env-overridable in bytes, and an explicit 0 disables
// reclaim entirely (a documented value, not a nonsensical one).
func TestLoadDiskReclaimThresholdOverrideAndDisable(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{"LOOP_DISK_RECLAIM_THRESHOLD_BYTES": "10737418240"})) // 10 GiB
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LoopDiskReclaimThreshold != 10<<30 {
		t.Errorf("LoopDiskReclaimThreshold = %d, want %d (10 GiB)", cfg.LoopDiskReclaimThreshold, 10<<30)
	}

	cfg, err = Load(fullEnv(map[string]string{"LOOP_DISK_RECLAIM_THRESHOLD_BYTES": "0"}))
	if err != nil {
		t.Fatalf("an explicit 0 (disable reclaim) must be accepted, got: %v", err)
	}
	if cfg.LoopDiskReclaimThreshold != 0 {
		t.Errorf("LoopDiskReclaimThreshold = %d, want 0 (reclaim disabled)", cfg.LoopDiskReclaimThreshold)
	}
}

// A negative or unparseable threshold fails loud at load, naming the var.
func TestLoadRejectsInvalidDiskReclaimThreshold(t *testing.T) {
	for _, bad := range []string{"-1", "abc"} {
		_, err := Load(fullEnv(map[string]string{"LOOP_DISK_RECLAIM_THRESHOLD_BYTES": bad}))
		if err == nil {
			t.Errorf("LOOP_DISK_RECLAIM_THRESHOLD_BYTES=%q should be rejected", bad)
			continue
		}
		if !strings.Contains(err.Error(), "LOOP_DISK_RECLAIM_THRESHOLD_BYTES") {
			t.Errorf("error %q should name the offending var", err.Error())
		}
	}
}

// Every loop knob is env-overridable; the *_MS knobs are milliseconds.
func TestLoadLoopOverrides(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"LOOP_POLL_INTERVAL_MS":         "5000",   // 5s
		"LOOP_CAP_BACKOFF_MS":           "120000", // 2m
		"LOOP_MAX_CONSECUTIVE_FAILURES": "5",
		"LOOP_MAX_TICKETS":              "10",
		"LOOP_MAX_RUNTIME_MS":           "3600000", // 1h
		"LOOP_CLAIM_TTL_MS":             "600000",  // 10m
		"STOP_FILE":                     "/tmp/custom-stop",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cfg.LoopClaimTTL != 10*time.Minute {
		t.Errorf("LoopClaimTTL = %v, want 10m", cfg.LoopClaimTTL)
	}
	if cfg.LoopPollInterval != 5*time.Second {
		t.Errorf("LoopPollInterval = %v, want 5s", cfg.LoopPollInterval)
	}
	if cfg.LoopCapBackoff != 2*time.Minute {
		t.Errorf("LoopCapBackoff = %v, want 2m", cfg.LoopCapBackoff)
	}
	if cfg.LoopMaxConsecutiveFailures != 5 {
		t.Errorf("LoopMaxConsecutiveFailures = %d, want 5", cfg.LoopMaxConsecutiveFailures)
	}
	if cfg.LoopMaxTickets != 10 {
		t.Errorf("LoopMaxTickets = %d, want 10", cfg.LoopMaxTickets)
	}
	if cfg.LoopMaxRuntime != time.Hour {
		t.Errorf("LoopMaxRuntime = %v, want 1h", cfg.LoopMaxRuntime)
	}
	if cfg.StopFile != "/tmp/custom-stop" {
		t.Errorf("StopFile = %q, want /tmp/custom-stop", cfg.StopFile)
	}
}

// An explicit 0 for the two run ceilings is a valid value meaning "unlimited" — it
// must not be rejected as nonsensical (it's the documented default).
func TestLoadLoopCeilingsAcceptExplicitZero(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"LOOP_MAX_TICKETS":    "0",
		"LOOP_MAX_RUNTIME_MS": "0",
	}))
	if err != nil {
		t.Fatalf("explicit 0 ceilings must be accepted (unlimited), got: %v", err)
	}
	if cfg.LoopMaxTickets != 0 {
		t.Errorf("LoopMaxTickets = %d, want 0", cfg.LoopMaxTickets)
	}
	if cfg.LoopMaxRuntime != 0 {
		t.Errorf("LoopMaxRuntime = %v, want 0", cfg.LoopMaxRuntime)
	}
}

// Nonsensical loop-knob overrides are rejected at load time with a message naming
// the offending var — unlike the lenient *_MS timeouts, these fail loud before the
// daemon launches.
func TestLoadRejectsInvalidLoopKnobs(t *testing.T) {
	cases := map[string]string{
		"LOOP_POLL_INTERVAL_MS":         "0",   // must be positive
		"LOOP_CAP_BACKOFF_MS":           "-1",  // must be positive
		"LOOP_MAX_CONSECUTIVE_FAILURES": "0",   // must be positive
		"LOOP_MAX_TICKETS":              "-1",  // must be non-negative
		"LOOP_MAX_RUNTIME_MS":           "abc", // must be an integer
	}
	for key, bad := range cases {
		_, err := Load(fullEnv(map[string]string{key: bad}))
		if err == nil {
			t.Errorf("%s=%q should be rejected", key, bad)
			continue
		}
		if !strings.Contains(err.Error(), key) {
			t.Errorf("error %q should name the offending var %s", err.Error(), key)
		}
	}
}

func TestLoadTimeoutFallback(t *testing.T) {
	for _, value := range []string{"abc", "0", "-5", ""} {
		cfg, err := Load(fullEnv(map[string]string{"TDD_TIMEOUT_MS": value}))
		if err != nil {
			t.Fatalf("unexpected error for %q: %v", value, err)
		}
		if cfg.TddTimeout != 30*time.Minute {
			t.Errorf("TDD_TIMEOUT_MS=%q → TddTimeout = %v, want 30m", value, cfg.TddTimeout)
		}
	}
}

// An operator who installed the harness from a release archive has no checkout to
// put a .env beside, and the Consumer's own repo is the one place it must never go
// (every stage bind-mounts that checkout into the sandbox, so a .env there hands
// the host-only tracker and GitHub credentials to the agent session). So the env
// file is resolved from the operator's own config dir, independent of cwd.
func TestResolveEnvFileUsesXDGConfigDirWhenNoLocalFile(t *testing.T) {
	home := t.TempDir()
	xdg := filepath.Join(home, "xdg")
	want := filepath.Join(xdg, "agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(want, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := ResolveEnvFile(func(k string) string {
		switch k {
		case "XDG_CONFIG_HOME":
			return xdg
		case "HOME":
			return home
		}
		return ""
	}, filepath.Join(home, "somewhere-else"))
	if got != want {
		t.Errorf("ResolveEnvFile = %q, want %q", got, want)
	}
}

// With no XDG_CONFIG_HOME set, the same file is looked for under the
// platform-conventional ~/.config, so the documented path holds on a stock macOS
// shell where XDG_CONFIG_HOME is usually unset.
func TestResolveEnvFileFallsBackToHomeConfig(t *testing.T) {
	home := t.TempDir()
	want := filepath.Join(home, ".config", "agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(want, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := ResolveEnvFile(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}, filepath.Join(home, "somewhere-else"))
	if got != want {
		t.Errorf("ResolveEnvFile = %q, want %q", got, want)
	}
}

// HARNESS_ENV_FILE wins outright, so an operator driving several projects can keep
// a credential set per project outside every checkout.
func TestResolveEnvFileHonoursExplicitOverride(t *testing.T) {
	dir := t.TempDir()
	explicit := filepath.Join(dir, "work.env")
	if err := os.WriteFile(explicit, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	// A file in the config dir too, to prove the override outranks it.
	xdg := filepath.Join(dir, "xdg")
	if err := os.MkdirAll(filepath.Join(xdg, "agent-harness"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(xdg, "agent-harness", ".env"), []byte("K=W\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := ResolveEnvFile(func(k string) string {
		switch k {
		case "HARNESS_ENV_FILE":
			return explicit
		case "XDG_CONFIG_HOME":
			return xdg
		}
		return ""
	}, dir)
	if got != explicit {
		t.Errorf("ResolveEnvFile = %q, want the explicit override %q", got, explicit)
	}
}

// A .env beside the binary/checkout still works: it is how every existing
// from-source operator runs, and dropping it would break them on upgrade.
func TestResolveEnvFileFallsBackToWorkingDir(t *testing.T) {
	home := t.TempDir() // no config-dir .env
	wd := t.TempDir()
	want := filepath.Join(wd, ".env")
	if err := os.WriteFile(want, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := ResolveEnvFile(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}, wd)
	if got != want {
		t.Errorf("ResolveEnvFile = %q, want the working-dir fallback %q", got, want)
	}
}

// Nothing anywhere resolves to "", which LoadDotEnv treats as a no-op — the
// environment alone is a legitimate way to run.
func TestResolveEnvFileEmptyWhenNoneExists(t *testing.T) {
	home := t.TempDir()
	if got := ResolveEnvFile(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}, t.TempDir()); got != "" {
		t.Errorf("ResolveEnvFile = %q, want empty", got)
	}
}

// The bind-mount hazard, detected so LoadConfig can warn about it: a credential
// file anywhere inside the Consumer checkout is readable by every sandbox.
func TestEnvFileInsideProject(t *testing.T) {
	cases := []struct {
		name    string
		envFile string
		project string
		want    bool
	}{
		{"at the checkout root", "/src/herd/.env", "/src/herd", true},
		// `.agent-harness/.env` is deliberately absent here: it is the masked
		// location, exempted by TestEnvFileInsideProjectExemptsTheMaskedLocation.
		{"nested in the checkout", "/src/herd/web/.env", "/src/herd", true},
		{"operator config dir", "/Users/dan/.config/agent-harness/.env", "/src/herd", false},
		// A sibling whose name merely starts with the checkout's path is NOT inside
		// it; a raw string-prefix test would call this a leak and nag forever.
		{"sibling with a shared prefix", "/src/herd-notes/.env", "/src/herd", false},
		{"no env file at all", "", "/src/herd", false},
		{"no project path", "/src/herd/.env", "", false},
		{"trailing separator on the project", "/src/herd/.env", "/src/herd/", true},
	}
	for _, c := range cases {
		if got := EnvFileInsideProject(c.envFile, c.project); got != c.want {
			t.Errorf("%s: EnvFileInsideProject(%q, %q) = %v, want %v", c.name, c.envFile, c.project, got, c.want)
		}
	}
}

// The hint is what the warning tells the operator to do, so it must name the same
// path ResolveEnvFile prefers — otherwise the fix does not take effect.
func TestEnvFileHintMatchesTheResolvedConfigDir(t *testing.T) {
	home := t.TempDir()
	getenv := func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}
	hint := EnvFileHint(getenv)
	want := filepath.Join(home, ".config", "agent-harness", ".env")
	if hint != want {
		t.Errorf("EnvFileHint = %q, want %q", hint, want)
	}
	// Writing the hinted file makes ResolveEnvFile pick it up.
	if err := os.MkdirAll(filepath.Dir(hint), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(hint, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := ResolveEnvFile(getenv, t.TempDir()); got != hint {
		t.Errorf("ResolveEnvFile = %q, want the hinted path %q", got, hint)
	}
}

// Local beats global, and scripts/loop-start.sh resolves PROJECT_PATH the same
// way. If the two disagreed, the script and the daemon could read different files
// — pidfile under one project, work happening in another.
func TestResolveEnvFilePrefersWorkingDirOverConfigDir(t *testing.T) {
	home := t.TempDir()
	wd := t.TempDir()
	local := filepath.Join(wd, ".env")
	if err := os.WriteFile(local, []byte("K=local\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	global := filepath.Join(home, ".config", "agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(global), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(global, []byte("K=global\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := ResolveEnvFile(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}, wd)
	if got != local {
		t.Errorf("ResolveEnvFile = %q, want the working-dir file %q", got, local)
	}
}

// With neither HOME nor XDG_CONFIG_HOME there is no config dir to name, so the
// hint is empty rather than a bogus "/.config/…" an operator cannot write.
func TestEnvFileHintEmptyWithoutAHome(t *testing.T) {
	if got := EnvFileHint(func(string) string { return "" }); got != "" {
		t.Errorf("EnvFileHint = %q, want empty", got)
	}
}

// The in-repo location. A Consumer already commits `.agent-harness/` for its
// config and prompts, so that is where an operator looks for the credential file
// too — and the harness masks it in every sandbox, so it is safe to keep there.
// Found relative to cwd, which is the checkout when you run the harness from the
// project you are working.
func TestResolveEnvFileFindsTheInRepoLocation(t *testing.T) {
	home := t.TempDir() // no config-dir .env
	project := t.TempDir()
	want := filepath.Join(project, ".agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(want, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := ResolveEnvFile(func(k string) string {
		if k == "HOME" {
			return home
		}
		return ""
	}, project)
	if got != want {
		t.Errorf("ResolveEnvFile = %q, want the in-repo file %q", got, want)
	}
}

// With PROJECT_PATH already exported the in-repo file is found from any cwd, so a
// `loop` launched from elsewhere still reads the project's own credentials.
func TestResolveEnvFileFindsTheInRepoLocationViaProjectPath(t *testing.T) {
	home := t.TempDir()
	project := t.TempDir()
	want := filepath.Join(project, ".agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(want), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(want, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	got := ResolveEnvFile(func(k string) string {
		switch k {
		case "HOME":
			return home
		case "PROJECT_PATH":
			return project
		}
		return ""
	}, t.TempDir()) // cwd is somewhere else entirely
	if got != want {
		t.Errorf("ResolveEnvFile = %q, want the in-repo file %q", got, want)
	}
}

// A bare ./.env still outranks ./.agent-harness/.env: an operator who keeps one
// beside the harness checkout should not have it shadowed by a project file.
func TestResolveEnvFilePrefersBareDotEnvOverTheInRepoLocation(t *testing.T) {
	dir := t.TempDir()
	bare := filepath.Join(dir, ".env")
	if err := os.WriteFile(bare, []byte("K=bare\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	inRepo := filepath.Join(dir, ".agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(inRepo), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(inRepo, []byte("K=inrepo\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	if got := ResolveEnvFile(func(string) string { return "" }, dir); got != bare {
		t.Errorf("ResolveEnvFile = %q, want %q", got, bare)
	}
}

// The blessed in-repo path is masked in every container, so it must NOT trip the
// "inside the bind-mounted checkout" warning — that warning exists for a
// credential file the mask does not cover, and crying wolf about the documented
// location would train an operator to ignore it.
func TestEnvFileInsideProjectExemptsTheMaskedLocation(t *testing.T) {
	if EnvFileInsideProject("/src/herd/.agent-harness/.env", "/src/herd") {
		t.Error("the masked in-repo location must not be reported as an exposure")
	}
	// Anywhere else inside the checkout is still unmasked, so still a warning.
	if !EnvFileInsideProject("/src/herd/.env", "/src/herd") {
		t.Error("a credential file elsewhere in the checkout is not masked and must warn")
	}
	if !EnvFileInsideProject("/src/herd/web/.env", "/src/herd") {
		t.Error("a nested credential file is not masked and must warn")
	}
}

// --- the tracker credential is per-kind (BEH-641) ---

// Load required LINEAR_API_KEY unconditionally, so a Jira-only or
// GitHub-Issues-only Consumer could not start the harness at all — the tracker
// port (ADR-0010) offered three adapters that only one credential could reach.
// Which credential is needed depends on `tracker.kind`, which lives in the
// project config, so the check belongs to trackers.New, not here.
func TestLoadDoesNotRequireATrackerCredential(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{"LINEAR_API_KEY": ""}))
	if err != nil {
		t.Fatalf("Load must not require a tracker credential of its own, got: %v", err)
	}
	if cfg.LinearAPIKey != "" {
		t.Errorf("LinearAPIKey = %q, want empty", cfg.LinearAPIKey)
	}
}

// It must still be carried through when set — trackers.New reads it from here.
func TestLoadCarriesTheLinearKeyWhenSet(t *testing.T) {
	cfg, err := Load(fullEnv(nil))
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.LinearAPIKey == "" {
		t.Error("LinearAPIKey must be carried through to trackers.New")
	}
}
