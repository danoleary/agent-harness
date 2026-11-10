package sandbox

import (
	"os/exec"
	"regexp"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/danoleary/agent-harness/internal/proc"
)

// claudeHelpTimeout bounds each `claude ... --help` probe; it reads no network.
const claudeHelpTimeout = 60 * time.Second

// helpFlagRe matches the option column of one `claude --help` line, e.g.
// `  -p, --print` or `  --settings <file-or-json>`. Flags named only inside a
// description (`... add --strict-mcp-config to skip`) sit further right and
// don't match, so a flag counts as known only where the CLI declares it.
var helpFlagRe = regexp.MustCompile(`^ {2}(?:(-[A-Za-z]), )?(--[A-Za-z0-9-]+)`)

// requireClaude skips the test where the Claude CLI isn't installed (host CI);
// inside the sandbox it is the pinned Dockerfile.base CLAUDE_VERSION.
func requireClaude(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("claude"); err != nil {
		t.Skip("claude CLI not on PATH; runs inside the sandbox image")
	}
}

// knownClaudeFlags parses the flags the installed CLI declares in its help.
func knownClaudeFlags(t *testing.T) map[string]bool {
	t.Helper()
	out, err := proc.CombinedOutput(claudeHelpTimeout, "claude", "--help")
	if err != nil {
		t.Fatalf("claude --help: %v\n%s", err, out)
	}
	known := map[string]bool{}
	for _, line := range strings.Split(string(out), "\n") {
		m := helpFlagRe.FindStringSubmatch(line)
		if m == nil {
			continue
		}
		if m[1] != "" {
			known[m[1]] = true
		}
		known[m[2]] = true
	}
	if len(known) == 0 {
		t.Fatalf("parsed no flags from claude --help; has its layout changed?\n%s", out)
	}
	return known
}

// launcherConfigs enumerates every combination of the optional Config fields,
// so a flag the launcher emits only under one option is still collected.
func launcherConfigs() []Config {
	type option func(*Config)
	options := []option{
		func(c *Config) { c.Model = "claude-opus-5-5" },
		func(c *Config) { c.NoMCP = true },
		func(c *Config) { c.ContainerName = "harness-x" },
		func(c *Config) { c.CacheVolume, c.CacheMountPath = "cache", "/cache" },
		func(c *Config) { c.FindingsDir = "/tmp/findings" },
		func(c *Config) { c.WorkDir = "/repo/.claude/worktrees/x" },
	}
	var configs []Config
	for mask := 0; mask < 1<<len(options); mask++ {
		c := Config{Image: "img", ProjectPath: "/repo", Prompt: "the prompt"}
		for i, apply := range options {
			if mask&(1<<i) != 0 {
				apply(&c)
			}
		}
		configs = append(configs, c)
	}
	return configs
}

// claudeFlags returns the flags in the claude half of a docker run argv
// (everything after the image's `claude` command).
func claudeFlags(t *testing.T, args []string, image string) []string {
	t.Helper()
	i := slices.Index(args, image)
	if i < 0 || i+1 >= len(args) || args[i+1] != "claude" {
		t.Fatalf("no `%s claude` in argv: %q", image, args)
	}
	var flags []string
	for _, a := range args[i+2:] {
		if strings.HasPrefix(a, "-") {
			flags = append(flags, a)
		}
	}
	return flags
}

// TestEveryLauncherFlagIsKnownToThePinnedCLI checks each flag the sandbox
// launcher can pass to claude against the installed CLI's own help (#261). A
// flag the CLI doesn't declare would fail, or be silently ignored by, every
// session of a Stage in production while argv-shape tests stay green.
// `claude --help` exits 0 even given an unknown flag, so the help text is the
// only honest answer.
func TestEveryLauncherFlagIsKnownToThePinnedCLI(t *testing.T) {
	requireClaude(t)
	known := knownClaudeFlags(t)

	seen := map[string]bool{}
	for _, c := range launcherConfigs() {
		for _, f := range claudeFlags(t, BuildDockerRunArgs(c), c.Image) {
			seen[f] = true
		}
	}
	for _, want := range []string{"--settings", "--strict-mcp-config", "--model"} {
		if !seen[want] {
			t.Fatalf("enumeration never produced %s; launcherConfigs misses an option", want)
		}
	}
	for f := range seen {
		if !known[f] {
			t.Errorf("launcher passes %s, which the installed claude --help does not declare", f)
		}
	}
}

// TestSessionSettingsJSONParsesInThePinnedCLI confirms the CLI reads the inline
// `--settings` value as JSON rather than as a settings-file path (#261): given
// a string it can't parse, it reports "Settings file not found" and exits 1.
func TestSessionSettingsJSONParsesInThePinnedCLI(t *testing.T) {
	requireClaude(t)
	if out, err := proc.CombinedOutput(claudeHelpTimeout, "claude", "--settings", sessionSettings(), "--help"); err != nil {
		t.Fatalf("claude --settings <sessionSettings> --help: %v\n%s", err, out)
	}
	// Guard the probe itself: unparseable JSON must fail it, or the check above
	// would pass whatever the launcher sends.
	if out, err := proc.CombinedOutput(claudeHelpTimeout, "claude", "--settings", "{not json", "--help"); err == nil {
		t.Fatalf("claude accepted unparseable --settings JSON, so this probe proves nothing:\n%s", out)
	}
}
