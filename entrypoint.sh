#!/usr/bin/env bash
# Runs once per container before the claude session. Two passes:
#   1. As root: pick the unprivileged user to run as (the bind-mounted herd
#      checkout's owner, so worktree writes succeed regardless of host uid),
#      hand it the writable surfaces it needs, then re-exec under gosu.
#   2. As that user: set the commit identity, trust the repo, then exec the
#      claude session. The container never pushes (ADR-0002) — no push auth is
#      wired, and GH_TOKEN never enters the container.
# claude --dangerously-skip-permissions refuses to run as root, so pass 1 must
# always drop privileges (BEH-316).
set -euo pipefail

# The checkout is bind-mounted at its real host path (ADR-0002); the harness
# passes that path in as HERD_PATH so this entrypoint can stat the mount.
MOUNT="${HERD_PATH:?HERD_PATH must be set by the harness (the checkout mount path)}"

if [ "$(id -u)" = "0" ]; then
	# Match the runtime user to the checkout's owner. On Linux that's the host
	# uid (which may not be 1000); on macOS Docker Desktop bind-mount ownership
	# is virtualised so this is effectively a no-op.
	target_uid="$(stat -c '%u' "$MOUNT")"
	target_gid="$(stat -c '%g' "$MOUNT")"

	# A root-owned checkout would force claude back under root and trip its
	# root guard — fall back to the image's node user (1000) and accept that
	# writes then require a non-root host owner.
	if [ "$target_uid" = "0" ]; then
		target_uid=1000
		target_gid=1000
	fi

	# Re-home the `node` account onto the target uid/gid so getpwuid() lookups
	# (git, npm) resolve to a real user (-o allows a non-unique id if the
	# host uid collides with a baked-in account). Only when it differs — usermod
	# writes a "no changes" line straight to the terminal that no redirect can
	# swallow, so guard rather than suppress.
	current_uid="$(id -u node)"
	current_gid="$(id -g node)"
	if [ "$target_gid" != "$current_gid" ]; then
		groupmod -o -g "$target_gid" node
	fi
	if [ "$target_uid" != "$current_uid" ] || [ "$target_gid" != "$current_gid" ]; then
		usermod -o -u "$target_uid" -g "$target_gid" node
	fi

	# Hand the target user the surfaces the session writes that aren't the
	# (already-owned) bind mount: its home (git/npm/corepack/claude caches) and
	# the pnpm-store volume, which mounts root-owned on its first use on Linux.
	chown -R "$target_uid:$target_gid" /home/node /pnpm-store

	# Re-own the baked Playwright browsers (PLAYWRIGHT_BROWSERS_PATH=/ms-playwright,
	# chowned to the image's node uid 1000) so an in-session `playwright install`
	# can write there — the heal path when the Dockerfile's PLAYWRIGHT_VERSION
	# drifts from web's @playwright/test and the gates need the matching browser
	# revision (BEH-405). Launching the baked browser needs no chown (its files are
	# world-readable), so only pay the recursive walk of the ~GB tree when the uid
	# actually changed (Linux host uid != 1000); on the macOS/uid-1000 path it stays
	# node-owned and this is skipped.
	if [ "$target_uid" != "1000" ] && [ -d /ms-playwright ]; then
		chown -R "$target_uid:$target_gid" /ms-playwright
	fi

	exec gosu "$target_uid:$target_gid" "$0" "$@"
fi

# --- below here runs as the unprivileged target user ---
export HOME=/home/node

git config --global user.name "Herd Agent Harness"
git config --global user.email "agent-harness@beherd.co"

# The repo is bind-mounted from the host; without this git refuses to operate
# ("detected dubious ownership") when the worktree's owner doesn't match.
git config --global --add safe.directory '*'

exec "$@"
