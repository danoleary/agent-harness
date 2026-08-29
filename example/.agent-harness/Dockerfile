# An example Consumer sandbox image (ADR-0008): the published base, plus this
# project's toolchain and nothing else.
#
# Pin the IMMUTABLE semver tag, never `:latest`, so a base bump is a deliberate,
# reviewable edit rather than a silent floating pull.
FROM ghcr.io/danoleary/agent-harness-base:0.1.0

ARG DOTNET_VERSION=9.0

# The project toolchain. The base already carries the claude CLI, git, bash and
# the uid-re-exec entrypoint — do not reinstall those.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends "dotnet-sdk-${DOTNET_VERSION}" \
	&& rm -rf /var/lib/apt/lists/*

# Point the toolchain's cache at the volume this Consumer declares in
# `[cache].path`, or the mounted volume is dead weight: the default cache lives
# under HOME, which is ephemeral in a `--rm` container, so every session would pay
# a cold network restore.
ENV NUGET_PACKAGES=/root/.nuget/packages

# Image-internal paths this layer bakes that the session must be able to WRITE.
# The container enters as root and drops to the bind-mounted checkout's owner uid,
# so anything baked owned by the image's uid 1000 is unwritable when the host uid
# differs; the entrypoint chowns each path listed here when that happens.
# Colon-separated. The CACHE path does NOT belong here — the harness passes that
# itself as HARNESS_CACHE_PATH.
ENV HARNESS_CHOWN_PATHS=""
