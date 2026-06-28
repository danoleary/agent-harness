# Sandbox image for one Claude Code skill session.
# Built once, reused per ticket. The herd checkout is bind-mounted at runtime at
# its real host path (ADR-0002); nothing herd-specific is baked in.
FROM node:24-bookworm

# --- pinned tool versions (override at build with --build-arg) ---
# claude must be pinned (AC); bump deliberately, never float to latest in a real build.
ARG CLAUDE_VERSION=2.0.14
ARG SUPABASE_VERSION=2.20.5
ARG GO_VERSION=1.26.2

ENV DEBIAN_FRONTEND=noninteractive

# Advertise the sandbox identity so the herd build skips its memory-hungry,
# post-`vite build` SPA-shell prerender/crawl step, which OOM-kills (exit 137)
# in this ~3.8GiB container even when the compile succeeds (BEH-470). Baked into
# the image so EVERY container from it — the agent session, the ground-truth gate
# re-run, and any manual `docker run` — gets a clean `pnpm run build` exit on a
# successful compile. `web/scripts/prerender-mode.ts` reads this; CI's full build
# (with prerender) stays the SSR-shell backstop. Mirrors Codex's CODEX_SANDBOX.
ENV HERD_SANDBOX=1

# git, bash, curl already partly present on the node image; supabase added below.
# No `gh`: the container never pushes or talks to GitHub (ADR-0002) — the harness
# owns all remote git I/O host-side, so GH_TOKEN never enters the image.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		ca-certificates curl gnupg git bash less jq gosu \
	&& rm -rf /var/lib/apt/lists/*

# supabase CLI (for schema-touching tdd tickets) — released .deb, arch-matched.
RUN arch="$(dpkg --print-architecture)" \
	&& curl -fsSL "https://github.com/supabase/cli/releases/download/v${SUPABASE_VERSION}/supabase_${SUPABASE_VERSION}_linux_${arch}.deb" -o /tmp/supabase.deb \
	&& dpkg -i /tmp/supabase.deb \
	&& rm /tmp/supabase.deb

# Go toolchain, pinned, baked in so agent-harness/ (pure-Go) tickets get the same
# in-session red-green + build + gofmt gates that web/ (pnpm) tickets get. Without
# it the node-based image has no `go`/`gofmt`, so `make check`/`make test`/`make
# build`/`make fmt-check` can't run in-session and a Go-only diff ships to the
# review session unverified (BEH-585). Installed from the official go.dev tarball
# (arch-matched, like supabase above) rather than apt's unpinned `golang` so the
# version is reproducible. GO_VERSION must satisfy agent-harness/go.mod's `go`
# directive (Makefile requires Go 1.26+) — bump it deliberately like
# CLAUDE_VERSION/PLAYWRIGHT_VERSION; the TestBakedGoVersionSatisfiesGoMod
# invariant fails CI on a drift. PATH puts /usr/local/go/bin ahead so the
# unprivileged `node` user the entrypoint execs under can invoke the toolchain.
ENV PATH=/usr/local/go/bin:$PATH
RUN arch="$(dpkg --print-architecture)" \
	&& curl -fsSL "https://go.dev/dl/go${GO_VERSION}.linux-${arch}.tar.gz" -o /tmp/go.tar.gz \
	&& tar -C /usr/local -xzf /tmp/go.tar.gz \
	&& rm /tmp/go.tar.gz

# pnpm via corepack (matches herd's package manager).
RUN corepack enable

# Point pnpm's content-addressed store at the persistent /pnpm-store volume the
# harness mounts into every sandbox + gate container. Without this the volume is
# dead weight: pnpm defaults its store under HOME, which is ephemeral in a `--rm`
# container, so each session (implementation install, review-session install,
# gate install, ci-fix) re-downloads every dependency from a cold store — the
# multi-minute "full pnpm install" tax (BEH-481). Set as a `pnpm_config_*` env
# rather than a global rc file because the entrypoint re-execs under gosu onto the
# checkout owner's uid; an env var is uid/HOME-independent and survives that switch,
# where a build-time rc under a fixed home would not. The prefix MUST be
# `pnpm_config_` (snake_cased `storeDir`), not `npm_config_`: pnpm 11 (herd pins
# pnpm@11.1.3, run via corepack) dropped reading npm-style config env/files, so an
# `npm_config_store_dir` would be silently ignored and the volume would stay dead
# weight. The store volume and the bind-mounted node_modules live on different
# filesystems, so pnpm copies rather than hard-links (it warns) — that's expected
# and still kills the network re-fetch, which is the cost that matters. Mirrors
# pnpm's official Docker guide (separate volume for the store).
ENV pnpm_config_store_dir=/pnpm-store

# pinned claude CLI.
RUN npm install -g "@anthropic-ai/claude-code@${CLAUDE_VERSION}"

# Playwright Chromium + its OS libraries, baked in for the in-session Storybook /
# a11y / E2E gates that drive a real browser (BEH-405). Without this a fresh
# worktree dies twice: first on a missing browser binary, then — once that's
# downloaded — on missing system libs (libnss3, libgbm1, libasound2, …) whose only
# documented fix is `sudo playwright install-deps`, and the sandbox has no sudo.
# Baking both into the image makes the gates runnable with no per-run download and
# no privilege. `--with-deps` installs the apt libraries (root, build-time) and the
# browser lands in PLAYWRIGHT_BROWSERS_PATH, which both the session and gate
# containers inherit. PLAYWRIGHT_VERSION must track web's `@playwright/test`
# (web/package.json) — bump it deliberately, like CLAUDE_VERSION above; a drift only
# costs a one-time re-download into the node-owned browsers dir, never a failure.
ARG PLAYWRIGHT_VERSION=1.60.0
ENV PLAYWRIGHT_BROWSERS_PATH=/ms-playwright
RUN npx -y "playwright@${PLAYWRIGHT_VERSION}" install --with-deps chromium \
	&& chown -R node:node /ms-playwright \
	&& rm -rf /var/lib/apt/lists/*

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Claude must run as a non-root user: `claude --dangerously-skip-permissions`
# refuses to start under root/sudo ("cannot be used with root/sudo privileges
# for security reasons"), which would exit the session 1 before /tdd ever
# creates a worktree (BEH-316). The node image ships an unprivileged `node` user
# (uid 1000) — pre-create the mount points owned by it so a fresh named volume
# initialises node-owned (Docker copies the image dir's ownership into an empty
# named volume on first mount). The herd checkout's mount target is the real host
# path and is created by Docker at runtime, so it is not pre-created here.
#
# We do NOT pin `USER node` here: on Linux the bind-mounted herd checkout is
# owned by the host uid (often != 1000), which a fixed uid couldn't write. The
# container instead enters as root and entrypoint.sh re-homes `node` onto the
# checkout's owner uid/gid and drops to it with gosu before exec'ing claude.
RUN mkdir -p /findings /pnpm-store \
	&& chown -R node:node /findings /pnpm-store

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
