package git_test

import (
	"os"
	"path/filepath"
	"testing"

	gitpkg "github.com/danoleary/agent-harness/internal/git"
	"github.com/danoleary/agent-harness/internal/gittest"
)

// plant writes an executable script at path that, when anything runs it, leaves
// the file ran behind: the trace a sandbox-planted command would leave on the host.
func plant(t *testing.T, path, ran string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("#!/bin/sh\ntouch '"+ran+"'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
}

func assertNotRun(t *testing.T, ran string) {
	t.Helper()
	if _, err := os.Stat(ran); err == nil {
		t.Errorf("the planted command ran on the host (%s exists)", ran)
	}
}

// The sandbox mounts the Consumer checkout read-write, .git included, so a session
// can write a hook. `--no-verify` does not stop post-checkout, and the next ticket's
// `worktree add` would run it on the host, next to GH_TOKEN (#332).
func TestHostGitDoesNotRunAHookTheSandboxPlanted(t *testing.T) {
	repo := gittest.New(t)
	ran := filepath.Join(t.TempDir(), "ran")
	plant(t, filepath.Join(repo.Dir, ".git", "hooks", "post-checkout"), ran)

	wt := gitpkg.Open(repo.Dir, gittest.Prefix).WithoutRemoteRetry().Worktree("hooked")
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertNotRun(t, ran)
}

// A core.hooksPath the sandbox set in .git/config is a hook directory of its own
// choosing; the host's override must beat it too.
func TestHostGitIgnoresAHooksPathTheSandboxSet(t *testing.T) {
	repo := gittest.New(t)
	ran := filepath.Join(t.TempDir(), "ran")
	hooks := filepath.Join(t.TempDir(), "hooks")
	plant(t, filepath.Join(hooks, "post-checkout"), ran)
	repo.Git(repo.Dir, "config", "core.hooksPath", hooks)

	wt := gitpkg.Open(repo.Dir, gittest.Prefix).WithoutRemoteRetry().Worktree("hooked")
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	assertNotRun(t, ran)
}

// core.fsmonitor names a command git runs on every index refresh — `git status`
// included — and a hook-free `--no-verify` does nothing about it (#332).
func TestHostGitDoesNotRunAnFsmonitorTheSandboxSet(t *testing.T) {
	repo := gittest.New(t)
	ran := filepath.Join(t.TempDir(), "ran")
	monitor := filepath.Join(t.TempDir(), "fsmonitor")
	plant(t, monitor, ran)
	repo.Git(repo.Dir, "config", "core.fsmonitor", monitor)

	checkout := gitpkg.Open(repo.Dir, gittest.Prefix).WithoutRemoteRetry()
	wt := checkout.Worktree("monitored")
	if err := wt.Create(); err != nil {
		t.Fatalf("Create: %v", err)
	}
	wt.Clean()
	assertNotRun(t, ran)
}

// HostArgs is how a host-side git call outside this package's runners carries the
// same overrides: they come first, ahead of -C and the subcommand, where git reads
// a -c.
func TestHostArgsPutTheOverridesAheadOfTheCommand(t *testing.T) {
	got := gitpkg.HostArgs("-C", "/repo", "status")
	want := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-C", "/repo", "status"}
	if len(got) != len(want) {
		t.Fatalf("HostArgs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("HostArgs = %q, want %q", got, want)
		}
	}
}
