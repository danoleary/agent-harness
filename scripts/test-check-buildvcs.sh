#!/bin/bash
# Unit tests for check-buildvcs.sh (BEH-459).
#
# Creates fixture Makefiles with VCS-stamping Go invocations and verifies the
# guard flags the ones missing -buildvcs=false while leaving buildvcs-safe
# Makefiles (and non-stamping go subcommands) alone. Like the other harness
# guards, this must run under macOS's stock bash 3.2.

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

SCRIPT_PATH="$(dirname "$0")/check-buildvcs.sh"

echo "Testing check-buildvcs.sh..."
echo

# Test 1: a bare `go build` (no -buildvcs=false) is flagged — the BEH-459 bug.
# Assert on the report content (not just exit status) so a missing/broken guard
# can't masquerade as a passing RED.
echo -n "Test 1: bare 'go build' flagged with actionable report... "
mkdir -p "$TEST_DIR/test1"
printf 'build:\n\tgo build -o bin/foo ./cmd/foo\n' > "$TEST_DIR/test1/Makefile"

report=$("$SCRIPT_PATH" "$TEST_DIR/test1" 2>&1 || true)
if "$SCRIPT_PATH" "$TEST_DIR/test1" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
elif echo "$report" | grep -qiE 'Makefile:2:.*buildvcs'; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected a 'Makefile:2 ... buildvcs' report, got: $report"
    exit 1
fi

# Test 2: a `go build ... -buildvcs=false` recipe passes (the fix is accepted).
echo -n "Test 2: 'go build -buildvcs=false' passes... "
mkdir -p "$TEST_DIR/test2"
printf 'build:\n\tgo build -buildvcs=false -o bin/foo ./cmd/foo\n' > "$TEST_DIR/test2/Makefile"

if "$SCRIPT_PATH" "$TEST_DIR/test2" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - a buildvcs-safe go build should pass"
    exit 1
fi

# Test 3: non-stamping go subcommands (vet/mod/fmt/version) are NOT flagged.
echo -n "Test 3: go vet/mod tidy/version not flagged... "
mkdir -p "$TEST_DIR/test3"
printf 'vet:\n\tgo vet ./...\ntidy:\n\tgo mod tidy\nfmt:\n\tgofmt -w .\nver:\n\tgo version\n' > "$TEST_DIR/test3/Makefile"

if "$SCRIPT_PATH" "$TEST_DIR/test3" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - non-stamping go subcommands should not be flagged"
    exit 1
fi

# Test 4: `docker build` (substring "build") is NOT mistaken for `go build`.
echo -n "Test 4: 'docker build' not flagged... "
mkdir -p "$TEST_DIR/test4"
printf 'image:\n\tdocker build -t foo .\n' > "$TEST_DIR/test4/Makefile"

if "$SCRIPT_PATH" "$TEST_DIR/test4" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - 'docker build' must not be treated as 'go build'"
    exit 1
fi

# Test 5: a comment mentioning `go build` is not flagged (false-positive guard).
echo -n "Test 5: 'go build' in a comment is allowed... "
mkdir -p "$TEST_DIR/test5"
printf '# run go build to compile\nbuild:\n\tgo build -buildvcs=false ./cmd/foo\n' > "$TEST_DIR/test5/Makefile"

if "$SCRIPT_PATH" "$TEST_DIR/test5" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - a comment mentioning go build should be allowed"
    exit 1
fi

# Test 6: `go test` and `go run` are also flagged (they stamp too).
echo -n "Test 6: bare 'go test' and 'go run' flagged... "
mkdir -p "$TEST_DIR/test6"
printf 'test:\n\tgo test ./...\nrun:\n\tgo run ./cmd/foo\n' > "$TEST_DIR/test6/Makefile"

report6=$("$SCRIPT_PATH" "$TEST_DIR/test6" 2>&1 || true)
if echo "$report6" | grep -qE 'Makefile:2:' && echo "$report6" | grep -qE 'Makefile:4:'; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected both go test (line 2) and go run (line 4) flagged, got: $report6"
    exit 1
fi

# Test 7: bare `go install` is also flagged (it stamps too, and both the guard
# pattern and the report name it).
echo -n "Test 7: bare 'go install' flagged... "
mkdir -p "$TEST_DIR/test7"
printf 'install:\n\tgo install ./cmd/foo\n' > "$TEST_DIR/test7/Makefile"

report7=$("$SCRIPT_PATH" "$TEST_DIR/test7" 2>&1 || true)
if "$SCRIPT_PATH" "$TEST_DIR/test7" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
elif echo "$report7" | grep -qE 'Makefile:2:'; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected a 'Makefile:2' report for go install, got: $report7"
    exit 1
fi

# Test 8: the real harness Makefile passes (dogfood — it must be buildvcs-safe).
echo -n "Test 8: real harness Makefile passes... "
HARNESS_DIR="$(dirname "$(dirname "$0")")"
if "$SCRIPT_PATH" "$HARNESS_DIR" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - the real harness Makefile should be buildvcs-safe"
    exit 1
fi

echo
echo -e "${GREEN}All tests passed!${NC}"
