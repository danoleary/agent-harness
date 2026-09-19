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

# --- default runtime paths land in the Consumer's .agent-harness/ dir ---------
#
# The viewer derives the pidfile and the STOP sentinel as siblings of the logs/
# dir it tails, so a pidfile written anywhere else makes a running daemon read as
# stopped. With PROJECT_PATH set and no explicit overrides, both defaults must
# land under $PROJECT_PATH/.agent-harness/ (BEH-641).
echo -n "loop-start.sh defaults pidfile + log under \$PROJECT_PATH/.agent-harness ... "
PROJ_DIR="$TEST_DIR/consumer"
mkdir -p "$PROJ_DIR"
PROJECT_PATH="$PROJ_DIR" LOOP_BIN="$FAKE_BIN" "$SCRIPT_PATH" > /dev/null 2>&1
default_pidfile="$PROJ_DIR/.agent-harness/loop.pid"
default_logfile="$PROJ_DIR/.agent-harness/loop.log"
default_pid="$(cat "$default_pidfile" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $default_pid"
if [ ! -f "$default_pidfile" ]; then
    echo -e "${RED}FAIL${NC} - no pidfile at $default_pidfile"
    exit 1
elif [ ! -f "$default_logfile" ]; then
    echo -e "${RED}FAIL${NC} - no logfile at $default_logfile"
    exit 1
elif [ -z "$default_pid" ] || ! kill -0 "$default_pid" 2>/dev/null; then
    echo -e "${RED}FAIL${NC} - pidfile at $default_pidfile does not name a live process"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# --- PROJECT_PATH is read from the sibling .env when unset in the environment --
#
# The operator's normal path: `./scripts/loop-start.sh` with no exported env, the
# same .env the daemon itself reads. The .env must be GREPPED, never sourced — it
# holds live credentials.
echo -n "loop-start.sh reads PROJECT_PATH from the sibling .env ... "
ENV_HARNESS="$TEST_DIR/harness-with-env"
mkdir -p "$ENV_HARNESS/scripts" "$ENV_HARNESS/bin"
cp "$SCRIPT_PATH" "$ENV_HARNESS/scripts/loop-start.sh"
cp "$FAKE_BIN" "$ENV_HARNESS/bin/loop"
ENV_PROJ="$TEST_DIR/consumer-from-env"
mkdir -p "$ENV_PROJ"
# A quoted value, plus a line that would execute if the file were sourced.
{
    echo "GH_TOKEN=\$(touch $TEST_DIR/SOURCED_THE_ENV)"
    echo "PROJECT_PATH=\"$ENV_PROJ\""
} > "$ENV_HARNESS/.env"
( cd "$ENV_HARNESS" && env -u PROJECT_PATH ./scripts/loop-start.sh > /dev/null 2>&1 )
env_pid="$(cat "$ENV_PROJ/.agent-harness/loop.pid" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $env_pid"
if [ -f "$TEST_DIR/SOURCED_THE_ENV" ]; then
    echo -e "${RED}FAIL${NC} - the .env was evaluated, not grepped; a credential line executed"
    exit 1
elif [ -z "$env_pid" ] || ! kill -0 "$env_pid" 2>/dev/null; then
    echo -e "${RED}FAIL${NC} - no live pidfile at $ENV_PROJ/.agent-harness/loop.pid (PROJECT_PATH not read from .env)"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# --- PROJECT_PATH is read from the operator config dir when there is no sibling .env
#
# The download-only operator: binaries on PATH, credentials in
# ~/.config/agent-harness/.env, and no harness checkout for a sibling .env to sit
# beside. Without this lookup PROJECT_PATH stays unset here, the pidfile and log
# default beside the binary instead of under the Consumer's .agent-harness/, and
# `watch` reports a running daemon as stopped.
echo -n "loop-start.sh reads PROJECT_PATH from the operator config dir ... "
XDG_HARNESS="$TEST_DIR/harness-no-env"
mkdir -p "$XDG_HARNESS/scripts" "$XDG_HARNESS/bin"
cp "$SCRIPT_PATH" "$XDG_HARNESS/scripts/loop-start.sh"
cp "$FAKE_BIN" "$XDG_HARNESS/bin/loop"
XDG_PROJ="$TEST_DIR/consumer-from-xdg"
mkdir -p "$XDG_PROJ"
XDG_DIR="$TEST_DIR/xdg-config"
mkdir -p "$XDG_DIR/agent-harness"
{
    echo "GH_TOKEN=\$(touch $TEST_DIR/SOURCED_THE_XDG_ENV)"
    echo "PROJECT_PATH=\"$XDG_PROJ\""
} > "$XDG_DIR/agent-harness/.env"
( cd "$XDG_HARNESS" && env -u PROJECT_PATH XDG_CONFIG_HOME="$XDG_DIR" ./scripts/loop-start.sh > /dev/null 2>&1 )
xdg_pid="$(cat "$XDG_PROJ/.agent-harness/loop.pid" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $xdg_pid"
if [ -f "$TEST_DIR/SOURCED_THE_XDG_ENV" ]; then
    echo -e "${RED}FAIL${NC} - the config-dir .env was evaluated, not grepped; a credential line executed"
    exit 1
elif [ -z "$xdg_pid" ] || ! kill -0 "$xdg_pid" 2>/dev/null; then
    echo -e "${RED}FAIL${NC} - no live pidfile at $XDG_PROJ/.agent-harness/loop.pid (PROJECT_PATH not read from the config dir)"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# --- a sibling .env still outranks the config dir --------------------------
#
# The from-source operator must not have their behaviour changed by the new
# lookup: the .env beside the binary is the one the daemon itself prefers when it
# runs from that directory, so loop-start.sh must agree with it.
echo -n "loop-start.sh prefers a sibling .env over the operator config dir ... "
BOTH_HARNESS="$TEST_DIR/harness-both"
mkdir -p "$BOTH_HARNESS/scripts" "$BOTH_HARNESS/bin"
cp "$SCRIPT_PATH" "$BOTH_HARNESS/scripts/loop-start.sh"
cp "$FAKE_BIN" "$BOTH_HARNESS/bin/loop"
SIBLING_PROJ="$TEST_DIR/consumer-sibling-wins"
IGNORED_PROJ="$TEST_DIR/consumer-should-be-ignored"
mkdir -p "$SIBLING_PROJ" "$IGNORED_PROJ"
echo "PROJECT_PATH=\"$SIBLING_PROJ\"" > "$BOTH_HARNESS/.env"
BOTH_XDG="$TEST_DIR/xdg-both"
mkdir -p "$BOTH_XDG/agent-harness"
echo "PROJECT_PATH=\"$IGNORED_PROJ\"" > "$BOTH_XDG/agent-harness/.env"
( cd "$BOTH_HARNESS" && env -u PROJECT_PATH XDG_CONFIG_HOME="$BOTH_XDG" ./scripts/loop-start.sh > /dev/null 2>&1 )
both_pid="$(cat "$SIBLING_PROJ/.agent-harness/loop.pid" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $both_pid"
if [ -f "$IGNORED_PROJ/.agent-harness/loop.pid" ]; then
    echo -e "${RED}FAIL${NC} - the config-dir .env won; a sibling .env must outrank it"
    exit 1
elif [ -z "$both_pid" ] || ! kill -0 "$both_pid" 2>/dev/null; then
    echo -e "${RED}FAIL${NC} - no live pidfile at $SIBLING_PROJ/.agent-harness/loop.pid"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# --- PROJECT_PATH is read from the project's own .agent-harness/.env -------
#
# The in-repo location: credentials beside the config and prompts the project
# already commits, found when the harness is run from that project. The harness
# masks this exact path in every container, which is what makes keeping it there
# safe.
echo -n "loop-start.sh reads PROJECT_PATH from the project's .agent-harness/.env ... "
REPO_HARNESS="$TEST_DIR/harness-for-repo-env"
mkdir -p "$REPO_HARNESS/scripts" "$REPO_HARNESS/bin"
cp "$SCRIPT_PATH" "$REPO_HARNESS/scripts/loop-start.sh"
cp "$FAKE_BIN" "$REPO_HARNESS/bin/loop"
REPO_PROJ="$TEST_DIR/consumer-in-repo-env"
mkdir -p "$REPO_PROJ/.agent-harness"
{
    echo "GH_TOKEN=\$(touch $TEST_DIR/SOURCED_THE_REPO_ENV)"
    echo "PROJECT_PATH=\"$REPO_PROJ\""
} > "$REPO_PROJ/.agent-harness/.env"
# Run FROM the project, with no sibling .env and an empty config dir.
( cd "$REPO_PROJ" && env -u PROJECT_PATH XDG_CONFIG_HOME="$TEST_DIR/empty-xdg" "$REPO_HARNESS/scripts/loop-start.sh" > /dev/null 2>&1 )
repo_pid="$(cat "$REPO_PROJ/.agent-harness/loop.pid" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $repo_pid"
if [ -f "$TEST_DIR/SOURCED_THE_REPO_ENV" ]; then
    echo -e "${RED}FAIL${NC} - the in-repo .env was evaluated, not grepped; a credential line executed"
    exit 1
elif [ -z "$repo_pid" ] || ! kill -0 "$repo_pid" 2>/dev/null; then
    echo -e "${RED}FAIL${NC} - no live pidfile at $REPO_PROJ/.agent-harness/loop.pid (PROJECT_PATH not read from the project)"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

# --- the resolved PROJECT_PATH is handed to the daemon ---------------------
#
# The script places the pidfile and log by the PROJECT_PATH it resolved, but the
# daemon resolves its own env file relative to ITS cwd. Unless the script exports
# what it resolved, the two can pick different files — the pidfile under one
# project while the daemon works another, which is the failure the viewer reports
# as "loop not running". Exporting it makes them agree by construction.
echo -n "loop-start.sh exports the resolved PROJECT_PATH to the daemon ... "
EXPORT_HARNESS="$TEST_DIR/harness-export"
mkdir -p "$EXPORT_HARNESS/scripts" "$EXPORT_HARNESS/bin"
cp "$SCRIPT_PATH" "$EXPORT_HARNESS/scripts/loop-start.sh"
EXPORT_PROJ="$TEST_DIR/consumer-export"
mkdir -p "$EXPORT_PROJ"
echo "PROJECT_PATH=\"$EXPORT_PROJ\"" > "$EXPORT_HARNESS/.env"
# A fake daemon that records the PROJECT_PATH it actually inherited.
cat > "$EXPORT_HARNESS/bin/loop" <<EOF
#!/bin/bash
echo "PROJECT_PATH=\${PROJECT_PATH:-<unset>}" > "$TEST_DIR/inherited-project-path"
exec sleep 60
EOF
chmod +x "$EXPORT_HARNESS/bin/loop"
( cd "$EXPORT_HARNESS" && env -u PROJECT_PATH ./scripts/loop-start.sh > /dev/null 2>&1 )
export_pid="$(cat "$EXPORT_PROJ/.agent-harness/loop.pid" 2>/dev/null || true)"
STARTED_PIDS="$STARTED_PIDS $export_pid"
wait_for_log "$TEST_DIR/inherited-project-path" "PROJECT_PATH="
inherited="$(cat "$TEST_DIR/inherited-project-path" 2>/dev/null || true)"
if [ "$inherited" != "PROJECT_PATH=$EXPORT_PROJ" ]; then
    echo -e "${RED}FAIL${NC} - daemon inherited '$inherited', want 'PROJECT_PATH=$EXPORT_PROJ'"
    exit 1
else
    echo -e "${GREEN}PASS${NC}"
fi

echo
echo -e "${GREEN}All tests passed!${NC}"
