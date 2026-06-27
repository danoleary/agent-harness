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
		"HERD_PATH":         "/Users/dan/herd",
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
	if cfg.HerdPath != "/Users/dan/herd" {
		t.Errorf("HerdPath = %q", cfg.HerdPath)
	}
	if cfg.Image != "herd-agent-harness:latest" {
		t.Errorf("Image = %q", cfg.Image)
	}
	if cfg.PnpmStoreVolume != "herd-pnpm-store" {
		t.Errorf("PnpmStoreVolume = %q", cfg.PnpmStoreVolume)
	}
	if cfg.TddTimeout != 30*time.Minute {
		t.Errorf("TddTimeout = %v, want 30m", cfg.TddTimeout)
	}
	if cfg.Model != "claude-opus-4-8" {
		t.Errorf("Model = %q, want claude-opus-4-8 (tdd sessions pin the exact Opus snapshot, not the floating alias)", cfg.Model)
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
}

func TestLoadHonoursOverrides(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"HARNESS_IMAGE":            "custom:tag",
		"PNPM_STORE_VOLUME":        "my-store",
		"TDD_TIMEOUT_MS":           "60000",
		"REVIEW_TIMEOUT_MS":        "120000",
		"RETROSPECTIVE_TIMEOUT_MS": "180000",
		"SESSION_IDLE_TIMEOUT_MS":  "30000", // 30s — kept below the 1m tdd cap (idle must stay under every cap)
		"TDD_MODEL":                "sonnet",
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
	if cfg.PnpmStoreVolume != "my-store" {
		t.Errorf("PnpmStoreVolume = %q", cfg.PnpmStoreVolume)
	}
	if cfg.TddTimeout != time.Minute {
		t.Errorf("TddTimeout = %v, want 1m", cfg.TddTimeout)
	}
	if cfg.Model != "sonnet" {
		t.Errorf("Model = %q, want sonnet (TDD_MODEL override)", cfg.Model)
	}
}

func TestLoadMissingRequired(t *testing.T) {
	for _, key := range []string{"GH_TOKEN", "LINEAR_API_KEY", "HERD_PATH"} {
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
