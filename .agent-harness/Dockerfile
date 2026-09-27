# The harness's own sandbox image: the harness works its own backlog as a
# Consumer (ADR-0008). The published base, plus the Go toolchain `make check`
# needs, and nothing else.
#
# Pin the IMMUTABLE semver tag, never `:latest`, so a base bump is a deliberate,
# reviewable edit rather than a silent floating pull.
FROM ghcr.io/danoleary/agent-harness-base:0.5.0

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

# /go is the cache path, which the harness chowns itself (HARNESS_CACHE_PATH).
ENV HARNESS_CHOWN_PATHS=""
