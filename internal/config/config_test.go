package config

import (
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
	if cfg.Model != "opus" {
		t.Errorf("Model = %q, want opus (tdd sessions must run on Opus)", cfg.Model)
	}
}

func TestLoadHonoursOverrides(t *testing.T) {
	cfg, err := Load(fullEnv(map[string]string{
		"HARNESS_IMAGE":     "custom:tag",
		"PNPM_STORE_VOLUME": "my-store",
		"TDD_TIMEOUT_MS":    "60000",
		"TDD_MODEL":         "sonnet",
	}))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
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
