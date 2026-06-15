// Package config resolves harness configuration for a single tool invocation
// (implementation, review, retrospective).
package config

import (
	"bufio"
	"fmt"
	"os"
	"strconv"
	"strings"
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
	// TddTimeout is the wall-clock cap for the tdd (implementation) session.
	TddTimeout time.Duration
	// ReviewTimeout is the wall-clock cap for the review session (DESIGN.md: 15 min).
	ReviewTimeout time.Duration
	// Model is the claude `--model` the tdd session runs on. Defaults to Opus —
	// the CLI's own default is not guaranteed to be Opus and a past run silently
	// fell back to Sonnet (BEH-316).
	Model string
}

const (
	defaultImage           = "herd-agent-harness:latest"
	defaultPnpmStoreVolume = "herd-pnpm-store"
	defaultTddTimeout      = 30 * time.Minute
	defaultReviewTimeout   = 15 * time.Minute
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
		TddTimeout:      parseTimeout(get("TDD_TIMEOUT_MS"), defaultTddTimeout),
		ReviewTimeout:   parseTimeout(get("REVIEW_TIMEOUT_MS"), defaultReviewTimeout),
		Model:           orDefault(get("TDD_MODEL"), defaultModel),
	}, nil
}

// LoadDotEnv loads KEY=VALUE pairs from a .env file into the process environment
// for any key not already set (mirrors `node --env-file-if-exists`). An absent
// file is a no-op. It is shared by the cmd entrypoints so each stays a thin
// wrapper. Call it before Load so the file fills any gaps the real env leaves.
func LoadDotEnv(path string) {
	f, err := os.Open(path)
	if err != nil {
		return
	}
	defer f.Close()

	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, val, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		key = strings.TrimSpace(key)
		val = strings.Trim(strings.TrimSpace(val), `"'`)
		if _, exists := os.LookupEnv(key); !exists {
			_ = os.Setenv(key, val)
		}
	}
}

func orDefault(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}

func parseTimeout(raw string, fallback time.Duration) time.Duration {
	if raw == "" {
		return fallback
	}
	ms, err := strconv.Atoi(raw)
	if err != nil || ms <= 0 {
		return fallback
	}
	return time.Duration(ms) * time.Millisecond
}
