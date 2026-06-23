package sandbox

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/beherd/agent-harness/internal/proc"
)

// smokeRunner bounds each docker call generously: the resolve itself is instant,
// but the image's first `pnpm` call may have corepack fetch the pinned pnpm.
func smokeRunner(name string, args ...string) ([]byte, error) {
	return proc.CombinedOutput(5*time.Minute, name, args...)
}

func smokeImage() string {
	if img := os.Getenv("HARNESS_IMAGE"); img != "" {
		return img
	}
	return "herd-agent-harness:latest"
}

// fakeStoreRunner returns a runner that records the command it was asked to run
// and replies with one canned (out, err), so the resolve/compare logic can be
// driven without Docker.
func fakeStoreRunner(out []byte, err error, gotCmd *string) func(string, ...string) ([]byte, error) {
	return func(name string, args ...string) ([]byte, error) {
		if gotCmd != nil {
			*gotCmd = strings.Join(append([]string{name}, args...), " ")
		}
		return out, err
	}
}

// storePathUnderMount is the comparison at the heart of the behavioral check:
// pnpm's *resolved* store path (from `pnpm store path`) must live under the
// mounted volume. pnpm appends a store-version segment (e.g. `/v11`), so the
// resolved path is the mount itself or a child of it — never the ephemeral
// HOME default the BEH-481 regression fell back to.
func TestStorePathUnderMount(t *testing.T) {
	mount := "/pnpm-store"
	cases := []struct {
		name     string
		resolved string
		want     bool
	}{
		{"mount itself", "/pnpm-store", true},
		{"versioned child (pnpm appends /v11)", "/pnpm-store/v11", true},
		{"ephemeral HOME default — the BEH-481 no-op", "/home/node/.local/share/pnpm/store/v3", false},
		{"root HOME default", "/root/.local/share/pnpm/store/v3", false},
		{"sibling that merely shares the prefix", "/pnpm-store-evil/v3", false},
		{"empty (resolution produced nothing)", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := storePathUnderMount(tc.resolved, mount); got != tc.want {
				t.Errorf("storePathUnderMount(%q, %q) = %v, want %v", tc.resolved, mount, got, tc.want)
			}
		})
	}
}

// When pnpm inside the image resolves its store under the mounted volume, the
// behavioral check passes — and it asks pnpm to *resolve* the path (`store
// path`) rather than echo config, so it measures the effect, not the text. The
// runner is invoked with `pnpm store path` past the entrypoint (which requires
// HERD_PATH), and the trailing newline docker emits is tolerated.
func TestVerifyPnpmStoreInImagePassesWhenResolvedUnderMount(t *testing.T) {
	var cmd string
	run := fakeStoreRunner([]byte("/pnpm-store/v11\n"), nil, &cmd)
	if err := verifyPnpmStoreInImage("herd-agent-harness:latest", "/pnpm-store", run); err != nil {
		t.Fatalf("expected pass when pnpm resolves under the mount, got: %v", err)
	}
	if !strings.Contains(cmd, "--entrypoint pnpm") {
		t.Errorf("check must bypass the entrypoint (it needs HERD_PATH) via `--entrypoint pnpm`, got: %s", cmd)
	}
	if !strings.Contains(cmd, "store path") {
		t.Errorf("check must resolve the effective store via `pnpm store path`, not echo config, got: %s", cmd)
	}
}

// The whole point of the behavioral check: when the wiring is a silent no-op and
// pnpm falls back to its ephemeral HOME default (the BEH-481 regression a
// text-grep on the Dockerfile happily passes), the check goes RED. This is the
// case the invariant-text pattern could never catch.
func TestVerifyPnpmStoreInImageFailsOnHomeDefaultNoOp(t *testing.T) {
	run := fakeStoreRunner([]byte("/home/node/.local/share/pnpm/store/v3\n"), nil, nil)
	err := verifyPnpmStoreInImage("herd-agent-harness:latest", "/pnpm-store", run)
	if err == nil {
		t.Fatal("expected an error when pnpm resolves to the ephemeral HOME default, got nil")
	}
	if !strings.Contains(err.Error(), "/pnpm-store") || !strings.Contains(err.Error(), "no-op") {
		t.Errorf("error should name the mount and call out the silent no-op, got: %v", err)
	}
}

// `docker run` merges stderr into stdout, and the image's first `pnpm` call has
// corepack print a "! Corepack is about to download …" notice before pnpm runs.
// The real store path is pnpm's final output line, so the check must read the
// last non-empty line — not treat the whole blob as the path (observed running
// the real image, BEH-487).
func TestVerifyPnpmStoreInImageToleratesCorepackChatter(t *testing.T) {
	out := []byte("! Corepack is about to download https://registry.npmjs.org/pnpm/-/pnpm-11.8.0.tgz\n/pnpm-store/v11\n")
	if err := verifyPnpmStoreInImage("herd-agent-harness:latest", "/pnpm-store", fakeStoreRunner(out, nil, nil)); err != nil {
		t.Fatalf("expected pass when corepack chatter precedes the store path, got: %v", err)
	}
}

// A `docker run` failure (daemon down, image missing) must surface docker's own
// reason rather than be mistaken for a passing/failing store path.
func TestVerifyPnpmStoreInImageSurfacesRunFailure(t *testing.T) {
	run := fakeStoreRunner(
		[]byte("Unable to find image 'herd-agent-harness:latest' locally\ndocker: Error response from daemon: pull access denied.\nSee 'docker run --help'."),
		errFake, nil,
	)
	err := verifyPnpmStoreInImage("herd-agent-harness:latest", "/pnpm-store", run)
	if err == nil {
		t.Fatal("expected an error when the docker run itself fails, got nil")
	}
	if !strings.Contains(err.Error(), "pull access denied") {
		t.Errorf("error should carry docker's own reason, got: %v", err)
	}
}

// The behavioral smoke check against the real image. It is the only layer that
// catches a config line that is present but wired to a prefix the image's pnpm
// doesn't honour, so it must run against an actual container — but a container
// (and a multi-minute image build) is too heavy for the default `make test`
// lane. It is therefore opt-in via HARNESS_DOCKER_SMOKE=1 (set by `make smoke`
// and the CI image-smoke job); the unit tests above keep the resolve/compare
// logic covered on every run. With the env set it still skips — never fails —
// when Docker is genuinely unavailable, so it can't wedge a non-Docker runner.
func TestPnpmStoreResolvesUnderMountedVolumeInImage(t *testing.T) {
	if os.Getenv("HARNESS_DOCKER_SMOKE") == "" {
		t.Skip("set HARNESS_DOCKER_SMOKE=1 (or run `make smoke`) to run the in-image pnpm store-dir behavioral check")
	}
	if _, err := exec.LookPath("docker"); err != nil {
		t.Skipf("HARNESS_DOCKER_SMOKE set but docker is not on PATH: %v", err)
	}
	if out, err := smokeRunner("docker", "info"); err != nil {
		t.Skipf("HARNESS_DOCKER_SMOKE set but the docker daemon is unreachable: %s", reasonOr(out, err))
	}

	image := smokeImage()
	if _, err := smokeRunner("docker", "image", "inspect", image); err != nil {
		// Build on miss so the check is self-contained — opting in means we want
		// the full behavioral verification, not a skip.
		if berr := BuildImage(image, filepath.Join(repoRoot(t), "agent-harness")); berr != nil {
			t.Fatalf("building sandbox image %q for the smoke check failed: %v", image, berr)
		}
	}

	if err := verifyPnpmStoreInImage(image, PnpmStoreMountPath, smokeRunner); err != nil {
		t.Error(err)
	}
}
