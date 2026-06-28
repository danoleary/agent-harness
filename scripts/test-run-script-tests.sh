#!/usr/bin/env bash
# Tests for run-script-tests.sh — the gate that discovers and runs every
# agent-harness/scripts/test-*.sh (BEH-591).
#
# The agent-harness scripts dir uses the PREFIX form (test-<name>.sh), unlike
# the repo-root suite's *.test.sh suffix — so this runner globs test-*.sh. The
# runner takes an optional DIR argument (default: its own dir). These tests point
# it at throwaway fixture dirs holding fake pass/fail test-*.sh files, so
# aggregation and exit codes are asserted without touching the real harness tests
# (and without recursion). Must run under macOS's stock bash 3.2.
#
# Harness-agnostic: plain bash + shell fixtures, no Go, no app server. Run directly:
#   agent-harness/scripts/test-run-script-tests.sh
set -euo pipefail

here="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
runner="${here}/run-script-tests.sh"

pass=0
fail=0
ok()   { printf 'ok   - %s\n' "$1"; pass=$((pass + 1)); }
ng()   { printf 'FAIL - %s\n' "$1"; fail=$((fail + 1)); }

assert_contains() { # haystack needle label
	if printf '%s' "$1" | grep -qF -- "$2"; then ok "$3"; else
		ng "$3 (missing: '$2')"; printf '  got: %s\n' "$1"
	fi
}
assert_not_contains() { # haystack needle label
	if printf '%s' "$1" | grep -qF -- "$2"; then
		ng "$3 (unexpected: '$2')"; printf '  got: %s\n' "$1"
	else ok "$3"; fi
}

# Write an executable fake test file that exits with the given code.
make_test() { # dir name exit_code
	local f="$1/$2"
	cat >"$f" <<EOF
#!/usr/bin/env bash
exit $3
EOF
	chmod +x "$f"
}

tmp="$(mktemp -d)"
trap 'rm -rf "$tmp"' EXIT

# ---- a directory whose test files all pass -> runner succeeds ----------------
green="${tmp}/green"
mkdir -p "$green"
make_test "$green" "test-alpha.sh" 0

if out="$("$runner" "$green" 2>&1)"; then
	ok "exits 0 when every test file passes"
else
	ng "exits 0 when every test file passes (exit $?)"; printf '  got: %s\n' "$out"
fi
assert_contains "$out" "test-alpha.sh" "names the test file it ran"

# ---- a directory with a failing test file -> runner fails, names it ----------
red="${tmp}/red"
mkdir -p "$red"
make_test "$red" "test-broken.sh" 1

if out="$("$runner" "$red" 2>&1)"; then
	ng "exits non-zero when a test file fails"; printf '  got: %s\n' "$out"
else
	ok "exits non-zero when a test file fails"
fi
assert_contains "$out" "test-broken.sh" "names the failing test file"

# ---- a mix: a failure must not stop later files; summary lists failures ------
mixed="${tmp}/mixed"
mkdir -p "$mixed"
make_test "$mixed" "test-a-fails.sh"  1
make_test "$mixed" "test-b-passes.sh" 0

if out="$("$runner" "$mixed" 2>&1)"; then
	ng "exits non-zero when any test file fails"; printf '  got: %s\n' "$out"
else
	ok "exits non-zero when any test file fails"
fi
assert_contains "$out" "test-b-passes.sh" "keeps running later files after a failure"
assert_contains "$out" "1 failed"         "summary reports the failed count"
# The failing file is surfaced in a trailing failure list, not just inline.
failblock="${out#*Failed:}"
assert_contains "$failblock" "test-a-fails.sh"      "lists the failing file under Failed:"
assert_not_contains "$failblock" "test-b-passes.sh" "does not list a passing file as failed"

# ---- a *.test.sh (suffix form) file is NOT matched by the prefix glob --------
# The agent-harness convention is the test-<name>.sh prefix; a stray suffix-form
# file must not be picked up by this runner (it belongs to the repo-root suite).
suffix="${tmp}/suffix"
mkdir -p "$suffix"
make_test "$suffix" "wrongform.test.sh" 1
if out="$("$runner" "$suffix" 2>&1)"; then
	ok "ignores suffix-form *.test.sh files"
else
	ng "ignores suffix-form *.test.sh files (exit $?)"; printf '  got: %s\n' "$out"
fi
assert_contains "$out" "nothing to run" "treats a dir with only suffix-form files as empty"

# ---- an empty directory -> exit 0 but say so, so a bad path is visible -------
empty="${tmp}/empty"
mkdir -p "$empty"
if out="$("$runner" "$empty" 2>&1)"; then
	ok "exits 0 when no test files are found"
else
	ng "exits 0 when no test files are found (exit $?)"; printf '  got: %s\n' "$out"
fi
assert_contains "$out" "nothing to run" "prints a notice when no test files match"

# ---- summary -----------------------------------------------------------------
printf '\n%d passed, %d failed\n' "$pass" "$fail"
[ "$fail" -eq 0 ]
