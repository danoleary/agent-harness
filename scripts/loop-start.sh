#!/bin/bash
# Launch the autonomous loop daemon (cmd/loop) as a detached background process
# the operator can walk away from — no tmux, no supervisor (BEH-578; DESIGN.md
# §Run model). It nohup's bin/loop into the background, appends all output to a
# logfile, and records the PID in a pidfile for a hard `kill` if ever needed.
#
# Graceful stop stays `touch agent-harness/STOP` (DESIGN.md §Stop control); the
# pidfile is the hard-kill escape hatch, not the normal stop. No supervisor means
# no auto-restart: a crash or a circuit-breaker trip stays down until the operator
# relaunches (the breaker wants a human to look first). The loop's exit-code
# contract (0 = deliberate stop, non-zero = crash) is what lets a future
# launchd/systemd unit — explicitly deferred — tell those apart and not resurrect
# a clean exit.
#
# Paths are env-overridable (LOOP_BIN/LOOP_PIDFILE/LOOP_LOGFILE) so the behaviour
# is unit-testable without launching the real daemon. It is bash-3.2-safe.

set -euo pipefail

# Resolve the harness dir (the parent of scripts/) so the script works from any
# cwd — bin/loop, the pidfile, and the log all default to living under it.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HARNESS_DIR="$(dirname "$SCRIPT_DIR")"

LOOP_BIN="${LOOP_BIN:-$HARNESS_DIR/bin/loop}"
LOOP_PIDFILE="${LOOP_PIDFILE:-$HARNESS_DIR/loop.pid}"
LOOP_LOGFILE="${LOOP_LOGFILE:-$HARNESS_DIR/loop.log}"

# Refuse to start a second daemon over a live one: a running pid in the pidfile
# means a loop is already up, and two would race the same ready-for-agent queue.
# A STALE pidfile (its process is gone — a crashed or killed daemon) is cleared
# and ignored so it can't wedge restarts.
if [ -f "$LOOP_PIDFILE" ]; then
    existing="$(cat "$LOOP_PIDFILE" 2>/dev/null || true)"
    if [ -n "$existing" ] && kill -0 "$existing" 2>/dev/null; then
        echo "loop already running (pid $existing, pidfile $LOOP_PIDFILE); touch ${HARNESS_DIR}/STOP to stop it first" >&2
        exit 1
    fi
    rm -f "$LOOP_PIDFILE"
fi

# Fail loud if the binary isn't built, rather than nohup'ing a non-existent path
# (which would write a pidfile for an instantly-dead process).
if [ ! -x "$LOOP_BIN" ]; then
    echo "loop binary not found or not executable at $LOOP_BIN — run 'make build' first" >&2
    exit 1
fi

# Detached background launch: nohup divorces the daemon from this shell's session
# so it survives the operator logging out; output appends to the log; $! captures
# the PID for the pidfile.
nohup "$LOOP_BIN" >> "$LOOP_LOGFILE" 2>&1 &
loop_pid=$!
echo "$loop_pid" > "$LOOP_PIDFILE"

echo "loop started (pid $loop_pid); logging to $LOOP_LOGFILE"
echo "  stop gracefully:  touch ${HARNESS_DIR}/STOP"
echo "  hard kill:        kill $loop_pid"
