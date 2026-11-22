#!/bin/bash
# Unit tests for retry.sh (#372).
#
# CI's "Check, build" job fetches origin/main over the network for the script
# test coverage step, and one failed fetch reds the required gate on a commit
# whose gates pass. retry.sh runs a command up to N times with a growing pause
# between tries. A stub command that fails a set number of times stands in for
# the network, and RETRY_DELAY=0 keeps the tests free of wall-clock sleeps. Like
# the other harness scripts, this must run under macOS's stock bash 3.2.

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
SCRIPT_PATH="$SCRIPT_DIR/retry.sh"

# A command that fails with exit 128 (git's code for a failed fetch) until it
# has run FAILS times, counting its runs in $TEST_DIR/runs.
FLAKY="$TEST_DIR/flaky"
cat > "$FLAKY" <<EOF
#!/bin/bash
n=\$(( \$(cat "$TEST_DIR/runs" 2>/dev/null || echo 0) + 1 ))
echo "\$n" > "$TEST_DIR/runs"
[ "\$n" -gt "\$FAILS" ] || { echo "fatal: unable to access origin (try \$n)" >&2; exit 128; }
echo "fetched on try \$n"
EOF
chmod +x "$FLAKY"

runs() { cat "$TEST_DIR/runs" 2>/dev/null || echo 0; }

fail() {
    echo -e "${RED}FAIL${NC} - $1"
    exit 1
}

echo "Testing retry.sh..."
echo

echo -n "a command that succeeds first time runs once and passes its output through ... "
rm -f "$TEST_DIR/runs"
out=$(FAILS=0 RETRY_DELAY=0 "$SCRIPT_PATH" 3 "$FLAKY" 2>&1) || fail "exit $?: $out"
[ "$(runs)" = 1 ] || fail "ran $(runs) times, want 1"
[ "$out" = "fetched on try 1" ] || fail "output: $out"
echo -e "${GREEN}PASS${NC}"

echo -n "the incident: a fetch that fails once passes on the next try, and says it retried ... "
rm -f "$TEST_DIR/runs"
out=$(FAILS=1 RETRY_DELAY=0 "$SCRIPT_PATH" 3 "$FLAKY" 2>&1) || fail "exit $?: $out"
[ "$(runs)" = 2 ] || fail "ran $(runs) times, want 2"
grep -q "fetched on try 2" <<< "$out" || fail "output: $out"
grep -q "try 1 of 3 failed (exit 128)" <<< "$out" || fail "no line naming the failed try: $out"
echo -e "${GREEN}PASS${NC}"

echo -n "a command that fails every try stops at the bound and exits with its last code ... "
rm -f "$TEST_DIR/runs"
rc=0
out=$(FAILS=9 RETRY_DELAY=0 "$SCRIPT_PATH" 3 "$FLAKY" 2>&1) || rc=$?
[ "$rc" = 128 ] || fail "exit $rc, want the command's own 128: $out"
[ "$(runs)" = 3 ] || fail "ran $(runs) times, want 3"
grep -q "gave up after 3 tries" <<< "$out" || fail "no give-up line: $out"
echo -e "${GREEN}PASS${NC}"

echo -n "the pause grows with each try: RETRY_DELAY, then twice it, none after the last ... "
STUB_BIN="$TEST_DIR/stubbin"
mkdir -p "$STUB_BIN"
cat > "$STUB_BIN/sleep" <<EOF
#!/bin/bash
echo "\$1" >> "$TEST_DIR/sleeps"
EOF
chmod +x "$STUB_BIN/sleep"
rm -f "$TEST_DIR/runs" "$TEST_DIR/sleeps"
FAILS=9 RETRY_DELAY=4 PATH="$STUB_BIN:$PATH" "$SCRIPT_PATH" 3 "$FLAKY" > /dev/null 2>&1 || true
[ "$(tr '\n' ' ' < "$TEST_DIR/sleeps")" = "4 8 " ] || fail "slept: $(cat "$TEST_DIR/sleeps")"
echo -e "${GREEN}PASS${NC}"

echo -n "a missing or non-numeric bound, or no command, is a usage error that runs nothing ... "
rm -f "$TEST_DIR/runs"
for args in "" "x $FLAKY" "0 $FLAKY" "3"; do
    rc=0
    # shellcheck disable=SC2086 # split the case into its arguments
    FAILS=0 RETRY_DELAY=0 "$SCRIPT_PATH" $args > /dev/null 2>&1 || rc=$?
    [ "$rc" = 64 ] || fail "'retry.sh $args' exited $rc, want 64"
done
[ "$(runs)" = 0 ] || fail "a usage error ran the command"
echo -e "${GREEN}PASS${NC}"

echo
echo "All retry.sh tests passed."
