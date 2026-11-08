#!/bin/bash
# Unit tests for check-pipefail-grep.sh (#273).
#
# Under pipefail, `producer | grep -q` reads as a miss whenever grep matches
# early, quits, and the producer takes SIGPIPE writing the rest: #177 in a
# guard, #273 in the tests' expect_fail. The guard flags the pipe in any script
# that sets pipefail. Fixture trees stand in for the repo. Like the other
# harness guards, this must run under macOS's stock bash 3.2.

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
trap "rm -rf $TEST_DIR" EXIT

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/check-pipefail-grep.sh"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# script DIR PATH LINE... — write a pipefail bash script at DIR/PATH whose
# body (from line 3) is the given lines.
script() {
    local dir=$1 path=$2
    shift 2
    mkdir -p "$(dirname "$dir/$path")"
    { printf '#!/bin/bash\nset -euo pipefail\n'; printf '%s\n' "$@"; } > "$dir/$path"
}

# expect_fail NAME DIR PATTERN — the guard must exit non-zero AND report PATTERN,
# so a missing or broken guard can't masquerade as a passing RED.
expect_fail() {
    local report
    report=$("$SCRIPT_PATH" "$2" 2>&1 || true)
    if "$SCRIPT_PATH" "$2" > /dev/null 2>&1; then
        echo -e "${RED}FAIL${NC} - $1: expected failure but got success"
        exit 1
    elif grep -qE -- "$3" <<< "$report"; then
        echo -e "${GREEN}PASS${NC}"
    else
        echo -e "${RED}FAIL${NC} - $1: expected a report matching '$3', got: $report"
        exit 1
    fi
}

expect_pass() {
    local report
    if report=$("$SCRIPT_PATH" "$2" 2>&1); then
        echo -e "${GREEN}PASS${NC}"
    else
        echo -e "${RED}FAIL${NC} - $1: expected success, got: $report"
        exit 1
    fi
}

echo "Testing check-pipefail-grep.sh..."
echo

echo -n "Test 1: expect_fail's echo-into-grep -q is flagged (#273)... "
script "$TEST_DIR/t1" scripts/test-a.sh 'x=$(true)' 'if echo "$report" | grep -qE -- "$3"; then :; fi'
expect_fail "echo pipe" "$TEST_DIR/t1" 'scripts/test-a\.sh:4:.*grep -q'

echo -n "Test 2: the remedy is named in the report... "
expect_fail "remedy" "$TEST_DIR/t1" 'here-string'

echo -n "Test 3: any producer and any spelling of quiet is flagged... "
script "$TEST_DIR/t3" scripts/b.sh \
    'git diff --name-only | grep -qx README.md' \
    'show x | grep -E -q y' \
    'printf "%s" "$v" |grep --quiet z' \
    'cat f | grep -iq w || true'
expect_fail "git diff" "$TEST_DIR/t3" 'scripts/b\.sh:3:'
echo -n "      ...grep -E -q... "
expect_fail "split flags" "$TEST_DIR/t3" 'scripts/b\.sh:4:'
echo -n "      ...--quiet... "
expect_fail "--quiet" "$TEST_DIR/t3" 'scripts/b\.sh:5:'
echo -n "      ...-iq... "
expect_fail "-iq" "$TEST_DIR/t3" 'scripts/b\.sh:6:'

echo -n "Test 4: here-strings, non-quiet greps, || and comments pass... "
script "$TEST_DIR/t4" scripts/c.sh \
    'grep -qE x <<< "$report"' \
    'n=$(echo "$r" | grep -c x || true)' \
    'echo "$r" | grep -E x > /dev/null' \
    'false || grep -q x file' \
    '# not `printf | grep -q`: SIGPIPE under pipefail' \
    '    # echo "$r" | grep -q x' \
    'echo "$r" | grep x && [ "$n" -eq 1 ]'
expect_pass "safe forms" "$TEST_DIR/t4"

echo -n "Test 5: a script that does not set pipefail is not flagged... "
mkdir -p "$TEST_DIR/t5/scripts"
printf '#!/bin/bash\nset -eu\necho "$r" | grep -q x\n' > "$TEST_DIR/t5/scripts/d.sh"
expect_pass "no pipefail" "$TEST_DIR/t5"

echo -n "Test 6: scripts/lib and root scripts are scanned too... "
script "$TEST_DIR/t6" scripts/lib/e.sh 'echo "$r" | grep -q x'
script "$TEST_DIR/t6" entrypoint.sh 'echo "$r" | grep -q x'
expect_fail "lib" "$TEST_DIR/t6" 'scripts/lib/e\.sh:3:'
echo -n "      ...root... "
expect_fail "root" "$TEST_DIR/t6" 'entrypoint\.sh:3:'

echo -n "Test 7: set -o pipefail on its own line counts... "
mkdir -p "$TEST_DIR/t7/scripts"
printf '#!/bin/bash\nset -e\nset -o pipefail\necho "$r" | grep -q x\n' > "$TEST_DIR/t7/scripts/f.sh"
expect_fail "set -o" "$TEST_DIR/t7" 'scripts/f\.sh:4:'

echo -n "Test 8: the real harness tree passes... "
expect_pass "real tree" "$REPO_DIR"

echo
echo -e "${GREEN}All tests passed!${NC}"
