#!/bin/bash
# Unit tests for check-exec-command.sh (BEH-388).
#
# Creates test fixtures with known violations and allowlisted cases,
# verifies the guard correctly detects/ignores them.

set -euo pipefail

# Color codes for output
if [ -t 1 ]; then
    GREEN='\033[0;32m'
    RED='\033[0;31m'
    NC='\033[0m'
else
    GREEN=''
    RED=''
    NC=''
fi

# Create a temp directory for test files
TEST_DIR=$(mktemp -d)
trap "rm -rf $TEST_DIR" EXIT

# Path to the script we're testing
SCRIPT_PATH="$(dirname "$0")/check-exec-command.sh"

echo "Testing check-exec-command.sh..."
echo

# Test 1: No violations (clean codebase)
echo -n "Test 1: Clean code (no violations)... "
mkdir -p "$TEST_DIR/test1"
cat > "$TEST_DIR/test1/main.go" << 'EOF'
package main

import "fmt"

func main() {
    fmt.Println("Hello")
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test1" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected success but got failure"
    exit 1
fi

# Test 2: Docker violation
echo -n "Test 2: Docker violation detection... "
mkdir -p "$TEST_DIR/test2"
cat > "$TEST_DIR/test2/violation.go" << 'EOF'
package main

import "os/exec"

func test() {
    exec.Command("docker", "ps").Run()
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test2" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 3: gh violation
echo -n "Test 3: gh violation detection... "
mkdir -p "$TEST_DIR/test3"
cat > "$TEST_DIR/test3/gh.go" << 'EOF'
package main

import "os/exec"

func test() {
    exec.Command("gh", "pr", "create").Run()
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test3" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 4: Git remote operations (violations)
echo -n "Test 4: Git remote violation detection... "
mkdir -p "$TEST_DIR/test4"
cat > "$TEST_DIR/test4/git-remote.go" << 'EOF'
package main

import "os/exec"

func test() {
    exec.Command("git", "fetch", "origin").Run()
    exec.Command("git", "push", "origin", "main").Run()
    exec.Command("git", "clone", "repo").Run()
    exec.Command("git", "ls-remote", "origin").Run()
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test4" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
else
    output=$("$SCRIPT_PATH" "$TEST_DIR/test4" 2>&1 | grep -c "unbounded git remote operation" || true)
    if [ "$output" -eq 4 ]; then
        echo -e "${GREEN}PASS${NC}"
    else
        echo -e "${RED}FAIL${NC} - expected 4 violations but got $output"
        exit 1
    fi
fi

# Test 5: Git local operations (allowed)
echo -n "Test 5: Git local operations allowed... "
mkdir -p "$TEST_DIR/test5"
cat > "$TEST_DIR/test5/git-local.go" << 'EOF'
package main

import "os/exec"

func test() {
    exec.Command("git", "status", "--porcelain").Output()
    exec.Command("git", "log", "--format=%s").Output()
    exec.Command("git", "rev-parse", "HEAD").Run()
    exec.Command("git", "worktree", "add", "path").Run()
    exec.Command("git", "rev-list", "--count", "HEAD").Output()
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test5" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected success (local git ops allowed) but got failure"
    exit 1
fi

# Test 6: internal/proc directory is excluded
echo -n "Test 6: internal/proc excluded... "
mkdir -p "$TEST_DIR/test6/internal/proc"
cat > "$TEST_DIR/test6/internal/proc/proc.go" << 'EOF'
package proc

import "os/exec"

func Run() {
    // These should be ignored (internal/proc is excluded)
    exec.Command("docker", "run").Run()
    exec.Command("gh", "pr", "create").Run()
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test6" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - internal/proc should be excluded"
    exit 1
fi

# Test 7: Real harness codebase (with allowlist)
echo -n "Test 7: Real harness allowlist... "
# Test against the actual harness directory which has the allowlisted files
HARNESS_DIR="$(dirname "$(dirname "$0")")"
if [ -d "$HARNESS_DIR/internal/session" ] && [ -d "$HARNESS_DIR/internal/sandbox" ]; then
    if "$SCRIPT_PATH" "$HARNESS_DIR" > /dev/null 2>&1; then
        echo -e "${GREEN}PASS${NC}"
    else
        echo -e "${RED}FAIL${NC} - real harness should pass with allowlist"
        exit 1
    fi
else
    echo -e "${GREEN}SKIP${NC} (harness dirs not found)"
fi

# Test 8: Non-daemon commands allowed
echo -n "Test 8: Non-daemon commands allowed... "
mkdir -p "$TEST_DIR/test8"
cat > "$TEST_DIR/test8/misc.go" << 'EOF'
package main

import "os/exec"

func test() {
    exec.Command("ls", "-la").Run()
    exec.Command("echo", "hello").Output()
    exec.Command("cat", "file.txt").Output()
    exec.Command("grep", "pattern", "file").Run()
}
EOF

if "$SCRIPT_PATH" "$TEST_DIR/test8" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - non-daemon commands should be allowed"
    exit 1
fi

echo
echo -e "${GREEN}All tests passed!${NC}"