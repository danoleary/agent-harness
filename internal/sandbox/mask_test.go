package sandbox

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The credential file may live in the Consumer checkout — `.agent-harness/.env` is
// the documented in-repo location — and that checkout is bind-mounted into every
// container at its real path. Masking it is therefore not optional hardening: it
// is what keeps the host-only tracker and GitHub tokens (ADR-0001/0002) out of a
// session that can read every other file in the tree.
func TestCredentialMaskHidesTheInRepoEnvFile(t *testing.T) {
	project := t.TempDir()
	env := filepath.Join(project, ".agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(env), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(env, []byte("GH_TOKEN=ghp_secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	masks := defaultCredentialMask(project)
	if len(masks) != 1 {
		t.Fatalf("got %d masks, want 1: %+v", len(masks), masks)
	}
	if masks[0].Target != env {
		t.Errorf("Target = %q, want the credential file %q", masks[0].Target, env)
	}
	// The source must be an EMPTY regular file: a container reading the masked path
	// must get nothing, and a regular file is the portable shape for a file-over-file
	// bind mount (a directory or a device is not).
	info, err := os.Stat(masks[0].Source)
	if err != nil {
		t.Fatalf("stat mask source: %v", err)
	}
	if !info.Mode().IsRegular() {
		t.Errorf("mask source %s is not a regular file (mode %s)", masks[0].Source, info.Mode())
	}
	if info.Size() != 0 {
		t.Errorf("mask source is %d bytes, want empty", info.Size())
	}
	// It must NOT be inside the checkout, or it would be mounted over itself.
	if strings.HasPrefix(masks[0].Source, project) {
		t.Errorf("mask source %s is inside the checkout %s", masks[0].Source, project)
	}
}

// The mask file has a fixed name and outlives the process, so every run after the
// first finds it already there. It must still be usable: a first run that left it
// read-only would make every later run fail to build a mask — and failing to mask
// is the one outcome that leaks the operator's tokens.
func TestCredentialMaskIsReusableAcrossRuns(t *testing.T) {
	project := t.TempDir()
	env := filepath.Join(project, ".agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(env), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(env, []byte("GH_TOKEN=ghp_secret\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	first := defaultCredentialMask(project)
	if len(first) != 1 {
		t.Fatalf("first call: got %d masks, want 1", len(first))
	}
	second := defaultCredentialMask(project) // must not panic or fail
	if len(second) != 1 || second[0].Source != first[0].Source {
		t.Fatalf("second call: got %+v, want the same mask as %+v", second, first)
	}
	info, err := os.Stat(second[0].Source)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if info.Size() != 0 {
		t.Errorf("mask source is %d bytes on reuse, want empty", info.Size())
	}
}

// Content left at the mask path by anything else must not survive: a container
// would read it at the masked path.
func TestCredentialMaskTruncatesADirtyMaskFile(t *testing.T) {
	path := filepath.Join(os.TempDir(), maskFileName)
	if err := os.WriteFile(path, []byte("leftover"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	project := t.TempDir()
	env := filepath.Join(project, ".agent-harness", ".env")
	if err := os.MkdirAll(filepath.Dir(env), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(env, []byte("K=V\n"), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}

	masks := defaultCredentialMask(project)
	if len(masks) != 1 {
		t.Fatalf("got %d masks, want 1", len(masks))
	}
	body, err := os.ReadFile(masks[0].Source)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(body) != 0 {
		t.Errorf("mask source still holds %q, want empty", body)
	}
}

// No credential file, no mount. Docker CREATES a missing bind-mount target, so
// masking unconditionally would litter a stray empty `.agent-harness/.env` into
// every Consumer checkout that never had one.
func TestCredentialMaskIsEmptyWhenNoEnvFileExists(t *testing.T) {
	if masks := defaultCredentialMask(t.TempDir()); len(masks) != 0 {
		t.Errorf("got %+v, want no masks", masks)
	}
}

// A directory at that path is not a credential file, and mounting a file over a
// directory fails the container outright.
func TestCredentialMaskIgnoresADirectory(t *testing.T) {
	project := t.TempDir()
	if err := os.MkdirAll(filepath.Join(project, ".agent-harness", ".env"), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if masks := defaultCredentialMask(project); len(masks) != 0 {
		t.Errorf("got %+v, want no masks", masks)
	}
}

// Every container that mounts the checkout must carry the mask, and it must be
// read-only so a session cannot write through it either. The session container is
// the one that runs the model, so it is the one that matters most.
func TestBuildDockerRunArgsMasksTheCredentialFile(t *testing.T) {
	restore := stubCredentialMask(t, []MaskMount{{Source: "/tmp/empty", Target: "/src/p/.agent-harness/.env"}})
	defer restore()

	args := BuildDockerRunArgs(Config{Image: "img", ProjectPath: "/src/p", Prompt: "hi"})
	want := "/tmp/empty:/src/p/.agent-harness/.env:ro"
	if !hasVolume(args, want) {
		t.Errorf("session args missing mask mount %q:\n%v", want, args)
	}
	// The mask must come AFTER the checkout mount, or the checkout mount replaces it.
	checkout := indexOfVolume(args, "/src/p:/src/p")
	mask := indexOfVolume(args, want)
	if checkout < 0 || mask < 0 || mask < checkout {
		t.Errorf("mask (%d) must follow the checkout mount (%d)", mask, checkout)
	}
}

// The gate and post_create containers mount the same checkout, so they need the
// same mask even though they carry no secrets of their own: the file is in the
// tree, not in their environment. They are one builder differing only in the
// command, so the mask is applied to whatever command runs there.
func TestWorktreeContainersMaskTheCredentialFile(t *testing.T) {
	restore := stubCredentialMask(t, []MaskMount{{Source: "/tmp/empty", Target: "/src/p/.agent-harness/.env"}})
	defer restore()

	gc := GateConfig{Image: "img", ProjectPath: "/src/p", WorktreePath: "/src/p/wt"}
	want := "/tmp/empty:/src/p/.agent-harness/.env:ro"
	for _, command := range []string{"pnpm run check", "pnpm install --frozen-lockfile"} {
		args := BuildWorktreeCommandArgs(gc, command)
		if !hasVolume(args, want) {
			t.Errorf("worktree container running %q missing mask mount %q:\n%v", command, want, args)
		}
	}
}

// stubCredentialMask swaps the filesystem-touching lookup for a fixed answer, so
// the arg-builder tests stay hermetic.
func stubCredentialMask(t *testing.T, masks []MaskMount) func() {
	t.Helper()
	orig := credentialMaskLookup
	credentialMaskLookup = func(string) []MaskMount { return masks }
	return func() { credentialMaskLookup = orig }
}

func indexOfVolume(args []string, spec string) int {
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" && args[i+1] == spec {
			return i
		}
	}
	return -1
}

func hasVolume(args []string, spec string) bool { return indexOfVolume(args, spec) >= 0 }

// writeFile lays body down at rel inside dir, making its parents.
func writeFile(t *testing.T, dir, rel, body string) string {
	t.Helper()
	path := filepath.Join(dir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatalf("write: %v", err)
	}
	return path
}

// A Consumer keeps more credentials in its tree than the harness's own: a
// service's infra/x/.env, a web app's .env.local. Every container mounts the whole
// checkout, so the mask is every `.env*` file in it, not one path (#332). A
// `.env.example` is committed documentation and stays readable.
func TestCredentialMaskHidesEveryEnvFileInTheCheckout(t *testing.T) {
	project := t.TempDir()
	secret := []string{".agent-harness/.env", "infra/x/.env", "web/.env.local", ".env.production", "api/.envrc", ".claude/worktrees/t/.env"}
	for _, rel := range secret {
		writeFile(t, project, rel, "TOKEN=secret\n")
	}
	writeFile(t, project, ".env.example", "TOKEN=\n")
	writeFile(t, project, "infra/x/.env.example", "TOKEN=\n")
	writeFile(t, project, "infra/x/env.go", "package x\n")

	got := map[string]bool{}
	for _, m := range defaultCredentialMask(project) {
		got[m.Target] = true
		if info, err := os.Stat(m.Source); err != nil || info.Size() != 0 {
			t.Errorf("mask source %s for %s is not an empty file", m.Source, m.Target)
		}
	}
	for _, rel := range secret {
		if !got[filepath.Join(project, rel)] {
			t.Errorf("%s is not masked; masks: %v", rel, got)
		}
	}
	if len(got) != len(secret) {
		t.Errorf("masked %d files, want %d: %v", len(got), len(secret), got)
	}
}

// The session container is the one that runs the model: a nested infra/x/.env is
// mounted over, read-only, with the empty file, after the checkout mount, so the
// container reads it empty.
func TestBuildDockerRunArgsMasksANestedEnvFile(t *testing.T) {
	project := t.TempDir()
	env := writeFile(t, project, "infra/x/.env", "TOKEN=secret\n")

	args := BuildDockerRunArgs(Config{Image: "img", ProjectPath: project, Prompt: "hi"})
	mask := -1
	for i := 0; i < len(args)-1; i++ {
		if args[i] == "-v" && strings.HasSuffix(args[i+1], ":"+env+":ro") {
			mask = i
			source := strings.TrimSuffix(args[i+1], ":"+env+":ro")
			if info, err := os.Stat(source); err != nil || info.Size() != 0 {
				t.Errorf("%s is mounted over with %s, not an empty file", env, source)
			}
		}
	}
	if checkout := indexOfVolume(args, project+":"+project); mask < 0 || mask < checkout {
		t.Errorf("no read-only mask of %s after the checkout mount (%d):\n%v", env, checkout, args)
	}
}

// The walk masks files that are there and creates none: Docker would create a
// missing mount target, so a directory with no .env must still have none after the
// masks are built. Nor does it descend into .git or node_modules, which hold no
// operator credentials and are where a large checkout's file count is.
func TestCredentialMaskCreatesNothingAndSkipsGitAndNodeModules(t *testing.T) {
	project := t.TempDir()
	writeFile(t, project, "infra/y/main.tf", "")
	writeFile(t, project, "node_modules/pkg/.env", "X=1\n")
	writeFile(t, project, ".git/.env", "X=1\n")

	if masks := defaultCredentialMask(project); len(masks) != 0 {
		t.Errorf("got %+v, want no masks", masks)
	}
	if _, err := os.Stat(filepath.Join(project, "infra", "y", ".env")); err == nil {
		t.Error("building the masks created infra/y/.env")
	}
}
