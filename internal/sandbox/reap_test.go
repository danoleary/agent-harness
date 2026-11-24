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

// hookSettings is the hooks half of the `--settings` JSON.
type hookSettings struct {
	Hooks map[string][]struct {
		Matcher string `json:"matcher"`
		Hooks   []struct {
			Type    string `json:"type"`
			Command string `json:"command"`
		} `json:"hooks"`
	} `json:"hooks"`
}

// Every sandboxed session carries the reaper and the interactive-git guard as
// PreToolUse hooks on Bash, and a SessionStart hook that installs the guard, in
// settings passed on the command line, so they reach the session without a new
// base image.
func TestEverySessionCarriesTheReaperAndTheGitGuardAsBashPreToolUseHooks(t *testing.T) {
	raw := valuesForFlag(BuildDockerRunArgs(baseConfig()), "--settings")
	if len(raw) != 1 {
		t.Fatalf("want one --settings, got %v", raw)
	}
	var settings hookSettings
	if err := json.Unmarshal([]byte(raw[0]), &settings); err != nil {
		t.Fatalf("--settings is not JSON: %v", err)
	}
	pre := settings.Hooks["PreToolUse"]
	if len(pre) != 1 || pre[0].Matcher != "Bash" || len(pre[0].Hooks) != 2 {
		t.Fatalf("want two PreToolUse hooks on Bash, got %+v", pre)
	}
	if h := pre[0].Hooks[0]; h.Type != "command" || h.Command != reapLeftoversCommand("/proc") {
		t.Errorf("the first hook must run the reaper over /proc, got %+v", h)
	}
	if h := pre[0].Hooks[1]; h.Type != "command" || h.Command != interactiveGitGuardCommand(guardDir) {
		t.Errorf("the second hook must run the interactive-git guard, got %+v", h)
	}
	start := settings.Hooks["SessionStart"]
	if len(start) != 1 || start[0].Matcher != "" || len(start[0].Hooks) != 1 || start[0].Hooks[0].Command != installGuardCommand(guardDir) {
		t.Errorf("want one SessionStart hook, on every source, that installs the guard in %s, got %+v", guardDir, start)
	}
}

// #433: the CLI prefixes a block's reason with the whole hook command, so the
// guard's PreToolUse command is one short line that runs the installed script,
// not the 3 KB of awk.
func TestTheGuardHookCommandIsOneShortLine(t *testing.T) {
	c := interactiveGitGuardCommand(guardDir)
	if strings.Contains(c, "\n") || len(c) > 120 || strings.Contains(c, "awk") {
		t.Errorf("the guard's hook command must be one short line naming the script, got %d bytes: %q", len(c), c)
	}
}

// guardHooks returns the guard's installer and hook commands as the session
// settings carry them, with the script installed under dir.
func guardHooks(t *testing.T, dir string) (install, guard string) {
	t.Helper()
	var settings hookSettings
	if err := json.Unmarshal([]byte(sessionSettingsIn(dir)), &settings); err != nil {
		t.Fatalf("session settings are not JSON: %v", err)
	}
	return settings.Hooks["SessionStart"][0].Hooks[0].Command, settings.Hooks["PreToolUse"][0].Hooks[1].Command
}

// runSh runs command under sh with stdin, and returns its exit code and stderr.
func runSh(t *testing.T, command, stdin string) (int, string) {
	t.Helper()
	c := exec.Command("sh", "-c", command)
	c.Stdin = strings.NewReader(stdin)
	var stderr strings.Builder
	c.Stderr = &stderr
	err := c.Run()
	if err == nil {
		return 0, stderr.String()
	}
	if ee, ok := err.(*exec.ExitError); ok {
		return ee.ExitCode(), stderr.String()
	}
	t.Fatalf("hook did not run: %v", err)
	return 0, ""
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

// runGuard runs the interactive-git guard as a session meets it: the
// SessionStart hook installs it, then Claude Code runs the PreToolUse hook. It
// returns the hook's exit code and stderr. Exit 2 blocks the Bash call and hands
// stderr to the session.
func runGuard(t *testing.T, command string) (int, string) {
	t.Helper()
	install, guard := guardHooks(t, t.TempDir())
	if code, stderr := runSh(t, install, `{"hook_event_name":"SessionStart","source":"startup"}`); code != 0 {
		t.Fatalf("the SessionStart hook failed to install the guard: exit %d; stderr %q", code, stderr)
	}
	event, _ := json.Marshal(map[string]any{
		"hook_event_name": "PreToolUse",
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": command},
	})
	return runSh(t, guard, string(event))
}

// #353, replayed: a compound Bash command with `git checkout -p` in it, its
// stderr on /dev/null, sat at the hunk prompt until the 10-minute Bash timeout.
// The guard blocks it before it runs, and says what to run instead.
func TestTheGuardBlocksTheCompoundCheckoutPatchThatHungTheSession(t *testing.T) {
	code, stderr := runGuard(t, "git status --short && git checkout -p -- internal/sandbox/reap.go 2>/dev/null; git diff --stat")
	if code != 2 {
		t.Fatalf("want exit 2 (block), got %d; stderr %q", code, stderr)
	}
	for _, want := range []string{"git checkout -p", "interactive", "git checkout -- <path>", "git restore <path>", "discard every uncommitted change", "copy the file aside"} {
		if !strings.Contains(stderr, want) {
			t.Errorf("the block message must name %q, got %q", want, stderr)
		}
	}
}

func TestTheGuardBlocksOnlyGitModesThatPrompt(t *testing.T) {
	for _, tc := range []struct {
		command string
		block   bool
	}{
		{"git checkout -p --", true},
		{"git add -p", true},
		{"git add --patch internal/x.go", true},
		{"git add -i", true},
		{"git restore -p internal/x.go", true},
		{"git reset --patch HEAD~1", true},
		{"git stash -p", true},
		{"git stash push --patch -m wip", true},
		{"git commit -p -m wip", true},
		{"git commit --interactive", true},
		{"git clean -i", true},
		{"git rebase -i origin/main", true},
		{"git -C /work/repo add -p", true},
		{"git -c core.pager=cat checkout --patch", true},
		{"GIT_PAGER=cat git add -p", true},
		{"timeout 60 git add -p", true},
		{"echo $(git add -p)", true},
		{"cd /work &&\n  git add -p", true},
		{"git status | cat; git reset -p", true},
		{"if git add -p; then :; fi", true},
		{"while ! git rebase -i origin/main; do :; done", true},

		{"git checkout -- file", false},
		{"git checkout main -- -p", false},
		{"git diff -p", false},
		{"git log -p -3", false},
		{"git show -p HEAD", false},
		{"git stash show -p", false},
		{"git stash list -p", false},
		{"git commit -i -m wip internal/x.go", false},
		{"git add internal/x.go && git commit -m 'stop git add -p hanging'", false},
		{`git commit -m "reject -p and --interactive"`, false},
		{"git commit -F - <<'EOF'\nReject interactive modes\n\ngit add -p\ngit checkout -p\nEOF\ngit log -1", false},
		{"grep -rn 'git add -p' docs/", false},
		{"echo git add -p", false},
		{"go test ./... # not git add -p", false},
		{"ls -p", false},
		{"", false},
	} {
		code, stderr := runGuard(t, tc.command)
		if tc.block && code != 2 {
			t.Errorf("%q: want blocked (exit 2), got exit %d", tc.command, code)
		}
		if !tc.block && code != 0 {
			t.Errorf("%q: want let through (exit 0), got exit %d; stderr %q", tc.command, code, stderr)
		}
	}
}

// A heredoc body that never ends must not leave the guard blocking or hanging.
func TestTheGuardLetsThroughAnUnterminatedHeredoc(t *testing.T) {
	if code, stderr := runGuard(t, "cat <<EOF\ngit add -p"); code != 0 {
		t.Errorf("want exit 0, got %d; stderr %q", code, stderr)
	}
}

// A guard script that was never installed, or was removed, lets the call
// through, as a failing awk does: the guard only saves time.
func TestTheGuardLetsThroughWhenItsScriptIsMissing(t *testing.T) {
	_, guard := guardHooks(t, t.TempDir())
	if code, stderr := runSh(t, guard, `{"tool_input":{"command":"git add -p"}}`); code != 0 {
		t.Errorf("want exit 0 with no installed script, got %d; stderr %q", code, stderr)
	}
}
