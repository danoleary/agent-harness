#!/bin/bash
# Unit tests for check-awk-portability.sh (#129).
#
# The guard fails when an awk program in scripts/*.sh or install.sh uses a regex
# interval expression ({n}, {n,}, {n,m}) or a gawk-only function. The sandbox's
# awk is mawk 1.3.4, which raises no error on an interval: the regex just never
# matches, so a guard script passes vacuously. Fixture trees stand in for the
# repo. Like the other harness guards, this must run under macOS's stock bash 3.2.

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

SCRIPT_PATH="$(dirname "$0")/check-awk-portability.sh"

# fixture DIR — a repo whose scripts use awk portably.
fixture() {
    mkdir -p "$1/scripts"
    cat > "$1/scripts/clean.sh" <<'EOF'
#!/bin/bash
sha256sum "$1" | awk '{print $1}'
printf '%s\n' "$1" | awk -F/ '
    {
        n = NF / 2
        if ($0 ~ /\/$/ && n > 0) print "dir"
        if (match($0, /^ ? ? ?\[[^]]+\]:/)) print substr($0, RSTART, RLENGTH)
    }'
grep -qE '^ {0,3}\[' README.md
for i in {1..3}; do echo "${#i}"; done
EOF
    cat > "$1/install.sh" <<'EOF'
#!/bin/sh
awk -v n="$name" '{ f = $2; sub(/^\*/, "", f); if (f == n) { print $1; exit } }' sums
EOF
}

# expect_fail NAME DIR PATTERN — the guard must exit non-zero AND report PATTERN,
# so a missing or broken guard can't masquerade as a passing RED.
expect_fail() {
    local report
    report=$("$SCRIPT_PATH" "$2" 2>&1 || true)
    if "$SCRIPT_PATH" "$2" > /dev/null 2>&1; then
        echo -e "${RED}FAIL${NC} - $1: expected failure but got success"
        exit 1
    elif echo "$report" | grep -qE -- "$3"; then
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

echo "Testing check-awk-portability.sh..."
echo

echo -n "Test 1: portable awk, and intervals outside awk, pass... "
fixture "$TEST_DIR/t1"
expect_pass "clean tree" "$TEST_DIR/t1"

# The #96 bug: mawk never matches ^ {0,3}\[, so the guard checked nothing.
echo -n "Test 2: an interval in an awk regex literal is flagged... "
fixture "$TEST_DIR/t2"
printf '#!/bin/bash\nawk %s/^ {0,3}\\[/ { print }%s README.md\n' "'" "'" > "$TEST_DIR/t2/scripts/links.sh"
expect_fail "interval literal" "$TEST_DIR/t2" 'scripts/links\.sh:2:.*\{0,3\}'
echo -n "Test 2b: the report points at the portable spelling... "
expect_fail "interval message" "$TEST_DIR/t2" 'repeat the atom.*\?'

echo -n "Test 3: an interval deep in a multi-line awk program names its own line... "
fixture "$TEST_DIR/t3"
cat > "$TEST_DIR/t3/scripts/multi.sh" <<'EOF'
#!/bin/bash
awk '
    /^#/ { next }
    {
        if (match($0, /x[0-9]{2}y/)) print
    }
' file
EOF
expect_fail "multi-line program" "$TEST_DIR/t3" 'scripts/multi\.sh:5:.*\{2\}'

echo -n "Test 4: {n,} after -F and -v options is flagged... "
fixture "$TEST_DIR/t4"
cat > "$TEST_DIR/t4/scripts/opts.sh" <<'EOF'
#!/bin/bash
x=$(awk -F '\t' -v n="$name" '$1 == n && $2 ~ /^a{2,}$/ { print }' file)
EOF
expect_fail "after options" "$TEST_DIR/t4" 'scripts/opts\.sh:2:.*\{2,\}'

echo -n "Test 5: install.sh is scanned too... "
fixture "$TEST_DIR/t5"
printf '#!/bin/sh\nv="$(git --version | awk %s/[0-9]{1,3}/ { print $3 }%s)"\n' "'" "'" > "$TEST_DIR/t5/install.sh"
expect_fail "install.sh" "$TEST_DIR/t5" 'install\.sh:2:.*\{1,3\}'

echo -n "Test 6: a string used as a dynamic regex is flagged... "
fixture "$TEST_DIR/t6"
printf '#!/bin/bash\nawk %s$0 ~ "^ab{3}" { print }%s file\n' "'" "'" > "$TEST_DIR/t6/scripts/dyn.sh"
expect_fail "dynamic regex" "$TEST_DIR/t6" 'scripts/dyn\.sh:2:.*\{3\}'

# A brace that is escaped or inside a bracket expression is a literal, and a
# shell comment that quotes a bad regex is prose, not code.
echo -n "Test 7: literal braces and commented-out awk are not flagged... "
fixture "$TEST_DIR/t7"
cat > "$TEST_DIR/t7/scripts/literal.sh" <<'EOF'
#!/bin/bash
# Never write awk '/^ {0,3}\[/' here: mawk ignores it.
awk '/\{2\}/ || /[{1]/ { print "{1}" } # a{2} in an awk comment' file
EOF
expect_pass "literal braces" "$TEST_DIR/t7"

echo -n "Test 8: a gawk-only function is flagged... "
fixture "$TEST_DIR/t8"
printf '#!/bin/bash\nawk %s{ print gensub(/a/, "b", "g") }%s file\n' "'" "'" > "$TEST_DIR/t8/scripts/gawk.sh"
expect_fail "gensub" "$TEST_DIR/t8" 'scripts/gawk\.sh:2:.*gensub'

echo -n "Test 9: gawk function names inside strings or longer names are not flagged... "
fixture "$TEST_DIR/t9"
printf '#!/bin/bash\nawk %s{ asort_n = 1; print "strftime(x)", asort_n }%s file\n' "'" "'" > "$TEST_DIR/t9/scripts/names.sh"
expect_pass "names" "$TEST_DIR/t9"

echo -n "Test 10: the real harness tree passes... "
HARNESS_DIR="$(dirname "$(dirname "$0")")"
expect_pass "real tree" "$HARNESS_DIR"

echo
echo -e "${GREEN}All tests passed!${NC}"
