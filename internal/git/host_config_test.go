package git_test

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
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

// The ticket's scenario (#351): a session writes a filter driver into .git/config
// and a `.gitattributes` that routes every file through it. A fixed -c list cannot
// name an arbitrary filter, so the host's next `worktree add` would run the smudge
// command on checkout. The host refuses to run git at all while the repository's
// own config names a command.
func TestHostGitDoesNotRunAFilterTheSandboxSet(t *testing.T) {
	repo := gittest.New(t)
	ran := filepath.Join(t.TempDir(), "ran")
	smudge := filepath.Join(t.TempDir(), "smudge")
	plant(t, smudge, ran)
	repo.Merge(".gitattributes", "* filter=x\n", "route every file through filter x")
	repo.Git(repo.Dir, "config", "filter.x.smudge", smudge)

	wt := gitpkg.Open(repo.Dir, gittest.Prefix).WithoutRemoteRetry().Worktree("filtered")
	err := wt.Create()
	assertNotRun(t, ran)
	if err == nil || !strings.Contains(err.Error(), "filter.x.smudge") {
		t.Fatalf("Create = %v, want a refusal naming filter.x.smudge", err)
	}
}

// Every command-valued key a host git operation can reach is refused, whichever
// command the host is about to run, and wherever in the repository's config it
// is set: the local .git/config or a file it includes.
func TestHostGitRefusesACommandValuedKeyInTheRepositoryConfig(t *testing.T) {
	for _, key := range []string{
		"filter.lfs.clean", "filter.x.process",
		"diff.external", "diff.x.textconv", "diff.x.command",
		"merge.x.driver",
		"core.sshCommand", "core.pager", "core.editor", "core.askPass", "core.gitProxy", "core.alternateRefsCommand",
		"pager.log", "sequence.editor",
		"credential.helper", "credential.https://example.com.helper",
		"remote.origin.uploadpack", "remote.origin.receivepack",
		"gpg.program", "gpg.ssh.program", "gpg.ssh.defaultKeyCommand",
	} {
		t.Run(key, func(t *testing.T) {
			repo := gittest.New(t)
			repo.Git(repo.Dir, "config", key, "/tmp/planted")
			err := gitpkg.Open(repo.Dir, gittest.Prefix).WithoutRemoteRetry().FetchMain()
			if err == nil || !strings.Contains(strings.ToLower(err.Error()), strings.ToLower(key)) {
				t.Fatalf("FetchMain = %v, want a refusal naming %s", err, key)
			}
		})
	}
}

func TestHostGitRefusesACommandSetInAnIncludedFile(t *testing.T) {
	repo := gittest.New(t)
	included := filepath.Join(t.TempDir(), "included")
	if err := os.WriteFile(included, []byte("[diff]\n\texternal = /tmp/planted\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	repo.Git(repo.Dir, "config", "include.path", included)

	_, err := gitpkg.Open(repo.Dir, gittest.Prefix).MainSHA()
	if err == nil || !strings.Contains(err.Error(), "diff.external") || !strings.Contains(err.Error(), included) {
		t.Fatalf("MainSHA = %v, want a refusal naming diff.external in %s", err, included)
	}
}

// An empty value is no command: `credential.helper =` is how a config resets the
// helper list, and a repository that carries one still gets host git.
func TestHostGitRunsWithAnEmptyCommandValuedKey(t *testing.T) {
	repo := gittest.New(t)
	repo.Git(repo.Dir, "config", "credential.helper", "")
	if err := gitpkg.Open(repo.Dir, gittest.Prefix).WithoutRemoteRetry().FetchMain(); err != nil {
		t.Fatalf("FetchMain = %v, want it to run", err)
	}
}

// An ext:: remote runs its URL as a command. Git refuses it by default, but a
// session can set protocol.ext.allow; the host's own override beats that.
func TestHostGitDoesNotRunAnExtRemoteTheSandboxSet(t *testing.T) {
	repo := gittest.New(t)
	ran := filepath.Join(t.TempDir(), "ran")
	cmd := filepath.Join(t.TempDir(), "remote")
	plant(t, cmd, ran)
	repo.Git(repo.Dir, "config", "protocol.ext.allow", "always")
	repo.Git(repo.Dir, "remote", "set-url", "origin", "ext::"+cmd)

	_ = gitpkg.Open(repo.Dir, gittest.Prefix).WithoutRemoteRetry().FetchMain()
	assertNotRun(t, ran)
}

// HostArgs is how a host-side git call outside this package's runners gets the
// same check: it refuses to build the argv while the config of the -C directory
// names a command.
func TestHostArgsRefusesACommandInTheRepositoryConfig(t *testing.T) {
	repo := gittest.New(t)
	repo.Git(repo.Dir, "config", "core.sshCommand", "/tmp/planted")
	if _, err := gitpkg.HostArgs("-C", repo.Dir, "ls-remote", "origin"); err == nil || !strings.Contains(err.Error(), "core.sshcommand") {
		t.Fatalf("HostArgs = %v, want a refusal naming core.sshcommand", err)
	}
}

// HostArgs is how a host-side git call outside this package's runners carries the
// same overrides: they come first, ahead of -C and the subcommand, where git reads
// a -c.
func TestHostArgsPutTheOverridesAheadOfTheCommand(t *testing.T) {
	repo := gittest.New(t)
	got, err := gitpkg.HostArgs("-C", repo.Dir, "status")
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "protocol.ext.allow=never",
		"-c", "submodule.recurse=false", "-c", "fetch.recurseSubmodules=false", "-C", repo.Dir, "status"}
	if len(got) != len(want) {
		t.Fatalf("HostArgs = %q, want %q", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("HostArgs = %q, want %q", got, want)
		}
	}
}

// A Consumer command the host runs starts its own git, which no -c reaches. With
// HostEnv in its environment, that git ignores a core.hooksPath the sandbox set
// too (#350).
func TestHostEnvStopsAHookInAGitTheHostDidNotStart(t *testing.T) {
	repo := gittest.New(t)
	ran := filepath.Join(t.TempDir(), "ran")
	hooks := filepath.Join(t.TempDir(), "hooks")
	plant(t, filepath.Join(hooks, "post-checkout"), ran)
	repo.Git(repo.Dir, "config", "core.hooksPath", hooks)

	cmd := exec.Command("git", "-C", repo.Dir, "checkout", "-q", "-b", "probe")
	cmd.Env = append(os.Environ(), gitpkg.HostEnv()...)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git checkout: %v: %s", err, out)
	}
	assertNotRun(t, ran)
}
