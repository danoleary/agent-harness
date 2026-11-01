#!/bin/bash
# check-script-test-git-config.sh — guard against script tests that build a git
# repo under the operator's global git config (#166).
#
# A scripts/test-*.sh inherits the host's environment. The sandbox has no global
# git config, so a test that runs `git init` and commits passes there, and fails
# or prompts for a key on an operator's machine that sets commit.gpgsign. No
# other gate can see the difference. This guard fails a test-*.sh that runs
# `git init` or `git clone` unless it sources scripts/lib/scratch-git.sh or sets
# GIT_CONFIG_GLOBAL itself. It is a per-file check: one such line anywhere in
# the file clears it. Must stay bash-3.2-safe.
#
# Usage: check-script-test-git-config.sh [DIR]   # DIR defaults to this script's dir
#
# Exit 0 = clean, exit 1 = violations found.

set -euo pipefail

DIR="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"

# `git`, any options (`-C dir`, `-c k=v`), then the init or clone subcommand.
BUILDS_REPO='(^|[^[:alnum:]_./-])git([[:space:]]+-[^[:space:]]+([[:space:]]+[^-[:space:]][^[:space:]]*)?)*[[:space:]]+(init|clone)([[:space:]]|$)'
ISOLATED='scratch-git\.sh|GIT_CONFIG_GLOBAL='

violations=0
shopt -s nullglob
for f in "$DIR"/test-*.sh; do
    hits=$(grep -nE "$BUILDS_REPO" "$f" | grep -vE '^[0-9]+:[[:space:]]*#' || true)
    [ -n "$hits" ] || continue
    if grep -vE '^[[:space:]]*#' "$f" | grep -qE "$ISOLATED"; then
        continue
    fi
    while IFS= read -r hit; do
        printf '%s:%s\n' "$f" "$hit"
    done <<< "$hits"
    violations=$((violations + 1))
done
shopt -u nullglob

if [ "$violations" -gt 0 ]; then
    cat <<'EOF'

These script tests build a git repo under the operator's global git config, so
a host that signs commits makes them fail or prompt for a key (#166). Source the
helper before the first git command and build the repo with it:

    . "$(dirname "$0")/lib/scratch-git.sh"
    scratch_git_repo "$dir"
EOF
    exit 1
fi
echo "No script test builds a git repo under the operator's global git config"
