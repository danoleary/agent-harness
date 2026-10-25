package config

import (
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"testing"
)

// An env setting the harness reads belongs in three places: config.go, the
// docs/reference/env.md table and .env.example (whose scaffold copy the scaffold
// package drift-checks). Nothing else reminds a session of the last two, so a new
// setting wired into Load and nowhere else passed `make check` (#138).

// notInEnvExample are keys Load reads that deliberately have no line in
// .env.example.
var notInEnvExample = map[string]string{
	"HARNESS_ENV_FILE": "it chooses which env file is read, so it cannot be set inside one",
	"HOME":             "set by the OS; only used to find the config-dir env file",
	"XDG_CONFIG_HOME":  "set by the OS; only used to find the config-dir env file",
}

// notInEnvReference are keys Load reads that deliberately have no row in the
// docs/reference/env.md tables.
var notInEnvReference = map[string]string{
	"HOME":            "set by the OS; the env-file search order names it",
	"XDG_CONFIG_HOME": "set by the OS; the env-file search order names it",
}

// keysLoadReads is every env key Load and the env-file search ask for. The env is
// complete and uses the OAuth credential, so every lookup a valid run makes,
// CLAUDE_CODE_OAUTH_TOKEN included, is reached.
func keysLoadReads(t *testing.T) []string {
	t.Helper()
	env := fullEnv(t, map[string]string{
		"ANTHROPIC_API_KEY":       "",
		"CLAUDE_CODE_OAUTH_TOKEN": OAuthTokenPrefix + "x",
	})
	seen := map[string]bool{}
	record := func(k string) string {
		seen[k] = true
		return env(k)
	}
	if _, err := Load(record, WithHarnessVersion("dev")); err != nil {
		t.Fatalf("Load: %v", err)
	}
	EnvFileCandidates(record, t.TempDir())

	keys := make([]string, 0, len(seen))
	for k := range seen {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func TestEveryEnvKeyIsInEnvExample(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", ".env.example"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keysLoadReads(t) {
		if _, ok := notInEnvExample[key]; ok {
			continue
		}
		// Commented out or not: an optional knob is listed as `# KEY=default`.
		line := regexp.MustCompile(`(?m)^#?[ \t]*` + regexp.QuoteMeta(key) + `=`)
		if !line.Match(raw) {
			t.Errorf("config reads %s but .env.example does not list it; add a `# %s=<default>` line "+
				"and copy .env.example to internal/scaffold/templates/env.example", key, key)
		}
	}
}

func TestEveryEnvKeyIsInEnvReference(t *testing.T) {
	raw, err := os.ReadFile(filepath.Join("..", "..", "docs", "reference", "env.md"))
	if err != nil {
		t.Fatal(err)
	}
	for _, key := range keysLoadReads(t) {
		if _, ok := notInEnvReference[key]; ok {
			continue
		}
		row := regexp.MustCompile(`(?m)^\| ` + "`" + regexp.QuoteMeta(key) + "`" + ` \|`)
		if !row.Match(raw) {
			t.Errorf("config reads %s but docs/reference/env.md has no table row for it", key)
		}
	}
}
