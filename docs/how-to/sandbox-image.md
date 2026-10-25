# Build a custom sandbox image

Every Stage, every Gate and your `post_create` hook run in your project's
sandbox image. That image layers your toolchain on the harness's published
**sandbox base image**, which carries the Claude CLI, `git`, `gosu` and the
uid-re-exec entrypoint (ADR-0008). `agent-harness init` writes a starting
Dockerfile for Go, pnpm, .NET or Python; this guide is for changing it or writing
your own.

## 1. Write a Dockerfile `FROM` a pinned base

Save it as `.agent-harness/Dockerfile`:

```dockerfile
FROM ghcr.io/danoleary/agent-harness-base:0.7.0

# Your toolchain. Everything here is yours; the base carries only the
# harness<->sandbox contract (claude CLI, git, gosu, the uid-re-exec entrypoint).
RUN apt-get update \
	&& apt-get install -y --no-install-recommends dotnet-sdk-9.0 \
	&& rm -rf /var/lib/apt/lists/*

# A tool installed outside /usr/bin goes on PATH here, never in a profile script:
# ENV PATH=/opt/mytool/bin:$PATH

# Image-internal paths your toolchain needs WRITABLE at run time, colon-separated.
# The container enters as root and drops to the bind-mounted checkout's owner uid,
# so anything you bake owned by the image's uid 1000 is unwritable when the host
# uid differs. The entrypoint chowns each path in this list when that happens.
#
# Your CACHE path does not go here — the harness passes that itself.
ENV HARNESS_CHOWN_PATHS=/opt/my-baked-tool
```

Pin the immutable semver tag, never `:latest`. A base bump should be a
deliberate, reviewable edit, not a silent floating pull. Each harness release
names the base it was built with.

## 2. Point `config.toml` at it

Declare **exactly one** of `dockerfile` or `image`:

```toml
image = "myproject-agent-harness:latest"   # the tag the harness builds it as
dockerfile = ".agent-harness/Dockerfile"   # build it on a local image miss, and when it changes
```

To use an image you build and publish yourself instead, set only `image` to its
ref; the harness pulls it on a miss. It still has to be built `FROM` the base.

## 3. Put every tool on the image `PATH`

The harness runs `post_create` and every Gate as a non-login `bash -c <command>`,
so `/etc/profile` and `~/.profile` are **not** sourced. A `PATH` you set only in
`/etc/profile.d/*.sh` never reaches a gate: the gate exits 127, and the run stops
as an environment failure. Use `ENV PATH=...` in the Dockerfile.

Check it before the first run:

```bash
docker build -f .agent-harness/Dockerfile -t myproject-agent-harness:latest .
docker run --rm -e PROJECT_PATH=/tmp myproject-agent-harness:latest bash -c 'command -v dotnet'
```

From a harness checkout, `scripts/check-sandbox-image.sh <image> <tool>...` checks
several tools at once.

## 4. Add a cache, if your toolchain has one

A persistent volume saves re-downloading dependencies in every container:

```toml
[cache]
volume = "myproject-nuget"
path = "/root/.nuget/packages"
prune_command = "dotnet nuget locals all --clear"   # runs on the HOST when disk is low
```

## Changing the image in a ticket

A ticket that edits this Dockerfile has its `post_create` and gates run in an
image built from the branch's copy (`<image>:branch-<fingerprint>`), so a fix to
the toolchain is gated in the image it fixes (ADR-0022). Only the Dockerfile's
own bytes are compared: a branch that changes only a file the Dockerfile `COPY`s
still runs in the current image.

## See also

- [`image`, `dockerfile` and `[cache]`](../reference/config.md)
- `agent-harness doctor` checks the sandbox image along with the rest of the setup.
