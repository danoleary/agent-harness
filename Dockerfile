# Sandbox image for one Claude Code skill session.
# Built once, reused per ticket. The herd checkout is bind-mounted at runtime at
# its real host path (ADR-0002); nothing herd-specific is baked in.
FROM node:24-bookworm

# --- pinned tool versions (override at build with --build-arg) ---
# claude must be pinned (AC); bump deliberately, never float to latest in a real build.
ARG CLAUDE_VERSION=2.0.14
ARG SUPABASE_VERSION=2.20.5

ENV DEBIAN_FRONTEND=noninteractive

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

# pnpm via corepack (matches herd's package manager).
RUN corepack enable

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
