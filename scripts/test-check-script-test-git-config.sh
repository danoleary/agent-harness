#!/bin/bash
# Unit tests for check-script-test-git-config.sh (#166).
#
# Builds fixture script-test dirs and checks the guard flags a test-*.sh that
# runs `git init` or `git clone` without isolating git from the operator's
# global config, and passes one that sources scripts/lib/scratch-git.sh or sets
# GIT_CONFIG_GLOBAL itself. A last case builds a repo through the helper under a
# global config that signs commits, the #166 incident, and checks the commit
# still lands unsigned. Must run under macOS's stock bash 3.2.

set -euo pipefail

if [ -t 1 ]; then
    GREEN='\033[0;32m'
    RED='\033[0;31m'
    NC='\033[0m'
else
    GREEN=''
    RED=''
    NC=''
fi

TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TEST_DIR"' EXIT

HERE="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$HERE/check-script-test-git-config.sh"

pass() { echo -e "${GREEN}PASS${NC}"; }
fail() { echo -e "${RED}FAIL${NC} - $*"; exit 1; }

# expect_fail LABEL DIR PATTERN — the guard must exit non-zero on DIR and its
# report must match PATTERN, so a missing guard can't pass as a RED.
expect_fail() {
    local report
    if "$SCRIPT_PATH" "$2" > /dev/null 2>&1; then
        fail "$1: expected failure but got success"
    fi
    report=$("$SCRIPT_PATH" "$2" 2>&1 || true)
    if ! echo "$report" | grep -qE "$3"; then
        fail "$1: report doesn't match '$3'; got: $report"
    fi
    pass
}

expect_pass() {
    local report
    if ! report=$("$SCRIPT_PATH" "$2" 2>&1); then
        fail "$1: expected success; got: $report"
    fi
    pass
}

# fixture NAME LINE... — DIR/NAME/test-x.sh holding the given lines.
fixture() {
    local d="$TEST_DIR/$1"
    shift
    mkdir -p "$d"
    printf '%s\n' '#!/bin/bash' "$@" > "$d/test-x.sh"
    echo "$d"
}

echo "Testing check-script-test-git-config.sh..."
echo

echo -n "Test 1: a bare git init is flagged... "
d=$(fixture t1 'set -e' 'git init -q -b main "$R"' 'git -C "$R" config commit.gpgsign false')
expect_fail "bare git init" "$d" 'test-x\.sh:3:.*git init'

echo -n "Test 2: git -c ... init is flagged... "
d=$(fixture t2 'git -c init.defaultBranch=main init -q "$R"')
expect_fail "git -c init" "$d" 'test-x\.sh:2:.*init'

echo -n "Test 2b: git clone is flagged... "
d=$(fixture t2b 'git clone -q "$O" "$R"')
expect_fail "git clone" "$d" 'test-x\.sh:2:.*clone'

echo -n "Test 3: a test that sources the helper passes... "
d=$(fixture t3 '. "$(dirname "$0")/lib/scratch-git.sh"' 'scratch_git_repo "$R"' 'git clone -q "$R" "$C"')
expect_pass "sources helper" "$d"

echo -n "Test 4: a test that sets GIT_CONFIG_GLOBAL itself passes... "
d=$(fixture t4 'export GIT_CONFIG_GLOBAL=/dev/null GIT_CONFIG_NOSYSTEM=1' 'git init -q "$R"')
expect_pass "own isolation" "$d"

echo -n "Test 5: comments and git subcommands that build no repo aren't flagged... "
d=$(fixture t5 '# git init -q "$R" is what the helper does' 'git -C "$R" log --oneline' 'git --version')
expect_pass "no repo built" "$d"

echo -n "Test 6: only test-*.sh files are scanned... "
mkdir -p "$TEST_DIR/t6"
printf '#!/bin/bash\ngit init -q "$1"\n' > "$TEST_DIR/t6/release.sh"
expect_pass "non-test script" "$TEST_DIR/t6"

echo -n "Test 7: the scripts dir itself is clean... "
expect_pass "real scripts" "$HERE"

# The #166 incident: an operator whose global config signs commits with a key
# the test can't reach. Without isolation, `git commit` in the scratch repo
# fails trying to run the signing program; through the helper it must not.
echo -n "Test 8: a repo built through the helper ignores a signing global config (#166)... "
G="$TEST_DIR/t8-global"
printf '[commit]\n\tgpgsign = true\n[tag]\n\tgpgsign = true\n[gpg]\n\tprogram = false\n' > "$G"
if GIT_CONFIG_GLOBAL="$G" bash -c '
    set -e
    git init -q "$1/control"
    cd "$1/control"
    git -c user.name=t -c user.email=t@example.invalid commit -q --allow-empty -m c
' _ "$TEST_DIR" > /dev/null 2>&1; then
    fail "the control commit should fail under the signing global config"
fi
if ! out=$(GIT_CONFIG_GLOBAL="$G" bash -c '
    set -e
    . "$2/lib/scratch-git.sh"
    scratch_git_repo "$1/t8"
    cd "$1/t8"
    git commit -q --allow-empty -m c
    git tag -a v1 -m v1
    git rev-parse --abbrev-ref HEAD
' _ "$TEST_DIR" "$HERE" 2>&1); then
    fail "commit through the helper failed: $out"
fi
[ "$out" = main ] || fail "expected the scratch repo on main, got: $out"
pass

echo
echo -e "${GREEN}All tests passed${NC}"
