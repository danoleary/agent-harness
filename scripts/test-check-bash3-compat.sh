#!/bin/bash
# Unit tests for check-bash3-compat.sh (BEH-457).
#
# Creates fixture scripts with known bash-4-only constructs and verifies the
# guard flags them, while bash-3.2-safe scripts pass. This test file (and the
# guard) must themselves run under macOS's stock bash 3.2.

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

SCRIPT_PATH="$(dirname "$0")/check-bash3-compat.sh"

echo "Testing check-bash3-compat.sh..."
echo

# Test 1: bash-3.2-safe script (no violations)
echo -n "Test 1: bash-3.2-safe script passes... "
mkdir -p "$TEST_DIR/test1"
cat > "$TEST_DIR/test1/safe.sh" << 'EOF'
#!/bin/bash
set -euo pipefail
arr=(one two three)
for item in "${arr[@]}"; do
    echo "${item##*/}"
done
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test1" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected success but got failure"
    exit 1
fi

# Test 2: declare -A (associative array) is flagged — the original BEH-409 bug
echo -n "Test 2: declare -A flagged... "
mkdir -p "$TEST_DIR/test2"
cat > "$TEST_DIR/test2/assoc.sh" << 'EOF'
#!/bin/bash
declare -A seen
seen[foo]=1
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test2" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 3: a comment mentioning a construct is not flagged (false-positive guard)
echo -n "Test 3: construct named in a comment is allowed... "
mkdir -p "$TEST_DIR/test3"
cat > "$TEST_DIR/test3/commented.sh" << 'EOF'
#!/bin/bash
# Plain indexed array (not `declare -A`) so this runs under bash 3.2.
arr=(a b c)
echo "${arr[@]}"
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test3" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - comment mentioning a construct should be allowed"
    exit 1
fi

# Test 4: nameref (declare -n) is flagged — bash 4.3+
echo -n "Test 4: declare -n (nameref) flagged... "
mkdir -p "$TEST_DIR/test4"
cat > "$TEST_DIR/test4/nameref.sh" << 'EOF'
#!/bin/bash
ref_to() {
    local -n target=$1
    target=hello
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test4" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 5: case modification expansion is flagged — bash 4.0+
echo -n "Test 5: \${var,,}/\${var^^} flagged... "
mkdir -p "$TEST_DIR/test5"
cat > "$TEST_DIR/test5/case.sh" << 'EOF'
#!/bin/bash
name="Foo"
echo "${name,,}"
echo "${name^^}"
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test5" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 5b: bash-3.2-safe parameter expansions are NOT mistaken for case-mod
echo -n "Test 5b: safe \${var#...}/\${var%...} allowed... "
mkdir -p "$TEST_DIR/test5b"
cat > "$TEST_DIR/test5b/expand.sh" << 'EOF'
#!/bin/bash
path="/a/b/c.txt"
echo "${path##*/}"
echo "${path%.*}"
echo "${path:-default}"
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test5b" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - safe parameter expansions should be allowed"
    exit 1
fi

# Test 6: mapfile / readarray builtins are flagged — bash 4.0+
echo -n "Test 6: mapfile/readarray flagged... "
mkdir -p "$TEST_DIR/test6"
cat > "$TEST_DIR/test6/mapfile.sh" << 'EOF'
#!/bin/bash
mapfile -t lines < input.txt
echo "${lines[0]}"
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test6" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 7: files without a bash shebang are not scanned (out of scope)
echo -n "Test 7: non-bash files ignored... "
mkdir -p "$TEST_DIR/test7"
cat > "$TEST_DIR/test7/notes.txt" << 'EOF'
declare -A this is prose, not a script
EOF
cat > "$TEST_DIR/test7/posix.sh" << 'EOF'
#!/bin/sh
echo "posix sh, not scanned for bashisms"
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test7" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - non-bash files should be ignored"
    exit 1
fi

# Test 7b: a bash shebang WITH flags is still scanned (not silently skipped)
echo -n "Test 7b: #!/bin/bash -eu shebang is scanned... "
mkdir -p "$TEST_DIR/test7b"
cat > "$TEST_DIR/test7b/flagged-shebang.sh" << 'EOF'
#!/bin/bash -eu
declare -A seen
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test7b" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - a #!/bin/bash -eu script must still be scanned"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 8: violation report names file and line (actionable output)
echo -n "Test 8: report includes file:line... "
mkdir -p "$TEST_DIR/test8"
cat > "$TEST_DIR/test8/bug.sh" << 'EOF'
#!/bin/bash
echo "line two"
declare -A m
EOF

report=$("$SCRIPT_PATH" "$TEST_DIR/test8" 2>&1 || true)
if echo "$report" | grep -qE 'bug\.sh:3:'; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected bug.sh:3 in report, got: $report"
    exit 1
fi

# Test 9: the real harness scripts pass (dogfood — they must be bash 3.2 safe)
echo -n "Test 9: real harness scripts pass... "
HARNESS_DIR="$(dirname "$(dirname "$0")")"
if "$SCRIPT_PATH" "$HARNESS_DIR" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - real harness scripts should be bash 3.2 safe"
    exit 1
fi

echo
echo -e "${GREEN}All tests passed!${NC}"
