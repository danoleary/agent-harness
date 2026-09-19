#!/bin/bash
# Launch the autonomous loop daemon (cmd/loop) as a detached background process
# the operator can walk away from — no tmux, no supervisor (BEH-578; DESIGN.md
# §Run model). It nohup's bin/loop into the background, appends all output to a
# logfile, and records the PID in a pidfile for a hard `kill` if ever needed.
#
# Graceful stop stays `touch .agent-harness/STOP` (DESIGN.md §Stop control); the
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

# The pidfile and the daemon log are PER-CONSUMER runtime state, so they default
# beside that Consumer's logs, in its `.agent-harness/` directory — not beside the
# harness binary, which one operator may point at several projects.
#
# The viewer depends on this placement: it derives the pidfile (and the STOP
# sentinel) as the sibling of the `logs/` dir it is tailing, so that it can report
# daemon liveness without re-reading the harness config. Writing the pidfile
# anywhere else makes a running daemon look stopped.
#
# PROJECT_PATH comes from the environment, else from the same env file the daemon
# itself reads, looked up in the same order (config.ResolveEnvFile):
#
#   1. $HARNESS_ENV_FILE                                  explicit
#   2. $HARNESS_DIR/.env                                  beside the binary
#   3. ${XDG_CONFIG_HOME:-$HOME/.config}/agent-harness/.env   operator config dir
#
# The sibling file is checked before the config dir so a from-source operator's
# existing layout keeps winning. The config dir is what an operator who installed
# from a release archive has — no checkout, so no sibling .env — and without this
# lookup PROJECT_PATH stays unset for them, the pidfile and log default beside the
# binary instead of under the Consumer's .agent-harness/, and the viewer reports a
# running daemon as stopped (it derives both from the logs dir it tails, ADR-0006).
#
# If nothing resolves it, fall back to the harness dir so a bare `loop-start.sh`
# still launches something inspectable rather than failing on an unset variable.
harness_config_home="${XDG_CONFIG_HOME:-${HOME:-}/.config}"
for env_candidate in \
    "${HARNESS_ENV_FILE:-}" \
    "$HARNESS_DIR/.env" \
    "${harness_config_home}/agent-harness/.env"; do
    [ -n "${PROJECT_PATH:-}" ] && break
    [ -n "$env_candidate" ] || continue
    [ -f "$env_candidate" ] || continue
    # A single grepped assignment, not `source`: the env file holds live
    # credentials and must not be evaluated by this shell.
    PROJECT_PATH="$(grep -E '^PROJECT_PATH=' "$env_candidate" | tail -n 1 | cut -d= -f2- | sed -e 's/^["'"'"']//' -e 's/["'"'"']$//' || true)"
done

if [ -n "${PROJECT_PATH:-}" ]; then
    RUNTIME_DIR="$PROJECT_PATH/.agent-harness"
    # Hand the daemon the project this launch decided on. It resolves its own env
    # file relative to ITS cwd, which is the caller's — so without this the script
    # and the daemon can read different files and disagree about which project is
    # being worked, leaving the pidfile and log under one while work happens in
    # another. An inherited value wins over any env file (LoadDotEnv fills only
    # unset keys), so exporting it makes the two agree by construction.
    export PROJECT_PATH
else
    RUNTIME_DIR="$HARNESS_DIR"
fi

LOOP_PIDFILE="${LOOP_PIDFILE:-$RUNTIME_DIR/loop.pid}"
LOOP_LOGFILE="${LOOP_LOGFILE:-$RUNTIME_DIR/loop.log}"

# The runtime dir must exist before nohup redirects into it.
mkdir -p "$(dirname "$LOOP_LOGFILE")" "$(dirname "$LOOP_PIDFILE")"

# Refuse to start a second daemon over a live one: a running pid in the pidfile
# means a loop is already up, and two would race the same ready-for-agent queue.
# A STALE pidfile (its process is gone — a crashed or killed daemon) is cleared
# and ignored so it can't wedge restarts.
if [ -f "$LOOP_PIDFILE" ]; then
    existing="$(cat "$LOOP_PIDFILE" 2>/dev/null || true)"
    if [ -n "$existing" ] && kill -0 "$existing" 2>/dev/null; then
        echo "loop already running (pid $existing, pidfile $LOOP_PIDFILE); touch ${RUNTIME_DIR}/STOP to stop it first" >&2
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
echo "  stop gracefully:  touch ${RUNTIME_DIR}/STOP"
echo "  hard kill:        kill $loop_pid"
