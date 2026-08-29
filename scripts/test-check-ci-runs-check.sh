#!/bin/bash
# Unit tests for check-ci-runs-check.sh (BEH-462).
#
# The guard fails if the harness CI workflow re-enumerates a `make check`
# sub-target as its own step (the divergence behind BEH-457/BEH-409) instead of
# running `make check` directly. Fixtures stand in a workflow YAML + a Makefile
# so the cases are exercised without touching the real files. Like the other
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

SCRIPT_PATH="$(dirname "$0")/check-ci-runs-check.sh"

# A Makefile whose `check` target has the canonical prerequisite list. Most
# tests reuse it; the divergence test overrides it with an extra sub-target.
CANONICAL_MAKEFILE='check: fmt-check vet check-exec check-bash3 check-buildvcs check-ci test
	@true
'

echo "Testing check-ci-runs-check.sh..."
echo

# Test 1 (tracer): a workflow that re-enumerates the check sub-targets as their
# own steps is flagged — the exact BEH-457/BEH-409 bug shape. Assert on the
# report content (not just exit status) so a broken guard can't fake a RED.
echo -n "Test 1: re-enumerated sub-targets flagged with actionable report... "
mkdir -p "$TEST_DIR/test1"
printf '%s' "$CANONICAL_MAKEFILE" > "$TEST_DIR/test1/Makefile"
cat > "$TEST_DIR/test1/wf.yaml" <<'YAML'
steps:
  - name: Format check
    run: make fmt-check
  - name: Vet
    run: make vet
  - name: Test
    run: make test
  - name: Build
    run: make build
YAML

report=$("$SCRIPT_PATH" "$TEST_DIR/test1/wf.yaml" "$TEST_DIR/test1/Makefile" 2>&1 || true)
if "$SCRIPT_PATH" "$TEST_DIR/test1/wf.yaml" "$TEST_DIR/test1/Makefile" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
elif echo "$report" | grep -qE 'fmt-check' && echo "$report" | grep -qiE 'make check'; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected a report naming an enumerated sub-target and 'make check', got: $report"
    exit 1
fi

# Test 2: a workflow that runs `make check` then `make build` passes — the fix.
echo -n "Test 2: 'make check' + 'make build' workflow passes... "
mkdir -p "$TEST_DIR/test2"
printf '%s' "$CANONICAL_MAKEFILE" > "$TEST_DIR/test2/Makefile"
cat > "$TEST_DIR/test2/wf.yaml" <<'YAML'
steps:
  - name: Check
    run: make check
  - name: Build
    run: make build
YAML

if "$SCRIPT_PATH" "$TEST_DIR/test2/wf.yaml" "$TEST_DIR/test2/Makefile" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - a 'make check' + 'make build' workflow should pass"
    exit 1
fi

# Test 3: a workflow that never invokes `make check` is flagged, even when it
# enumerates nothing — running only `make build` is not a gate.
echo -n "Test 3: workflow missing 'make check' flagged... "
mkdir -p "$TEST_DIR/test3"
printf '%s' "$CANONICAL_MAKEFILE" > "$TEST_DIR/test3/Makefile"
cat > "$TEST_DIR/test3/wf.yaml" <<'YAML'
steps:
  - name: Build
    run: make build
YAML

report3=$("$SCRIPT_PATH" "$TEST_DIR/test3/wf.yaml" "$TEST_DIR/test3/Makefile" 2>&1 || true)
if "$SCRIPT_PATH" "$TEST_DIR/test3/wf.yaml" "$TEST_DIR/test3/Makefile" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
elif echo "$report3" | grep -qiE "does not run 'make check'"; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected a 'does not run make check' report, got: $report3"
    exit 1
fi

# Test 4 (the point of the guard): a *new* sub-target added to `check:` and run
# as its own step in the workflow is flagged — without the guard knowing the name
# ahead of time. This is the divergence BEH-462 exists to stop: a future
# prerequisite must be covered with no edit to this guard.
echo -n "Test 4: a future enumerated sub-target is flagged dynamically... "
mkdir -p "$TEST_DIR/test4"
printf 'check: fmt-check check-newguard test\n\t@true\n' > "$TEST_DIR/test4/Makefile"
cat > "$TEST_DIR/test4/wf.yaml" <<'YAML'
steps:
  - name: Check
    run: make check
  - name: New guard
    run: make check-newguard
  - name: Build
    run: make build
YAML

report4=$("$SCRIPT_PATH" "$TEST_DIR/test4/wf.yaml" "$TEST_DIR/test4/Makefile" 2>&1 || true)
if "$SCRIPT_PATH" "$TEST_DIR/test4/wf.yaml" "$TEST_DIR/test4/Makefile" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure but got success"
    exit 1
elif echo "$report4" | grep -qE 'check-newguard'; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected 'make check-newguard' to be flagged, got: $report4"
    exit 1
fi

# Test 5: a comment mentioning a sub-target is not a step — don't flag it. The
# workflow is otherwise correct (runs make check), so it must pass.
echo -n "Test 5: a sub-target named only in a comment is not flagged... "
mkdir -p "$TEST_DIR/test5"
printf '%s' "$CANONICAL_MAKEFILE" > "$TEST_DIR/test5/Makefile"
cat > "$TEST_DIR/test5/wf.yaml" <<'YAML'
steps:
  # make fmt-check and make vet are run by `make check`, not as steps here.
  - name: Check
    run: make check
  - name: Build
    run: make build
YAML

report5=$("$SCRIPT_PATH" "$TEST_DIR/test5/wf.yaml" "$TEST_DIR/test5/Makefile" 2>&1 || true)
if "$SCRIPT_PATH" "$TEST_DIR/test5/wf.yaml" "$TEST_DIR/test5/Makefile" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - a sub-target in a comment must not be flagged, got: $report5"
    exit 1
fi

# Test 6 (dogfood): the real harness workflow + Makefile pass — they must run
# `make check` directly, with no sub-target re-enumeration.
echo -n "Test 6: the real workflow + Makefile pass... "
if "$SCRIPT_PATH" > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - the real agent-harness CI workflow has drifted from 'make check'"
    "$SCRIPT_PATH" || true
    exit 1
fi

# --- discovery picks the GATE workflow, not just any make-invoking one --------
#
# The workflow path used to be hardcoded to the Consumer's
# `.github/workflows/agent-harness.yaml`. On extraction that file does not exist,
# so the guard died with "workflow not found" and stopped guarding anything
# (BEH-641). Discovery must find the gate wherever it lives — and must not be
# fooled by a sibling workflow that merely runs some other `make` target.
echo -n "Test 7: discovery finds the gate, ignoring other make-invoking workflows... "
DISC="$TEST_DIR/discovery"
mkdir -p "$DISC/scripts" "$DISC/.github/workflows"
cp "$SCRIPT_PATH" "$DISC/scripts/"
cp "$(dirname "$SCRIPT_PATH")/../Makefile" "$DISC/Makefile"
# An image-build workflow, alphabetically first, that runs `make` but is NOT the gate.
cat > "$DISC/.github/workflows/aaa-image.yaml" <<'EOF'
name: Image
jobs:
  image:
    steps:
      - run: make base-image
EOF
cat > "$DISC/.github/workflows/ci.yml" <<'EOF'
name: CI
jobs:
  gates:
    steps:
      - run: make check
      - run: make build
EOF
if out=$("$DISC/scripts/$(basename "$SCRIPT_PATH")" 2>&1); then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - discovery did not settle on the gate workflow:"
    echo "$out"
    exit 1
fi

# --- no gate workflow at all is a failure, not a silent pass -----------------
echo -n "Test 8: a repo with no gate workflow fails loud... "
NOGATE="$TEST_DIR/nogate"
mkdir -p "$NOGATE/scripts" "$NOGATE/.github/workflows"
cp "$SCRIPT_PATH" "$NOGATE/scripts/"
cp "$(dirname "$SCRIPT_PATH")/../Makefile" "$NOGATE/Makefile"
cat > "$NOGATE/.github/workflows/unrelated.yml" <<'EOF'
name: Unrelated
jobs:
  hello:
    steps:
      - run: echo hi
EOF
if "$NOGATE/scripts/$(basename "$SCRIPT_PATH")" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - passed with no workflow running the gate at all"
    exit 1
fi
echo -e "${GREEN}PASS${NC}"

echo
echo -e "${GREEN}All tests passed!${NC}"
