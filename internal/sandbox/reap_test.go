package sandbox

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// fakeProc is a stand-in /proc: one directory per live pid, each holding the
// NUL-separated argv the reaper reads. The pids are real sleeping processes this
// test owns, so a kill lands on them and on nothing else on the machine.
type fakeProc struct {
	root  string
	procs map[string]*exec.Cmd
}

func newFakeProc(t *testing.T) *fakeProc {
	t.Helper()
	fp := &fakeProc{root: t.TempDir(), procs: map[string]*exec.Cmd{}}
	t.Cleanup(func() {
		for _, c := range fp.procs {
			_ = c.Process.Kill()
			_ = c.Wait()
		}
	})
	return fp
}

// spawn starts a sleeping process and records argv as its cmdline.
func (fp *fakeProc) spawn(t *testing.T, name string, argv ...string) {
	t.Helper()
	c := exec.Command("sleep", "60")
	if err := c.Start(); err != nil {
		t.Fatal(err)
	}
	fp.procs[name] = c
	dir := filepath.Join(fp.root, strconv.Itoa(c.Process.Pid))
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cmdline"), []byte(strings.Join(argv, "\x00")+"\x00"), 0o644); err != nil {
		t.Fatal(err)
	}
}

// alive reports whether the named process is still running once the reaper has
// had its chance.
func (fp *fakeProc) alive(t *testing.T, name string) bool {
	t.Helper()
	c := fp.procs[name]
	done := make(chan struct{})
	go func() { _ = c.Wait(); close(done) }()
	select {
	case <-done:
		return false
	case <-time.After(300 * time.Millisecond):
		return c.Process.Signal(syscall.Signal(0)) == nil
	}
}

// runHook runs the reaper as Claude Code runs a PreToolUse hook: through a shell,
// with the hook event JSON on stdin.
func runHook(t *testing.T, procRoot, command string) {
	t.Helper()
	event, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"cwd":             "/Users/dan/herd/.claude/worktrees/pr-1384",
		"tool_input":      map[string]any{"command": command},
	})
	c := exec.Command("sh", "-c", reapLeftoversCommand(procRoot))
	c.Stdin = strings.NewReader(string(event))
	if out, err := c.CombinedOutput(); err != nil {
		t.Fatalf("reaper hook failed: %v\n%s", err, out)
	}
}

// #248, replayed: an outer `timeout 500` killed pnpm but left the headless
// Chromium renderer and the vitest run it served behind, and they blocked the
// next story run. Before that next run starts, the reaper kills both, and leaves
// alone the claude session whose own argv (its prompt) names them.
func TestTheReaperKillsALeftoverBrowserAndVitestBeforeAStoryRun(t *testing.T) {
	fp := newFakeProc(t)
	fp.spawn(t, "renderer", "/home/node/.cache/ms-playwright/chromium_headless_shell-1187/chrome-headless-shell-linux64/chrome-headless-shell", "--type=renderer", "--headless")
	fp.spawn(t, "vitest", "node", "/Users/dan/herd/node_modules/.pnpm/vitest@3.2.4/node_modules/vitest/vitest.mjs", "run", "--project=storybook", "src/components/Swatch.stories.tsx")
	fp.spawn(t, "claude", "node", "/usr/local/bin/claude", "-p", "kill any leftover chrome-headless-shell and vitest processes")

	runHook(t, fp.root, "pnpm vitest run --project=storybook src/components/Swatch.stories.tsx")

	if fp.alive(t, "renderer") {
		t.Error("the leftover headless browser survived the story run's start")
	}
	if fp.alive(t, "vitest") {
		t.Error("the leftover vitest run survived the story run's start")
	}
	if !fp.alive(t, "claude") {
		t.Error("the reaper killed the claude session, whose prompt only names the browser")
	}
}

// A Bash command that runs no story leaves every process alone: the reaper only
// clears the way for a story run.
func TestTheReaperLeavesProcessesAloneForACommandThatRunsNoStory(t *testing.T) {
	fp := newFakeProc(t)
	fp.spawn(t, "renderer", "/opt/chrome-headless-shell-linux64/chrome-headless-shell", "--type=renderer")

	runHook(t, fp.root, "git status --short")

	if !fp.alive(t, "renderer") {
		t.Error("a command that runs no story must not reap the browser")
	}
}

// Every sandboxed session carries the reaper as a PreToolUse hook on Bash, in
// settings passed on the command line, so it reaches the session without a new
// base image.
func TestEverySessionCarriesTheReaperAsABashPreToolUseHook(t *testing.T) {
	raw := valuesForFlag(BuildDockerRunArgs(baseConfig()), "--settings")
	if len(raw) != 1 {
		t.Fatalf("want one --settings, got %v", raw)
	}
	var settings struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal([]byte(raw[0]), &settings); err != nil {
		t.Fatalf("--settings is not JSON: %v", err)
	}
	pre := settings.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher != "Bash" || len(pre[0].Hooks) != 1 {
		t.Fatalf("want one PreToolUse hook on Bash, got %+v", pre)
	}
	if h := pre[0].Hooks[0]; h.Type != "command" || h.Command != reapLeftoversCommand("/proc") {
		t.Errorf("the hook must run the reaper over /proc, got %+v", h)
	}
}

// #248: a session that drives no browser starts no MCP servers. The CLI's
// --strict-mcp-config with no --mcp-config ignores every server the project
// declares.
func TestASessionWithoutMCPPassesStrictMCPConfig(t *testing.T) {
	c := baseConfig()
	if slices.Contains(BuildDockerRunArgs(c), "--strict-mcp-config") {
		t.Error("a session may use the project's MCP servers unless told otherwise")
	}
	c.NoMCP = true
	args := BuildDockerRunArgs(c)
	if !slices.Contains(args, "--strict-mcp-config") {
		t.Error("a NoMCP session must pass --strict-mcp-config")
	}
	if slices.Contains(args, "--mcp-config") {
		t.Error("a NoMCP session must name no MCP config")
	}
}
