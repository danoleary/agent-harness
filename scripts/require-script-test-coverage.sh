#!/usr/bin/env bash
# Fail when a behavioral change to an agent-harness script ships without touching
# its sibling test (BEH-591).
#
# The run-script-tests gate (this dir's run-script-tests.sh) only RUNS the
# test-*.sh files that exist — so a change to agent-harness/scripts/foo.sh that
# adds no covering case sails through green. This gate closes that loop by
# inspecting the DIFF: if a changed agent-harness/scripts/<name>.sh already has a
# sibling agent-harness/scripts/test-<name>.sh, that test file MUST also appear
# in the diff. It is the agent-harness counterpart of the repo-root BEH-463 gate,
# differing only in the PREFIX-form sibling (test-<name>.sh) and the
# agent-harness/scripts/ location. Scripts with no sibling test yet are out of
# scope — this guards regressions in already-tested scripts, it does not force
# tests onto untested ones. Must stay bash-3.2-safe (macOS stock).
#
# Usage:
#   require-script-test-coverage.sh [BASE_REF]   # BASE_REF defaults to origin/main
set -euo pipefail

base_ref="${1:-origin/main}"

root="$(git rev-parse --show-toplevel)"
cd "$root"

# Compare the branch tip against its merge-base with the base ref, so only the
# commits this branch introduces are considered (mirrors the repo-root gate).
base="$(git merge-base "$base_ref" HEAD 2>/dev/null || echo "$base_ref")"
changed="$(git diff --name-only "$base" HEAD)"

# Production harness scripts that changed: agent-harness/scripts/*.sh, excluding
# the prefix-form test files (basename starting with test-). Build the list with
# a while-read loop rather than `mapfile` (bash 4+; macOS ships 3.2 — BEH-457).
prod=()
while IFS= read -r f; do
	[ -n "$f" ] || continue
	prod+=("$f")
done < <(printf '%s\n' "$changed" | grep -E '^agent-harness/scripts/[^/]+\.sh$' | grep -vE '/test-[^/]+\.sh$' || true)

violations=()
for f in "${prod[@]+"${prod[@]}"}"; do
	[ -n "$f" ] || continue
	# A deleted script shows up in the diff but has no new behavior to cover —
	# skip anything that no longer exists at the tip.
	[ -f "$f" ] || continue
	# Sibling lives at the PREFIX-form path test-<name>.sh in the same dir.
	dir="$(dirname "$f")"
	test_file="${dir}/test-$(basename "$f")"
	if [ -f "$test_file" ]; then
		# Sibling test exists at the tip — it must also appear in the diff.
		if ! printf '%s\n' "$changed" | grep -qxF "$test_file"; then
			violations+=("$f changed, but $test_file was not — add/extend a failing case for the new behavior.")
		fi
	elif git cat-file -e "$base:$test_file" 2>/dev/null; then
		# Sibling test existed at the base but is gone at the tip: coverage was
		# deleted while the script is still being changed — the same gap as an
		# untouched test, just reached by removing the test instead of ignoring it.
		violations+=("$f changed, but its sibling test $test_file was deleted — keep (and extend) the test that covers it.")
	fi
	# else: the script never had a sibling test — out of scope. This gate guards
	# regressions in already-tested scripts; it does not force tests onto
	# untested ones.
done

if [ "${#violations[@]}" -gt 0 ]; then
	printf 'ERROR: agent-harness script changed without touching its sibling test (BEH-591):\n'
	for v in "${violations[@]}"; do
		printf '  - %s\n' "$v"
	done
	exit 1
fi
