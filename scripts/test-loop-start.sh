#!/bin/bash
# Unit tests for loop-start.sh (BEH-578).
#
# Drives the start script with a FAKE loop binary (via the LOOP_BIN/LOOP_PIDFILE/
# LOOP_LOGFILE env overrides) so no real daemon launches, and asserts the
# detached-launch contract: a pidfile pointing at the live child, the child's
# output captured to the logfile, a prompt non-blocking return, the
# already-running guard, the stale-pidfile recovery, and the missing-binary
# failure. Like the other harness guards it must run under macOS's stock bash 3.2.

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

SCRIPT_PATH="$(dirname "$0")/loop-start.sh"
TEST_DIR=$(mktemp -d)

# Track every fake daemon we launch so the trap can tear them down — a detached
# nohup child outlives this test process otherwise.
STARTED_PIDS=""

cleanup() {
    for p in $STARTED_PIDS; do
        kill "$p" 2>/dev/null || true
    done
    rm -rf "$TEST_DIR"
}
trap cleanup EXIT

# A stand-in for bin/loop: announces itself to stdout, then becomes a long sleep
# (via exec, so the recorded PID is the live process we can signal/kill).
FAKE_BIN="$TEST_DIR/fake-loop"
cat > "$FAKE_BIN" <<'EOF'
#!/bin/bash
echo "fake-loop started"
exec sleep 60
EOF
chmod +x "$FAKE_BIN"

# Poll for up to ~2s for a file to contain a substring (the detached child writes
# its output asynchronously, after loop-start.sh has already returned).
wait_for_log() {
    file="$1"
    needle="$2"
    i=0
    while [ "$i" -lt 20 ]; do
        if [ -f "$file" ] && grep -q "$needle" "$file"; then
            return 0
        fi
        sleep 0.1
        i=$((i + 1))
    done
    return 1
}

echo "Testing loop-start.sh..."
echo

# Test 1: a detached launch writes a pidfile pointing at a live child and streams
# the child's output to the logfile. The fake sleeps 60s, so the script returning
# at all proves it did NOT block on the child (detached, non-blocking).
echo -n "Test 1: detached launch writes live pidfile + captures log... "
PIDFILE="$TEST_DIR/loop.pid"
LOGFILE="$TEST_DIR/loop.log"
LOOP_BIN="$FAKE_BIN" LOOP_PIDFILE="$PIDFILE" LOOP_LOGFILE="$LOGFILE" "$SCRIPT_PATH" > /dev/null 2>&1
pid="$(cat "$PIDFILE" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $pid"
if [ -z "$pid" ]; then
    echo -e "${RED}FAIL${NC} - no pidfile written at $PIDFILE"
    exit 1
elif ! kill -0 "$pid" 2>/dev/null; then
    echo -e "${RED}FAIL${NC} - pidfile pid $pid is not a live process"
    exit 1
elif ! wait_for_log "$LOGFILE" "fake-loop started"; then
    echo -e "${RED}FAIL${NC} - child output not captured to $LOGFILE"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 2: a missing/unbuilt binary fails loud (non-zero + actionable message) and
# writes NO pidfile — rather than nohup'ing a non-existent path, which would
# record a pidfile for an instantly-dead process.
echo -n "Test 2: missing binary fails loud, writes no pidfile... "
MISSING_BIN="$TEST_DIR/not-built-loop"
PIDFILE2="$TEST_DIR/loop2.pid"
LOGFILE2="$TEST_DIR/loop2.log"
report2=$(LOOP_BIN="$MISSING_BIN" LOOP_PIDFILE="$PIDFILE2" LOOP_LOGFILE="$LOGFILE2" "$SCRIPT_PATH" 2>&1 || true)
if LOOP_BIN="$MISSING_BIN" LOOP_PIDFILE="$PIDFILE2" LOOP_LOGFILE="$LOGFILE2" "$SCRIPT_PATH" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - expected non-zero exit for a missing binary"
    exit 1
elif [ -f "$PIDFILE2" ]; then
    echo -e "${RED}FAIL${NC} - a missing binary must not write a pidfile, but $PIDFILE2 exists"
    exit 1
elif ! echo "$report2" | grep -qiE 'not found|make build'; then
    echo -e "${RED}FAIL${NC} - expected an actionable 'not found / make build' message, got: $report2"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 3: with a loop already running (pidfile points at a live process), a second
# launch refuses (non-zero + message) and does NOT overwrite the pidfile — two
# daemons would race the same ready-for-agent queue.
echo -n "Test 3: refuses to start over an already-running loop... "
PIDFILE3="$TEST_DIR/loop3.pid"
LOGFILE3="$TEST_DIR/loop3.log"
LOOP_BIN="$FAKE_BIN" LOOP_PIDFILE="$PIDFILE3" LOOP_LOGFILE="$LOGFILE3" "$SCRIPT_PATH" > /dev/null 2>&1
first_pid="$(cat "$PIDFILE3")"
STARTED_PIDS="$STARTED_PIDS $first_pid"
report3=$(LOOP_BIN="$FAKE_BIN" LOOP_PIDFILE="$PIDFILE3" LOOP_LOGFILE="$LOGFILE3" "$SCRIPT_PATH" 2>&1 || true)
second_pid="$(cat "$PIDFILE3")"
if LOOP_BIN="$FAKE_BIN" LOOP_PIDFILE="$PIDFILE3" LOOP_LOGFILE="$LOGFILE3" "$SCRIPT_PATH" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - a second launch over a live loop should exit non-zero"
    # Whatever extra daemon that errant success spawned must still be reaped.
    STARTED_PIDS="$STARTED_PIDS $(cat "$PIDFILE3" 2>/dev/null || true)"
    exit 1
elif [ "$second_pid" != "$first_pid" ]; then
    echo -e "${RED}FAIL${NC} - the pidfile was overwritten ($first_pid -> $second_pid)"
    STARTED_PIDS="$STARTED_PIDS $second_pid"
    exit 1
elif ! echo "$report3" | grep -qiE 'already running'; then
    echo -e "${RED}FAIL${NC} - expected an 'already running' message, got: $report3"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# Test 4: a STALE pidfile (its pid is dead) is cleared and the launch proceeds —
# a leftover pidfile from a crashed/killed daemon must not wedge restarts.
echo -n "Test 4: stale pidfile is cleared and launch proceeds... "
PIDFILE4="$TEST_DIR/loop4.pid"
LOGFILE4="$TEST_DIR/loop4.log"
# Capture a definitely-dead pid: spawn a no-op and reap it.
( exit 0 ) &
dead_pid=$!
wait "$dead_pid" 2>/dev/null || true
echo "$dead_pid" > "$PIDFILE4"
LOOP_BIN="$FAKE_BIN" LOOP_PIDFILE="$PIDFILE4" LOOP_LOGFILE="$LOGFILE4" "$SCRIPT_PATH" > /dev/null 2>&1
new_pid="$(cat "$PIDFILE4" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $new_pid"
if [ -z "$new_pid" ] || [ "$new_pid" = "$dead_pid" ]; then
    echo -e "${RED}FAIL${NC} - stale pidfile ($dead_pid) was not replaced (got '$new_pid')"
    exit 1
elif ! kill -0 "$new_pid" 2>/dev/null; then
    echo -e "${RED}FAIL${NC} - the freshly launched pid $new_pid is not alive"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

echo
echo -e "${GREEN}All tests passed!${NC}"
