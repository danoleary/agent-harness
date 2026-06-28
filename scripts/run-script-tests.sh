#!/usr/bin/env bash
# Discover and run every agent-harness harness shell test (test-*.sh in this
# dir), aggregate the results, and exit non-zero if any file fails (BEH-591).
#
# The repo-root suite (scripts/*.test.sh) has its own runner; the agent-harness
# scripts dir uses the PREFIX form (test-<name>.sh) instead, and nothing ran it —
# the tests passed by hand but gated nothing. This is that runner. Like the
# repo-root one it makes no assumption beyond "execute the file, check its exit
# code", so a new agent-harness/scripts/test-<name>.sh is picked up by the glob
# with zero extra wiring. Must stay bash-3.2-safe (macOS stock).
#
# Usage:
#   run-script-tests.sh [DIR]   # DIR defaults to this script's dir
set -euo pipefail

# Run hermetically. When invoked from a git hook (lefthook's pre-push gate) git
# exports GIT_DIR / GIT_WORK_TREE / GIT_INDEX_FILE / … into the environment;
# those leak into the tests' throwaway git fixtures and make their git commands
# operate on the REAL repo instead of the /tmp fixture, so a test that passes
# from a bare shell fails under `git push`. Clear the inherited git *location*
# vars (keep GIT_EXEC_PATH so git can still find its own helpers).
while IFS='=' read -r _name _; do
	case "$_name" in
	GIT_EXEC_PATH) ;;
	GIT_*) unset "$_name" ;;
	esac
done < <(env)

dir="${1:-$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)}"

shopt -s nullglob
tests=("$dir"/test-*.sh)
shopt -u nullglob

if [ "${#tests[@]}" -eq 0 ]; then
	printf 'no test files matched %s/test-*.sh — nothing to run\n' "$dir"
	exit 0
fi

ran=0
failed=()
for t in "${tests[@]}"; do
	name="$(basename "$t")"
	printf '→ %s\n' "$name"
	# A failing file must not abort the run — capture its code and carry on so
	# every test file is exercised and all failures are reported together.
	if bash "$t"; then
		printf '✓ %s\n' "$name"
	else
		printf '✗ %s (exit %d)\n' "$name" "$?"
		failed+=("$name")
	fi
	ran=$((ran + 1))
done

printf '\n%d test file(s) run, %d failed\n' "$ran" "${#failed[@]}"
if [ "${#failed[@]}" -gt 0 ]; then
	printf 'Failed:\n'
	printf '  - %s\n' "${failed[@]}"
	exit 1
fi
