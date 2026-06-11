# Sandbox image for one Claude Code skill session.
# Built once, reused per ticket. The herd checkout is bind-mounted at runtime
# (see DESIGN.md "Fixed bind-mount path"); nothing herd-specific is baked in.
FROM node:24-bookworm

# --- pinned tool versions (override at build with --build-arg) ---
# claude must be pinned (AC); bump deliberately, never float to latest in a real build.
ARG CLAUDE_VERSION=2.0.14
ARG SUPABASE_VERSION=2.20.5

ENV DEBIAN_FRONTEND=noninteractive

# git, bash, curl already partly present on the node image; gh + supabase added below.
RUN apt-get update \
	&& apt-get install -y --no-install-recommends \
		ca-certificates curl gnupg git bash less jq gosu \
	&& mkdir -p -m 755 /etc/apt/keyrings \
	&& curl -fsSL https://cli.github.com/packages/githubcli-archive-keyring.gpg \
		| tee /etc/apt/keyrings/githubcli-archive-keyring.gpg > /dev/null \
	&& chmod go+r /etc/apt/keyrings/githubcli-archive-keyring.gpg \
	&& echo "deb [arch=$(dpkg --print-architecture) signed-by=/etc/apt/keyrings/githubcli-archive-keyring.gpg] https://cli.github.com/packages stable main" \
		> /etc/apt/sources.list.d/github-cli.list \
	&& apt-get update \
	&& apt-get install -y --no-install-recommends gh \
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

COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# Claude must run as a non-root user: `claude --dangerously-skip-permissions`
# refuses to start under root/sudo ("cannot be used with root/sudo privileges
# for security reasons"), which would exit the session 1 before /tdd ever
# creates a worktree (BEH-316). The node image ships an unprivileged `node` user
# (uid 1000) — pre-create the mount points owned by it so a fresh named volume
# initialises node-owned (Docker copies the image dir's ownership into an empty
# named volume on first mount).
#
# We do NOT pin `USER node` here: on Linux the bind-mounted herd checkout is
# owned by the host uid (often != 1000), which a fixed uid couldn't write. The
# container instead enters as root and entrypoint.sh re-homes `node` onto the
# checkout's owner uid/gid and drops to it with gosu before exec'ing claude.
RUN mkdir -p /workspace/herd /findings /pnpm-store \
	&& chown -R node:node /workspace /findings /pnpm-store

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
