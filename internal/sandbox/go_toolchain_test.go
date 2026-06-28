package sandbox

import (
	"path/filepath"
	"regexp"
	"strconv"
	"testing"
)

// agent-harness/ is a pure-Go module, but the sandbox image is built off
// node:24-bookworm, which ships no Go toolchain. Without `go`/`gofmt` baked in,
// a Go-only ticket cannot run any of the harness's own gates in-session
// (`make check`/`make test`/`make build`/`make fmt-check` all shell out to
// `go`), so the red-green TDD loop and the pre-handoff compile/format check that
// web/ tickets get from pnpm are entirely absent for Go — the diff ships to the
// review session unverified (BEH-585). This pins "the image bakes a Go
// toolchain" as an enforced invariant, mirroring the Playwright/pnpm-store
// Dockerfile invariants in this package.
//
// The toolchain must be pinned (like CLAUDE_VERSION/PLAYWRIGHT_VERSION) so a
// build is reproducible and bumped deliberately, and it must land on PATH so the
// non-root `node` user the entrypoint drops to can actually invoke it.
func TestDockerfileBakesGoToolchain(t *testing.T) {
	root := repoRoot(t)
	dockerfile := mustRead(t, filepath.Join(root, "agent-harness", "Dockerfile"))

	if !regexp.MustCompile(`(?m)^ARG\s+GO_VERSION=[0-9][^\s]*\s*$`).MatchString(dockerfile) {
		t.Error("Dockerfile must pin the Go toolchain via `ARG GO_VERSION=<version>` so agent-harness/ tickets get the in-session go gates web/ tickets get from pnpm (BEH-585)")
	}

	// The install must actually fetch + unpack the pinned release (the official
	// go.dev tarball references the GO_VERSION arg) rather than apt's unpinned
	// `golang`, which would float and defeat the pin.
	if !regexp.MustCompile(`go\$\{GO_VERSION\}`).MatchString(dockerfile) {
		t.Error("Dockerfile must install the pinned Go release (a `go${GO_VERSION}` tarball), not an unpinned package (BEH-585)")
	}

	// Go must be on PATH for the unprivileged `node` user the entrypoint execs
	// under; the official tarball installs to /usr/local/go, so /usr/local/go/bin
	// must be added to PATH.
	if !regexp.MustCompile(`(?m)^ENV\s+PATH=.*?/usr/local/go/bin`).MatchString(dockerfile) {
		t.Error("Dockerfile must put /usr/local/go/bin on PATH so `go`/`gofmt` are invokable by the non-root sandbox user (BEH-585)")
	}
}

// The baked Go version must satisfy the module's own `go` directive in
// agent-harness/go.mod (the Makefile states "Requires Go 1.26+"). A drift where
// go.mod bumps past the baked toolchain would make `go build`/`go test` fail in
// the sandbox with a toolchain-too-old error — exactly the in-session gate this
// ticket restores. This turns the "must track" relationship into an enforced
// one, mirroring the Playwright version-pin invariant.
func TestBakedGoVersionSatisfiesGoMod(t *testing.T) {
	root := repoRoot(t)
	dockerfile := mustRead(t, filepath.Join(root, "agent-harness", "Dockerfile"))
	gomod := mustRead(t, filepath.Join(root, "agent-harness", "go.mod"))

	bakedMatch := regexp.MustCompile(`(?m)^ARG\s+GO_VERSION=([0-9]+)\.([0-9]+)`).FindStringSubmatch(dockerfile)
	if bakedMatch == nil {
		t.Fatal("could not find `ARG GO_VERSION=<major.minor...>` in agent-harness/Dockerfile")
	}
	modMatch := regexp.MustCompile(`(?m)^go\s+([0-9]+)\.([0-9]+)`).FindStringSubmatch(gomod)
	if modMatch == nil {
		t.Fatal("could not find `go <major.minor>` directive in agent-harness/go.mod")
	}

	bakedMajor, bakedMinor := atoi(t, bakedMatch[1]), atoi(t, bakedMatch[2])
	modMajor, modMinor := atoi(t, modMatch[1]), atoi(t, modMatch[2])

	if bakedMajor < modMajor || (bakedMajor == modMajor && bakedMinor < modMinor) {
		t.Errorf("Go version drift: Dockerfile bakes Go %d.%d but agent-harness/go.mod requires go %d.%d — bump the GO_VERSION ARG to match, or the sandbox can't build/test the module (BEH-585)", bakedMajor, bakedMinor, modMajor, modMinor)
	}
}

func atoi(t *testing.T, s string) int {
	t.Helper()
	n, err := strconv.Atoi(s)
	if err != nil {
		t.Fatalf("parse int %q: %v", s, err)
	}
	return n
}
