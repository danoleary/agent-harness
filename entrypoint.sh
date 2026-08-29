#!/usr/bin/env bash
# Runs once per container before the claude session. Two passes:
#   1. As root: pick the unprivileged user to run as (the bind-mounted Consumer
#      checkout's owner, so worktree writes succeed regardless of host uid),
#      hand it the writable surfaces it needs, then re-exec under gosu.
#   2. As that user: set the commit identity, trust the repo, then exec the
#      claude session. The container never pushes (ADR-0002) — no push auth is
#      wired, and GH_TOKEN never enters the container.
# claude --dangerously-skip-permissions refuses to run as root, so pass 1 must
# always drop privileges (BEH-316).
set -euo pipefail

# The checkout is bind-mounted at its real host path (ADR-0002); the harness
# passes that path in as PROJECT_PATH so this entrypoint can stat the mount.
MOUNT="${PROJECT_PATH:?PROJECT_PATH must be set by the harness (the checkout mount path)}"

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

	# Hand the target user its home (git/npm/corepack/claude caches), which the
	# session always writes and the bind mount does not cover.
	chown -R "$target_uid:$target_gid" /home/node

	# The Consumer's toolchain cache volume, when one is declared. The harness
	# passes its in-container mount path (ADR-0008's `[cache].path`) as
	# HARNESS_CACHE_PATH, because a named volume mounts root-owned on its first
	# use on Linux and the unprivileged session could not write it. Unconditional
	# (not uid-gated): the volume is fresh on first mount whatever the host uid is.
	if [ -n "${HARNESS_CACHE_PATH:-}" ] && [ -d "$HARNESS_CACHE_PATH" ]; then
		chown -R "$target_uid:$target_gid" "$HARNESS_CACHE_PATH"
	fi

	# Image-internal paths a Consumer layer bakes and needs writable, declared as a
	# colon-separated HARNESS_CHOWN_PATHS in its own Dockerfile (herd uses it for
	# the baked Playwright browsers, so an in-session `playwright install` can heal
	# a version drift — BEH-405). These are baked owned by the image's node uid
	# 1000 and are only unwritable when the target uid differs, so the guard keeps
	# the macOS/uid-1000 path from paying a recursive walk of a ~GB tree.
	if [ "$target_uid" != "1000" ] && [ -n "${HARNESS_CHOWN_PATHS:-}" ]; then
		# IFS split, not an array: this must run under macOS's stock bash 3.2.
		saved_ifs="$IFS"
		IFS=':'
		for extra in $HARNESS_CHOWN_PATHS; do
			IFS="$saved_ifs"
			[ -n "$extra" ] && [ -d "$extra" ] && chown -R "$target_uid:$target_gid" "$extra"
			IFS=':'
		done
		IFS="$saved_ifs"
	fi

	exec gosu "$target_uid:$target_gid" "$0" "$@"
fi

# --- below here runs as the unprivileged target user ---
export HOME=/home/node

git config --global user.name "Agent Harness"
git config --global user.email "agent-harness@users.noreply.github.com"

# The repo is bind-mounted from the host; without this git refuses to operate
# ("detected dubious ownership") when the worktree's owner doesn't match.
git config --global --add safe.directory '*'

exec "$@"
