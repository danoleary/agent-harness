// Package config resolves harness configuration for a single tool invocation
// (implementation, review, retrospective).
package config

import (
	"fmt"
	"strconv"
	"time"
)

// Config is the resolved harness configuration.
type Config struct {
	// LinearAPIKey is the host-only Linear key (ADR-0001) — never passed into the sandbox.
	LinearAPIKey string
	// Image is the sandbox image tag.
	Image string
	// HerdPath is the absolute host path to the herd checkout to bind-mount.
	HerdPath string
	// PnpmStoreVolume is the Docker volume name for the persistent pnpm store.
	PnpmStoreVolume string
	// TddTimeout is the wall-clock cap for the tdd session.
	TddTimeout time.Duration
	// Model is the claude `--model` the tdd session runs on. Defaults to Opus —
	// the CLI's own default is not guaranteed to be Opus and a past run silently
	// fell back to Sonnet (BEH-316).
	Model string
}

const (
	defaultImage           = "herd-agent-harness:latest"
	defaultPnpmStoreVolume = "herd-pnpm-store"
	defaultTddTimeout      = 30 * time.Minute
	defaultModel           = "opus"
)

// Getenv looks up an environment variable by name, returning "" when unset.
type Getenv func(string) string

func requireEnv(get Getenv, key string) (string, error) {
	v := get(key)
	if v == "" {
		return "", fmt.Errorf("missing required env var: %s", key)
	}
	return v, nil
}

// Load reads harness config from the environment. The Claude credential is the
// only secret that crosses the sandbox boundary (ADR-0002); it is validated for
// presence here but not returned — it flows into the container via docker's
// `-e NAME` reading the harness's own inherited environment, so it never sits in
// our argv. GH_TOKEN is validated host-side too but stays host-only (the
// harness's own push + `gh pr create`); it never enters the container.
func Load(get Getenv) (Config, error) {
	// Exactly one Claude credential is required: a long-lived API key
	// (ANTHROPIC_API_KEY) or a subscription OAuth token (CLAUDE_CODE_OAUTH_TOKEN,
	// from `claude setup-token`). The latter must NOT be set as ANTHROPIC_API_KEY
	// — Claude Code would send it via x-api-key and Anthropic rejects it (BEH-316).
	if get("ANTHROPIC_API_KEY") == "" && get("CLAUDE_CODE_OAUTH_TOKEN") == "" {
		return Config{}, fmt.Errorf(
			"missing Claude credential: set ANTHROPIC_API_KEY (sk-ant-api03-…) " +
				"or CLAUDE_CODE_OAUTH_TOKEN (sk-ant-oat01-… from `claude setup-token`)",
		)
	}
	if _, err := requireEnv(get, "GH_TOKEN"); err != nil {
		return Config{}, err
	}

	linearKey, err := requireEnv(get, "LINEAR_API_KEY")
	if err != nil {
		return Config{}, err
	}
	herdPath, err := requireEnv(get, "HERD_PATH")
	if err != nil {
		return Config{}, err
	}

	return Config{
		LinearAPIKey:    linearKey,
		HerdPath:        herdPath,
		Image:           orDefault(get("HARNESS_IMAGE"), defaultImage),
		PnpmStoreVolume: orDefault(get("PNPM_STORE_VOLUME"), defaultPnpmStoreVolume),
		TddTimeout:      parseTimeout(get("TDD_TIMEOUT_MS")),
		Model:           orDefault(get("TDD_MODEL"), defaultModel),
	}, nil
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func parseTimeout(raw string) time.Duration {
	if raw == "" {
		return defaultTddTimeout
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return defaultTddTimeout
	}
	return time.Duration(ms) * time.Millisecond
}
