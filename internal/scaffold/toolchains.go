package scaffold

import (
	"cmp"
	"encoding/json"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"

	"github.com/danoleary/agent-harness/internal/config"
)

// Toolchain is one day-one project toolchain `init` knows the defaults for: how
// to recognise it, and what it writes for it. The defaults are recorded in
// ADR-0021 and each one is taken from a Consumer config the harness already
// ships or documents — the harness's own (Go), example/ (.NET), herd and
// docs/CONSUMER.md (pnpm) — except Python, which has no prior Consumer.
type Toolchain struct {
	// Name is what --toolchain and the interactive prompt take.
	Name string
	// Label is how the generated files name it.
	Label string
	// PostCreate is the per-worktree setup command (idempotent).
	PostCreate string
	// Gates are the host-side gates, in order.
	Gates []config.Gate
	// HandoffStripPaths is the regenerable build output stripped on handoff.
	HandoffStripPaths []string
	// Cache is the persistent cache the image points its toolchain at; a zero
	// CacheSpec writes no [cache] table.
	Cache CacheSpec
	// Layer is the Dockerfile block added on top of the base image.
	Layer string
	// ChownPaths is the image's HARNESS_CHOWN_PATHS.
	ChownPaths string

	// markers are checkout-root globs whose presence identifies the toolchain.
	markers []string
	// tailor adjusts the defaults to what the checkout declares (go.mod's Go
	// version, package.json's scripts, a uv lockfile).
	tailor func(dir string, tc *Toolchain)
}

// CacheSpec is a [cache] table without the volume name, which is per project.
type CacheSpec struct {
	// Suffix names the volume `<project>-<suffix>`.
	Suffix string
	Path   string
	Prune  string
}

// toolchains is the detection order: when a checkout matches more than one, the
// first is the default and the interactive prompt offers the rest.
func toolchains() []Toolchain {
	return []Toolchain{
		{
			Name:       "go",
			Label:      "Go",
			PostCreate: "go mod download",
			Gates: []config.Gate{
				{Name: "test", Command: "go test ./..."},
				{Name: "vet", Command: "go vet ./..."},
			},
			Cache: CacheSpec{Suffix: "go", Path: "/go", Prune: "go clean -cache"},
			Layer: `# Match go.mod's Go. TARGETARCH is set by BuildKit (amd64 | arm64).
ARG GO_VERSION=<<GO_VERSION>>
ARG TARGETARCH
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
	| tar -C /usr/local -xz

# The module and build caches live on the [cache] volume: HOME is ephemeral in a
# --rm container. GOTOOLCHAIN=local stops go fetching a different toolchain.
ENV PATH=/usr/local/go/bin:/go/bin:$PATH \
	GOPATH=/go \
	GOMODCACHE=/go/pkg/mod \
	GOCACHE=/go/cache \
	GOTOOLCHAIN=local
`,
			markers: []string{"go.mod"},
			tailor:  tailorGo,
		},
		{
			Name:              "pnpm",
			Label:             "Node + pnpm",
			PostCreate:        "pnpm install --frozen-lockfile",
			HandoffStripPaths: []string{"node_modules"},
			Cache:             CacheSpec{Suffix: "pnpm", Path: "/pnpm-store", Prune: "pnpm store prune"},
			Layer: `# node is already in the base. pnpm comes through corepack, at the version
# package.json's "packageManager" field pins; COREPACK_HOME is baked, so it is
# listed in HARNESS_CHOWN_PATHS below.
ENV COREPACK_HOME=/opt/corepack \
	COREPACK_ENABLE_DOWNLOAD_PROMPT=0 \
	npm_config_store_dir=/pnpm-store
RUN mkdir -p /opt/corepack && corepack enable pnpm
`,
			ChownPaths: "/opt/corepack",
			markers:    []string{"pnpm-lock.yaml", "pnpm-workspace.yaml"},
			tailor:     tailorPnpm,
		},
		{
			Name:       "dotnet",
			Label:      ".NET",
			PostCreate: "dotnet restore",
			Gates: []config.Gate{
				{Name: "test", Command: "dotnet test"},
				{Name: "format", Command: "dotnet format --verify-no-changes"},
			},
			HandoffStripPaths: []string{"obj", "bin"},
			Cache:             CacheSpec{Suffix: "nuget", Path: "/root/.nuget/packages", Prune: "dotnet nuget locals all --clear"},
			Layer: `ARG DOTNET_VERSION=9.0
RUN apt-get update \
	&& apt-get install -y --no-install-recommends "dotnet-sdk-${DOTNET_VERSION}" \
	&& rm -rf /var/lib/apt/lists/*

# Point NuGet at the [cache] volume: HOME is ephemeral in a --rm container.
ENV NUGET_PACKAGES=/root/.nuget/packages
`,
			markers: []string{"*.sln", "*.slnx", "*.csproj", "*.fsproj", "global.json"},
		},
		{
			Name:              "python",
			Label:             "Python (uv)",
			PostCreate:        "uv sync",
			Gates:             []config.Gate{{Name: "test", Command: "uv run pytest"}},
			HandoffStripPaths: []string{".venv"},
			Cache:             CacheSpec{Suffix: "uv", Path: "/uv-cache"},
			Layer: `# uv manages the virtualenv, and the interpreter if pyproject.toml asks for one
# the base's python3 is not. Pin it: https://astral.sh/uv/<version>/install.sh
RUN curl -LsSf https://astral.sh/uv/install.sh \
	| env UV_INSTALL_DIR=/usr/local/bin UV_NO_MODIFY_PATH=1 sh

# Packages and downloaded interpreters live on the [cache] volume: HOME is
# ephemeral in a --rm container.
ENV UV_CACHE_DIR=/uv-cache/cache \
	UV_PYTHON_INSTALL_DIR=/uv-cache/python \
	UV_LINK_MODE=copy
`,
			markers: []string{"pyproject.toml", "uv.lock", "requirements.txt"},
			tailor:  tailorPython,
		},
	}
}

// toolchainNames lists every --toolchain value.
func toolchainNames() []string {
	var names []string
	for _, tc := range toolchains() {
		names = append(names, tc.Name)
	}
	return names
}

// byName returns the named toolchain, tailored to the checkout.
func byName(dir, name string) (Toolchain, bool) {
	for _, tc := range toolchains() {
		if tc.Name == name {
			if tc.tailor != nil {
				tc.tailor(dir, &tc)
			}
			return tc, true
		}
	}
	return Toolchain{}, false
}

// Detect returns the first toolchain whose marker files are at the checkout
// root, tailored to it.
func Detect(dir string) (Toolchain, bool) {
	for _, tc := range toolchains() {
		if hasAny(dir, tc.markers) {
			return byName(dir, tc.Name)
		}
	}
	return Toolchain{}, false
}

func hasAny(dir string, globs []string) bool {
	for _, g := range globs {
		if m, _ := filepath.Glob(filepath.Join(dir, g)); len(m) > 0 {
			return true
		}
	}
	return false
}

// defaultGoVersion is the Go the harness itself builds with, used when go.mod
// declares none.
const defaultGoVersion = "1.26.2"

var goDirective = regexp.MustCompile(`(?m)^(go|toolchain)\s+(?:go)?(\d+\.\d+(?:\.\d+)?)\s*$`)

// tailorGo installs the Go that go.mod asks for — its `toolchain` line if it has
// one, else its `go` line. go.dev only publishes full x.y.z archives, so a bare
// `go 1.24` is 1.24.0.
func tailorGo(dir string, tc *Toolchain) {
	var goLine, toolchainLine string
	if raw, err := os.ReadFile(filepath.Join(dir, "go.mod")); err == nil {
		for _, m := range goDirective.FindAllStringSubmatch(string(raw), -1) {
			if m[1] == "toolchain" {
				toolchainLine = m[2]
			} else {
				goLine = m[2]
			}
		}
	}
	version := cmp.Or(toolchainLine, goLine, defaultGoVersion)
	if strings.Count(version, ".") == 1 {
		version += ".0"
	}
	tc.Layer = strings.ReplaceAll(tc.Layer, "<<GO_VERSION>>", version)
}

// pnpmGateScripts are the package.json scripts that become gates, in gate order.
var pnpmGateScripts = []string{"check", "typecheck", "lint", "test"}

// tailorPnpm gates on the package.json scripts the project actually has, so no
// gate names a script that does not exist. With none of them, `pnpm test` stands
// in for the Consumer to replace.
func tailorPnpm(dir string, tc *Toolchain) {
	var pkg struct {
		Scripts map[string]string `json:"scripts"`
	}
	if raw, err := os.ReadFile(filepath.Join(dir, "package.json")); err == nil {
		_ = json.Unmarshal(raw, &pkg)
	}
	tc.Gates = nil
	for _, s := range pnpmGateScripts {
		if _, ok := pkg.Scripts[s]; ok {
			tc.Gates = append(tc.Gates, config.Gate{Name: s, Command: "pnpm " + s})
		}
	}
	if len(tc.Gates) == 0 {
		tc.Gates = []config.Gate{{Name: "test", Command: "pnpm test"}}
	}
}

// tailorPython keeps post_create lockfile-frozen when there is a lockfile, and
// falls back to a plain requirements install when there is no pyproject.toml.
func tailorPython(dir string, tc *Toolchain) {
	switch {
	case hasAny(dir, []string{"uv.lock"}):
		tc.PostCreate = "uv sync --frozen"
	case hasAny(dir, []string{"pyproject.toml"}):
		tc.PostCreate = "uv sync"
	default:
		tc.PostCreate = "uv venv --allow-existing\nuv pip install -r requirements.txt"
	}
}

// tomlString renders s as a TOML basic string. Go's quoting is a subset of
// TOML's for the printable text a config holds.
func tomlString(s string) string { return strconv.Quote(s) }
