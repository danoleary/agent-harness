#!/usr/bin/env bash
# Runs once per container before the claude session: set the commit identity,
# trust the bind-mounted repo, and wire git to push over HTTPS with GH_TOKEN.
set -euo pipefail

git config --global user.name "Herd Agent Harness"
git config --global user.email "agent-harness@beherd.co"

# The repo is bind-mounted from the host and owned by the host uid; without this
# git refuses to operate ("detected dubious ownership").
git config --global --add safe.directory '*'

# Let `git push` (review session) authenticate with the GH_TOKEN PAT.
if [ -n "${GH_TOKEN:-}" ]; then
	gh auth setup-git
fi

exec "$@"
