# The harness's own sandbox image: the harness works its own backlog as a
# Consumer (ADR-0008). The published base, plus the Go toolchain `make check`
# needs, and nothing else.
#
# Pin the IMMUTABLE semver tag, never `:latest`, so a base bump is a deliberate,
# reviewable edit rather than a silent floating pull.
FROM ghcr.io/danoleary/agent-harness-base:0.6.2

# Match go.mod's `go` line. TARGETARCH is set by BuildKit (amd64 | arm64).
ARG GO_VERSION=1.26.2
ARG TARGETARCH
RUN curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${TARGETARCH}.tar.gz" \
	| tar -C /usr/local -xz

# The module and build caches live on the `[cache]` volume, or every session
# pays a cold `go mod download` and a cold compile: HOME is ephemeral in a `--rm`
# container. GOTOOLCHAIN=local stops go from fetching a different toolchain.
ENV PATH=/usr/local/go/bin:/go/bin:$PATH \
	GOPATH=/go \
	GOMODCACHE=/go/pkg/mod \
	GOCACHE=/go/cache \
	GOTOOLCHAIN=local \
	GOFLAGS=-buildvcs=false

# ENV alone is not enough: post_create and every Gate run as `bash -lc`, and a
# login shell's /etc/profile RESETS PATH, so go and gofmt vanished there and
# `make check` exited 127 host-side on a branch that passed it in-session (#40).
# /etc/profile sources profile.d after its reset, so put the toolchain back on
# PATH there. The other GO* vars survive a login shell untouched.
# scripts/check-sandbox-image.sh, run by CI, probes exactly this.
RUN printf '%s\n' \
	'case ":$PATH:" in *:/usr/local/go/bin:*) ;; *) PATH="/usr/local/go/bin:/go/bin:$PATH" ;; esac' \
	'export PATH' \
	>/etc/profile.d/go.sh

# /go is the cache path, which the harness chowns itself (HARNESS_CACHE_PATH).
ENV HARNESS_CHOWN_PATHS=""
