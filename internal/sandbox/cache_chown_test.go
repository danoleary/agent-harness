package sandbox

import (
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
)

// harnessRoot walks up from this test file to the HARNESS module root — the dir
// holding go.mod and entrypoint.sh. It anchors on the harness's own files, not on
// any Consumer's layout, so this invariant keeps holding once the harness is
// extracted to its standalone repo (ADR-0007) and there is no herd checkout above
// it. Consumer-image invariants (the Playwright pin, the pnpm store wiring) are a
// Consumer's own guards and do not belong in this module.
func harnessRoot(t *testing.T) string {
	t.Helper()
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	dir := filepath.Dir(thisFile)
	for {
		_, gErr := os.Stat(filepath.Join(dir, "go.mod"))
		_, eErr := os.Stat(filepath.Join(dir, "entrypoint.sh"))
		if gErr == nil && eErr == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("could not locate the harness module root (go.mod + entrypoint.sh) above " + thisFile)
		}
		dir = parent
	}
}

// cacheGateConfig is a worktree-container config with a cache declared.
func cacheGateConfig() GateConfig {
	return GateConfig{
		Image:          "example-agent-harness:latest",
		ProjectPath:    "/Users/dan/proj",
		WorktreePath:   "/Users/dan/proj/.claude/worktrees/beh-641",
		CacheVolume:    "myproj-nuget",
		CacheMountPath: "/root/.nuget/packages",
		ContainerName:  "example-harness-gate-1",
	}
}

// A Consumer's toolchain cache is a named Docker volume, and a named volume mounts
// ROOT-owned on its first use on Linux. The session runs unprivileged (the
// entrypoint drops from root to the checkout owner, BEH-316), so it cannot write
// that fresh volume unless the entrypoint chowns it first. The entrypoint used to
// chown the literal /pnpm-store, which only worked because herd was the only
// Consumer; a shared base image (ADR-0007) cannot know the path, because it is the
// Consumer's `[cache].path` (ADR-0008). So the harness passes it by value as
// HARNESS_CACHE_PATH, and these tests pin that it travels with EVERY container
// that mounts the volume — otherwise the first install into a fresh cache fails
// with EACCES and the failure surfaces as an opaque non-zero gate.
func TestPassesCachePathToEntrypointWhenCacheMounted(t *testing.T) {
	c := baseConfig()
	c.CacheVolume = "myproj-nuget"
	c.CacheMountPath = "/root/.nuget/packages"

	args := BuildDockerRunArgs(c)
	if got := envValue(args, "HARNESS_CACHE_PATH"); got != c.CacheMountPath {
		t.Errorf(
			"session container HARNESS_CACHE_PATH = %q, want the mounted cache path %q — "+
				"without it the entrypoint cannot chown a fresh root-owned volume and the "+
				"unprivileged session fails its first install with EACCES",
			got, c.CacheMountPath,
		)
	}
}

// The gate and post_create containers mount the same volume, so they need the same
// chown — and since they are now the same builder differing only in the command,
// that agreement is structural rather than a property two functions must maintain.
// post_create is the one that actually populates a cold cache, so a miss here is
// the likeliest failure.
func TestPassesCachePathToEntrypointInWorktreeContainers(t *testing.T) {
	c := cacheGateConfig()
	c.CacheVolume = "myproj-nuget"
	c.CacheMountPath = "/root/.nuget/packages"

	if got := envValue(BuildWorktreeCommandArgs(c, "true"), "HARNESS_CACHE_PATH"); got != c.CacheMountPath {
		t.Errorf("worktree container HARNESS_CACHE_PATH = %q, want %q", got, c.CacheMountPath)
	}
}

// No volume means no chown target. Passing a path for an unmounted cache would make
// the entrypoint chown a directory the image happens to bake, which is not the
// Consumer's intent and could be an expensive recursive walk.
func TestOmitsCachePathWhenNoCacheVolume(t *testing.T) {
	c := baseConfig()
	c.CacheVolume = ""

	if got := envValue(BuildDockerRunArgs(c), "HARNESS_CACHE_PATH"); got != "" {
		t.Errorf("HARNESS_CACHE_PATH = %q, want unset when no cache volume is declared", got)
	}

	g := cacheGateConfig()
	g.CacheVolume = ""
	if got := envValue(BuildWorktreeCommandArgs(g, "true"), "HARNESS_CACHE_PATH"); got != "" {
		t.Errorf("gate HARNESS_CACHE_PATH = %q, want unset when no cache volume is declared", got)
	}
}

// The entrypoint ships in the shared base image every Consumer runs (ADR-0007), so
// a project-specific path baked into it is a defect that silently degrades every
// OTHER Consumer: a .NET project would have its NuGet cache left root-owned while
// the entrypoint chowns a /pnpm-store that does not exist. These are the two paths
// that were hardcoded before the split; the test pins that they left, and that the
// generic replacements are actually wired.
func TestEntrypointCarriesNoProjectSpecificPaths(t *testing.T) {
	entrypoint := mustRead(t, filepath.Join(harnessRoot(t), "entrypoint.sh"))

	// A comment may legitimately name herd's paths as the worked example, so only
	// flag them where they are operative — inside a chown command.
	for _, banned := range []string{"/pnpm-store", "/ms-playwright"} {
		bad := regexp.MustCompile(`(?m)^\s*[^#\n]*chown[^\n]*` + regexp.QuoteMeta(banned))
		if bad.MatchString(entrypoint) {
			t.Errorf(
				"entrypoint.sh chowns the project-specific path %q; the base image is shared "+
					"by every Consumer (ADR-0007), so this surface must come from "+
					"HARNESS_CACHE_PATH (harness-supplied) or HARNESS_CHOWN_PATHS "+
					"(Consumer-image-supplied) instead",
				banned,
			)
		}
	}

	for _, required := range []string{"HARNESS_CACHE_PATH", "HARNESS_CHOWN_PATHS"} {
		if !strings.Contains(entrypoint, required) {
			t.Errorf("entrypoint.sh must honour %s so a Consumer can declare its writable surfaces", required)
		}
	}
}

// mustRead reads a repo file into a string or fails the test.
func mustRead(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	return string(b)
}

// One host can run the harness against several projects at once, and cmd/loop's
// hard-abort path kills containers by `docker ps --filter name=<prefix>`. So the
// prefix must be Consumer-scoped: with a shared constant (it was "herd-harness-"),
// Ctrl-C on one project would kill another project's in-flight session — the
// session is detached in its own process group precisely so a signal cannot reach
// it, which is what makes the name the only handle.
func TestContainerPrefixIsScopedToTheConsumer(t *testing.T) {
	a := ContainerPrefix("/Users/dan/src/alpha")
	b := ContainerPrefix("/Users/dan/src/beta")

	if a == b {
		t.Fatalf("two Consumers share the container prefix %q; one project's abort would kill the other's session", a)
	}
	if !strings.HasPrefix(a, "alpha") || !strings.HasPrefix(b, "beta") {
		t.Errorf("prefixes should name their Consumer, got %q and %q", a, b)
	}
	// A `docker ps --filter name=` match is a substring match, so one prefix must
	// not be a prefix of another, or the shorter name's abort reaps the longer's.
	if strings.HasPrefix(ContainerPrefix("/x/app"), ContainerPrefix("/x/app2")) ||
		strings.HasPrefix(ContainerPrefix("/x/app2"), ContainerPrefix("/x/app")) {
		t.Error("one Consumer's prefix is a prefix of another's; the kill filter would over-match")
	}
}

// Docker accepts only [a-zA-Z0-9][a-zA-Z0-9_.-]* as a container name, and a
// checkout directory can be called anything. A name Docker rejects fails the run
// at `docker run`, long after the harness has claimed the ticket.
func TestContainerPrefixIsAlwaysAValidDockerName(t *testing.T) {
	valid := regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*$`)

	for _, path := range []string{
		"/Users/dan/src/my project", // a space
		"/Users/dan/src/my.project", // a dot
		"/Users/dan/src/Ölprojekt",  // non-ASCII
		"/Users/dan/src/proj/",      // a trailing separator
		"/Users/dan/src/---",        // folds away entirely
		"/",                         // no basename at all
	} {
		got := ContainerPrefix(path)
		if !valid.MatchString(got) {
			t.Errorf("ContainerPrefix(%q) = %q, which Docker will reject as a container name", path, got)
		}
	}
}
