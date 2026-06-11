#!/usr/bin/env bash
# Runs once per container before the claude session. Two passes:
#   1. As root: pick the unprivileged user to run as (the bind-mounted herd
#      checkout's owner, so worktree writes succeed regardless of host uid),
#      hand it the writable surfaces it needs, then re-exec under gosu.
#   2. As that user: set the commit identity, trust the repo, wire git to push
#      over HTTPS with GH_TOKEN, then exec the claude session.
# claude --dangerously-skip-permissions refuses to run as root, so pass 1 must
# always drop privileges (BEH-316).
set -euo pipefail

MOUNT=/workspace/herd

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
	# (git, gh, npm) resolve to a real user (-o allows a non-unique id if the
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

	exec gosu "$target_uid:$target_gid" "$0" "$@"
fi

# --- below here runs as the unprivileged target user ---
export HOME=/home/node

git config --global user.name "Herd Agent Harness"
git config --global user.email "agent-harness@beherd.co"

# The repo is bind-mounted from the host; without this git refuses to operate
# ("detected dubious ownership") when the worktree's owner doesn't match.
git config --global --add safe.directory '*'

# Let `git push` (review session) authenticate with the GH_TOKEN PAT.
if [ -n "${GH_TOKEN:-}" ]; then
	gh auth setup-git
fi

exec "$@"
