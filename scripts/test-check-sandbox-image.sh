#!/bin/bash
# Unit tests for check-sandbox-image.sh (#40).
#
# The probe runs a sandbox image the way a Gate does — through the entrypoint, in
# a non-login `bash -c` (#38) — and fails if a toolchain binary is not on that
# shell's PATH. A
# fake `docker` (DOCKER=...) stands in for the daemon: it records its argv and
# answers `command -v` from a list of "installed" tools, so the cases run with no
# docker and no image. Like the other harness guards, this must run under macOS's
# stock bash 3.2.

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

SCRIPT_PATH="$(dirname "$0")/check-sandbox-image.sh"

# The fake docker logs its argv one arg per line to $FAKE_LOG, then plays the
# container: it evaluates the bash -c script it was handed with `command -v`
# answered from $FAKE_TOOLS (space-separated), so a tool absent from the list is
# "not on the Gate's PATH".
FAKE_DOCKER="$TEST_DIR/docker"
cat > "$FAKE_DOCKER" <<'EOF'
#!/bin/bash
printf '%s\n' "$@" > "$FAKE_LOG"
[ -z "${FAKE_EXIT:-}" ] || exit "$FAKE_EXIT"
script="${!#}"
command() {
    [ "$1" = "-v" ] || return 2
    case " $FAKE_TOOLS " in
    *" $2 "*) echo "/usr/local/go/bin/$2" ;;
    *) return 1 ;;
    esac
}
eval "$script"
EOF
chmod +x "$FAKE_DOCKER"

echo "Testing check-sandbox-image.sh..."
echo

# Test 1: the #40 bug — go is in the image but not on the Gate shell's PATH. The
# report must name the missing tool, so a broken probe can't fake a RED.
echo -n "Test 1: a tool missing from the Gate's shell is flagged by name... "
report=$(DOCKER="$FAKE_DOCKER" FAKE_LOG="$TEST_DIR/log1" FAKE_TOOLS="gofmt" \
    "$SCRIPT_PATH" img:test 2>&1) && status=0 || status=$?
if [ "$status" -eq 0 ]; then
    echo -e "${RED}FAIL${NC} - expected failure but got success: $report"
    exit 1
elif echo "$report" | grep -qE "PATH in img:test: go$"; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected a report naming 'go', got: $report"
    exit 1
fi

# Test 2: every tool resolves — the probe passes.
echo -n "Test 2: go and gofmt both on the Gate shell's PATH passes... "
if DOCKER="$FAKE_DOCKER" FAKE_LOG="$TEST_DIR/log2" FAKE_TOOLS="go gofmt" \
    "$SCRIPT_PATH" img:test > /dev/null 2>&1; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - a Gate shell with go and gofmt should pass"
    exit 1
fi

# Test 3: the probe runs the image the way a Gate does — a throwaway container,
# through the entrypoint (which needs PROJECT_PATH), in the non-login `bash -c`
# the harness uses since #38. Asserting the argv is the point: a probe in a
# different shell than the Gate's checks a PATH the Gate never sees (#40, #83).
echo -n "Test 3: probes via docker run --rm, PROJECT_PATH, bash -c... "
DOCKER="$FAKE_DOCKER" FAKE_LOG="$TEST_DIR/log3" FAKE_TOOLS="go gofmt" \
    "$SCRIPT_PATH" img:test > /dev/null 2>&1
argv="$(tr '\n' ' ' < "$TEST_DIR/log3")"
case "$argv" in
"run --rm -e PROJECT_PATH="*" img:test bash -c "*)
    echo -e "${GREEN}PASS${NC}"
    ;;
*)
    echo -e "${RED}FAIL${NC} - unexpected docker argv: $argv"
    exit 1
    ;;
esac

# Test 4: explicit tool names replace the default list.
echo -n "Test 4: explicit tool args are probed instead of the default... "
report4=$(DOCKER="$FAKE_DOCKER" FAKE_LOG="$TEST_DIR/log4" FAKE_TOOLS="go gofmt" \
    "$SCRIPT_PATH" img:test dotnet 2>&1) && status=0 || status=$?
if [ "$status" -ne 0 ] && echo "$report4" | grep -q "dotnet"; then
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${RED}FAIL${NC} - expected dotnet flagged, got (exit $status): $report4"
    exit 1
fi

# Test 5: no image is a usage error, not a silent pass.
echo -n "Test 5: no image argument is a usage error... "
if DOCKER="$FAKE_DOCKER" FAKE_LOG="$TEST_DIR/log5" "$SCRIPT_PATH" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected a usage error"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 6: an image docker cannot run (not built, bad entrypoint) fails the probe —
# a run that never reached the shell must not read as "nothing missing".
echo -n "Test 6: a docker run failure fails the probe... "
if DOCKER="$FAKE_DOCKER" FAKE_LOG="$TEST_DIR/log6" FAKE_TOOLS="go gofmt" FAKE_EXIT=125 \
    "$SCRIPT_PATH" img:test > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected failure when docker run fails"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

echo
echo -e "${GREEN}All tests passed!${NC}"
