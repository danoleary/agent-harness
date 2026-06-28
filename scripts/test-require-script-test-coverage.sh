#!/usr/bin/env bash
# Tests for require-script-test-coverage.sh — the diff-coverage gate that fails a
# push when a behavioral change to an agent-harness script
# (agent-harness/scripts/<name>.sh) ships WITHOUT touching its sibling
# agent-harness/scripts/test-<name>.sh (BEH-591).
#
# This is the agent-harness counterpart to the repo-root gate (BEH-463): same
# idea, but the sibling lives at the PREFIX-form path test-<name>.sh, and the
# scripts live under agent-harness/scripts/ rather than scripts/.
#
# Each case builds a throwaway git repo, commits a base, then a branch tip, and
# runs the gate with the base ref as its argument — mirroring how CI calls it
# with FETCH_HEAD. Must run under macOS's stock bash 3.2.
#
# Harness-agnostic: plain git + shell, no Go, no app server. Run directly:
#   agent-harness/scripts/test-require-script-test-coverage.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="${here}/require-script-test-coverage.sh"

pass=0
fail=0
ok()   { printf 'ok   - %s\n' "$1"; pass=$((pass + 1)); }
ng()   { printf 'FAIL - %s\n' "$1"; fail=$((fail + 1)); }

assert_contains() { # haystack needle label
	if printf '%s' "$1" | grep -qF -- "$2"; then ok "$3"; else
		ng "$3 (missing: '$2')"; printf '  got: %s\n' "$1"
	fi
}

# Build a fresh git repo with a base commit on `main`, returning its path. The
# base holds agent-harness/scripts/sample.sh + test-sample.sh so the sibling
# relationship exists to be (un)covered by the branch tip.
new_repo() {
	local repo="$1"
	git -c init.defaultBranch=main init -q "$repo"
	git -C "$repo" config user.email test@example.com
	git -C "$repo" config user.name "Test"
	git -C "$repo" config commit.gpgsign false
	mkdir -p "${repo}/agent-harness/scripts"
	printf '#!/usr/bin/env bash\necho base\n' >"${repo}/agent-harness/scripts/sample.sh"
	printf '#!/usr/bin/env bash\nexit 0\n'     >"${repo}/agent-harness/scripts/test-sample.sh"
	git -C "$repo" add agent-harness
	git -C "$repo" commit -qm "base"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# ---- tracer: prod script changed, sibling test untouched -> gate fails -------
# Branch off main first so merge-base(main, HEAD) is the base commit, exactly as
# a real feature branch diverges from origin/main.
repo="${tmp}/uncovered"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/agent-harness/scripts/sample.sh"
git -C "$repo" commit -qam "change sample.sh only"

if out="$(cd "$repo" && "$script" main 2>&1)"; then
	ng "fails when a changed prod script's sibling test is untouched"
	printf '  got: %s\n' "$out"
else
	ok "fails when a changed prod script's sibling test is untouched"
fi
assert_contains "$out" "sample.sh" "names the uncovered script"

# ---- covered: prod script AND its sibling test both changed -> gate passes ---
repo="${tmp}/covered"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/agent-harness/scripts/sample.sh"
printf '#!/usr/bin/env bash\nexit 1\n'       >"${repo}/agent-harness/scripts/test-sample.sh"
git -C "$repo" commit -qam "change sample.sh + its test"

if (cd "$repo" && "$script" main >/dev/null 2>&1); then
	ok "passes when the sibling test is changed alongside the script"
else
	ng "passes when the sibling test is changed alongside the script"
fi

# ---- no sibling test exists -> out of scope, gate passes ---------------------
repo="${tmp}/no-sibling"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho hi\n' >"${repo}/agent-harness/scripts/fresh.sh"
git -C "$repo" add agent-harness/scripts/fresh.sh
git -C "$repo" commit -qm "add a script with no sibling test"

if (cd "$repo" && "$script" main >/dev/null 2>&1); then
	ok "passes when the changed script has no sibling test"
else
	ng "passes when the changed script has no sibling test"
fi

# ---- the test file itself changed -> not treated as an uncovered prod script -
# test-<name>.sh must be recognised as a TEST, never as a production script
# missing its own test (which would be the prefix form's foot-gun).
repo="${tmp}/test-only"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\nexit 1\n' >"${repo}/agent-harness/scripts/test-sample.sh"
git -C "$repo" commit -qam "change only the test"

if (cd "$repo" && "$script" main >/dev/null 2>&1); then
	ok "passes when only the test file changed"
else
	ng "passes when only the test file changed"
fi

# ---- no script changes at all -> gate passes (clean no-op) -------------------
repo="${tmp}/unrelated"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
printf 'docs\n' >"${repo}/agent-harness/README.md"
git -C "$repo" add agent-harness/README.md
git -C "$repo" commit -qm "unrelated change"

if (cd "$repo" && "$script" main >/dev/null 2>&1); then
	ok "passes when no harness script changed"
else
	ng "passes when no harness script changed"
fi

# ---- prod script deleted -> gate passes (no behavior to cover) --------------
repo="${tmp}/deleted"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
git -C "$repo" rm -q agent-harness/scripts/sample.sh
git -C "$repo" commit -qm "delete sample.sh"

if (cd "$repo" && "$script" main >/dev/null 2>&1); then
	ok "passes when the script was deleted (no behavior to cover)"
else
	ng "passes when the script was deleted (no behavior to cover)"
fi

# ---- prod script changed, its sibling test DELETED -> gate fails ------------
# Removing the test while still changing the script is the same coverage gap as
# leaving it untouched, just reached by deletion.
repo="${tmp}/deleted-test"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/agent-harness/scripts/sample.sh"
git -C "$repo" rm -q agent-harness/scripts/test-sample.sh
git -C "$repo" commit -qam "change sample.sh and delete its test"

if out="$(cd "$repo" && "$script" main 2>&1)"; then
	ng "fails when the sibling test is deleted alongside a prod change"
	printf '  got: %s\n' "$out"
else
	ok "fails when the sibling test is deleted alongside a prod change"
fi
assert_contains "$out" "sample.sh" "names the script whose test was deleted"

# ---- repo-root scripts/ changes are out of scope for THIS gate --------------
# This gate guards agent-harness/scripts/ only; a change under the repo-root
# scripts/ dir (covered by its own BEH-463 gate) must not be flagged here.
repo="${tmp}/repo-root"
new_repo "$repo"
mkdir -p "${repo}/scripts"
printf '#!/usr/bin/env bash\necho base\n' >"${repo}/scripts/other.sh"
printf '#!/usr/bin/env bash\nexit 0\n'     >"${repo}/scripts/other.test.sh"
git -C "$repo" add scripts
git -C "$repo" commit -qm "add a repo-root tested script"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/scripts/other.sh"
git -C "$repo" commit -qam "change a repo-root script only"

if (cd "$repo" && "$script" main >/dev/null 2>&1); then
	ok "ignores repo-root scripts/ changes (other gate's job)"
else
	ng "ignores repo-root scripts/ changes (other gate's job)"
fi

# ---- several scripts change, only one is covered -> gate fails on the gap ----
repo="${tmp}/multi"
new_repo "$repo"
printf '#!/usr/bin/env bash\necho base2\n' >"${repo}/agent-harness/scripts/second.sh"
printf '#!/usr/bin/env bash\nexit 0\n'     >"${repo}/agent-harness/scripts/test-second.sh"
git -C "$repo" add agent-harness/scripts/second.sh agent-harness/scripts/test-second.sh
git -C "$repo" commit -qm "add a second already-tested script"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n'  >"${repo}/agent-harness/scripts/sample.sh"
printf '#!/usr/bin/env bash\nexit 1\n'        >"${repo}/agent-harness/scripts/test-sample.sh"
printf '#!/usr/bin/env bash\necho changed2\n' >"${repo}/agent-harness/scripts/second.sh"
git -C "$repo" commit -qam "change both scripts, cover only sample"

if out="$(cd "$repo" && "$script" main 2>&1)"; then
	ng "fails when one of several changed scripts is uncovered"
	printf '  got: %s\n' "$out"
else
	ok "fails when one of several changed scripts is uncovered"
fi
assert_contains "$out" "second.sh" "names the uncovered script among several"
if printf '%s' "$out" | grep -qF "sample.sh changed, but"; then
	ng "does not flag the covered script as a violation"
	printf '  got: %s\n' "$out"
else
	ok "does not flag the covered script as a violation"
fi

# ---- summary -----------------------------------------------------------------
printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
