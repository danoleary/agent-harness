package sandbox

import (
	"os"
	"os/exec"
	"strings"
	"testing"
)

// The checkout is a host (macOS) tree bind-mounted into a linuxkit container, so
// the ctime and inode git recorded in the index on the host differ from what the
// container's stat returns. git's racy-index check then treats clean files as
// dirty: `git rebase origin/main` refused with "Your local changes … would be
// overwritten" on a tree `git status` called clean, and `update-index --refresh`
// did not heal it (#151). Every container that runs git on the mount carries
// core.checkStat=minimal and core.trustctime=false via GIT_CONFIG_COUNT env, so
// git compares only whole-second mtime and size — values the bind mount
// preserves. The env, not the entrypoint, carries it: env beats every config
// level and reaches a Consumer's already-built image on the next run.
func TestContainersIgnoreBindMountStatDrift(t *testing.T) {
	cases := map[string][]string{
		"session":     BuildDockerRunArgs(baseConfig()),
		"gate":        BuildWorktreeCommandArgs(cacheGateConfig(), "make check"),
		"post_create": BuildWorktreeCommandArgs(cacheGateConfig(), "make setup"),
	}
	for name, args := range cases {
		t.Run(name, func(t *testing.T) {
			env := containerGitEnv(args)
			if got := gitConfigUnder(t, env, "core.checkStat"); got != "minimal" {
				t.Errorf("core.checkStat = %q, want minimal — else inode/ctime drift across the bind mount reads as local changes", got)
			}
			if got := gitConfigUnder(t, env, "core.trustctime"); got != "false" {
				t.Errorf("core.trustctime = %q, want false — the host's ctime never matches the container's", got)
			}
		})
	}
}

// containerGitEnv is the GIT_CONFIG_* `-e NAME=value` pairs the argv sets.
func containerGitEnv(args []string) []string {
	var env []string
	for _, e := range valuesForFlag(args, "-e") {
		if strings.HasPrefix(e, "GIT_CONFIG_") && strings.Contains(e, "=") {
			env = append(env, e)
		}
	}
	return env
}

// gitConfigUnder asks a real git what key resolves to with env applied, in a
// fresh repo isolated from the user's own config, so the test pins what git
// actually reads rather than the shape of the env.
func gitConfigUnder(t *testing.T, env []string, key string) string {
	t.Helper()
	repo := t.TempDir()
	if out, err := exec.Command("git", "init", "-q", repo).CombinedOutput(); err != nil {
		t.Fatalf("git init: %v: %s", err, out)
	}
	cmd := exec.Command("git", "-C", repo, "config", "--get", key)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	cmd.Env = append(cmd.Env, env...)
	out, _ := cmd.Output() // exits 1 when the key is unset; the empty value says so
	return strings.TrimSpace(string(out))
}
