#!/usr/bin/env bash
# Tests for require-script-test-coverage.sh — the diff-coverage gate that fails a
# push when a behavioral change to a harness script (scripts/<name>.sh) ships
# WITHOUT touching its sibling scripts/test-<name>.sh (BEH-591).
#
# The fixtures live at scripts/, where this repo keeps its scripts: the gate once
# matched a stale agent-harness/scripts/ prefix, so it never fired here and these
# fixtures, built under that same prefix, stayed green with it (#171).
#
# Each case builds a throwaway git repo, commits a base, then a branch tip, and
# runs the gate with the base ref as its argument — mirroring how CI calls it
# with FETCH_HEAD. Must run under macOS's stock bash 3.2.
#
# Harness-agnostic: plain git + shell, no Go, no app server. Run directly:
#   scripts/test-require-script-test-coverage.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
script="${here}/require-script-test-coverage.sh"
. "${here}/lib/scratch-git.sh"

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
# base holds scripts/sample.sh + test-sample.sh so the sibling
# relationship exists to be (un)covered by the branch tip.
new_repo() {
	local repo="$1"
	scratch_git_repo "$repo"
	mkdir -p "${repo}/scripts"
	printf '#!/usr/bin/env bash\necho base\n' >"${repo}/scripts/sample.sh"
	printf '#!/usr/bin/env bash\nexit 0\n'     >"${repo}/scripts/test-sample.sh"
	git -C "$repo" add scripts
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
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/scripts/sample.sh"
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
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/scripts/sample.sh"
printf '#!/usr/bin/env bash\nexit 1\n'       >"${repo}/scripts/test-sample.sh"
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
printf '#!/usr/bin/env bash\necho hi\n' >"${repo}/scripts/fresh.sh"
git -C "$repo" add scripts/fresh.sh
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
printf '#!/usr/bin/env bash\nexit 1\n' >"${repo}/scripts/test-sample.sh"
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
printf 'docs\n' >"${repo}/README.md"
git -C "$repo" add README.md
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
git -C "$repo" rm -q scripts/sample.sh
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
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/scripts/sample.sh"
git -C "$repo" rm -q scripts/test-sample.sh
git -C "$repo" commit -qam "change sample.sh and delete its test"

if out="$(cd "$repo" && "$script" main 2>&1)"; then
	ng "fails when the sibling test is deleted alongside a prod change"
	printf '  got: %s\n' "$out"
else
	ok "fails when the sibling test is deleted alongside a prod change"
fi
assert_contains "$out" "sample.sh" "names the script whose test was deleted"

# ---- #171: scripts/foo.sh changed, test-foo.sh untouched -> gate fails ------
# The ticket's own case, at the repo-root scripts/ path CI diffs in this repo.
# With the stale agent-harness/scripts/ filter no path here ever matched, so the
# gate passed vacuously.
repo="${tmp}/repo-root"
new_repo "$repo"
printf '#!/usr/bin/env bash\necho base\n' >"${repo}/scripts/foo.sh"
printf '#!/usr/bin/env bash\nexit 0\n'     >"${repo}/scripts/test-foo.sh"
git -C "$repo" add scripts
git -C "$repo" commit -qm "add a tested repo-root script"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/scripts/foo.sh"
git -C "$repo" commit -qam "change scripts/foo.sh only"

if out="$(cd "$repo" && "$script" main 2>&1)"; then
	ng "fails when scripts/foo.sh changes without scripts/test-foo.sh"
	printf '  got: %s\n' "$out"
else
	ok "fails when scripts/foo.sh changes without scripts/test-foo.sh"
fi
assert_contains "$out" "scripts/foo.sh changed, but scripts/test-foo.sh" "names scripts/foo.sh and its sibling test"

# ---- a script in a subdirectory of scripts/ is out of scope ------------------
# scripts/lib/*.sh are sourced helpers, not siblings of a test-<name>.sh.
repo="${tmp}/nested"
new_repo "$repo"
mkdir -p "${repo}/scripts/lib"
printf '#!/usr/bin/env bash\necho base\n' >"${repo}/scripts/lib/helper.sh"
printf '#!/usr/bin/env bash\nexit 0\n'     >"${repo}/scripts/lib/test-helper.sh"
git -C "$repo" add scripts
git -C "$repo" commit -qm "add a nested helper"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/scripts/lib/helper.sh"
git -C "$repo" commit -qam "change the nested helper only"

if (cd "$repo" && "$script" main >/dev/null 2>&1); then
	ok "ignores scripts in subdirectories of scripts/"
else
	ng "ignores scripts in subdirectories of scripts/"
fi

# ---- several scripts change, only one is covered -> gate fails on the gap ----
repo="${tmp}/multi"
new_repo "$repo"
printf '#!/usr/bin/env bash\necho base2\n' >"${repo}/scripts/second.sh"
printf '#!/usr/bin/env bash\nexit 0\n'     >"${repo}/scripts/test-second.sh"
git -C "$repo" add scripts/second.sh scripts/test-second.sh
git -C "$repo" commit -qm "add a second already-tested script"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n'  >"${repo}/scripts/sample.sh"
printf '#!/usr/bin/env bash\nexit 1\n'        >"${repo}/scripts/test-sample.sh"
printf '#!/usr/bin/env bash\necho changed2\n' >"${repo}/scripts/second.sh"
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

# ---- #177: a large diff covers the test early -> gate passes ---------------
# The membership test piped the changed list into `grep -q`, which exits on its
# first match; under pipefail the writer's SIGPIPE read as "test untouched". A
# list past the pipe buffer, with the test sorted early, makes that fire.
repo="${tmp}/large"
new_repo "$repo"
git -C "$repo" checkout -q -b feat
printf '#!/usr/bin/env bash\necho changed\n' >"${repo}/scripts/sample.sh"
printf '#!/usr/bin/env bash\nexit 1\n'       >"${repo}/scripts/test-sample.sh"
mkdir -p "${repo}/zz"
pad="$(printf '%0240d' 0 | tr 0 x)"
i=1
while [ "$i" -le 600 ]; do
	printf 'x\n' >"${repo}/zz/${i}-${pad}"
	i=$((i + 1))
done
git -C "$repo" add -A
git -C "$repo" commit -qm "change sample.sh and its test among many files"

if out="$(cd "$repo" && "$script" main 2>&1)"; then
	ok "passes a covered script in a diff past the pipe buffer"
else
	ng "passes a covered script in a diff past the pipe buffer"
	printf '  got: %s\n' "$out"
fi

# ---- summary -----------------------------------------------------------------
printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
