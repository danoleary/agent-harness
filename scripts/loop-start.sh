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

# Two optional arguments. A loop name (#106) starts that loop rather than the
# ticket loop: `pr-doctor`, or a [[loops]] entry. It gets its own pidfile and
# log, loop-<name>.pid and loop-<name>.log, so the viewer can probe the daemon it
# follows; `tickets` is the default and keeps loop.pid and loop.log.
#
# --instance <id> (or --instance=<id>) starts one of several ticket-loop daemons
# against the same Consumer (#89). Each instance has its own pidfile, log and
# STOP-<id> sentinel; the default instance (no id) keeps the loop's files and its
# STOP-<name>. Claims, gate slots and the plain STOP are shared by every daemon.
LOOP_NAME=""
LOOP_INSTANCE=""
while [ "$#" -gt 0 ]; do
    case "$1" in
        --instance)
            if [ "$#" -lt 2 ] || [ -z "$2" ]; then
                echo "loop-start.sh: --instance needs an id, e.g. --instance b" >&2
                exit 2
            fi
            LOOP_INSTANCE="$2"
            shift 2
            ;;
        --instance=*)
            LOOP_INSTANCE="${1#--instance=}"
            if [ -z "$LOOP_INSTANCE" ]; then
                echo "loop-start.sh: --instance needs an id, e.g. --instance b" >&2
                exit 2
            fi
            shift
            ;;
        -*)
            echo "loop-start.sh: unknown argument '$1' (usage: loop-start.sh [<name>] [--instance <id>])" >&2
            exit 2
            ;;
        *)
            if [ -n "$LOOP_NAME" ]; then
                echo "loop-start.sh: one loop at a time — got '$LOOP_NAME' and '$1' (start each in its own daemon)" >&2
                exit 2
            fi
            LOOP_NAME="$1"
            shift
            ;;
    esac
done
# The name and the id name files, so they are held to the loop-name rule:
# lowercase letters, digits and dashes, not starting with a dash. The daemon
# re-checks both, refuses a name no loop has, and refuses an id that names a loop.
case "$LOOP_NAME" in
    -*|*[!a-z0-9-]*)
        echo "loop-start.sh: loop '$LOOP_NAME' must be lowercase letters, digits and dashes" >&2
        exit 2
        ;;
esac
case "$LOOP_INSTANCE" in
    -*|*[!a-z0-9-]*)
        echo "loop-start.sh: instance '$LOOP_INSTANCE' must be lowercase letters, digits and dashes" >&2
        exit 2
        ;;
esac

# Resolve the harness dir (the parent of scripts/) so the script works from any
# cwd — bin/loop, the pidfile, and the log all default to living under it.
SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HARNESS_DIR="$(dirname "$SCRIPT_DIR")"

# Where the daemon binary is depends on how the harness was installed, and the
# shipped launcher has to work for every shape:
#
#   $HARNESS_DIR/bin/loop   a source checkout after `make build`
#   $HARNESS_DIR/loop       an older release archive, which laid the binaries at its root
#   $SCRIPT_DIR/loop        the installed layout: <prefix>/libexec/agent-harness/, where
#                           this script sits BESIDE the daemon it launches
#   $(command -v loop)      a loop the operator put on PATH themselves
#
# Looking only in bin/ made the launcher shipped IN the archive fail on the
# archive's own layout, and tell the operator to run `make build` — a target a
# release user does not have.
#
# `agent-harness start` short-circuits all of this: the dispatcher already knows
# which install it belongs to, so it exports LOOP_BIN before exec'ing this script
# and the search below never runs. The search is what keeps the script working
# when it is invoked directly.
if [ -z "${LOOP_BIN:-}" ]; then
    for loop_candidate in "$HARNESS_DIR/bin/loop" "$HARNESS_DIR/loop" "$SCRIPT_DIR/loop"; do
        if [ -x "$loop_candidate" ]; then
            LOOP_BIN="$loop_candidate"
            break
        fi
    done
fi
if [ -z "${LOOP_BIN:-}" ]; then
    # `command -v` resolves the PATH without running anything.
    LOOP_BIN="$(command -v loop 2>/dev/null || true)"
fi
# Keep the bin/ path as the value reported when nothing resolved, so the error
# below names a concrete place rather than an empty string.
LOOP_BIN="${LOOP_BIN:-$HARNESS_DIR/bin/loop}"

# The pidfile and the daemon log are PER-CONSUMER runtime state, so they default
# beside that Consumer's logs, in its `.agent-harness/` directory — not beside the
# harness binary, which one operator may point at several projects.
#
# The viewer depends on this placement: it looks for the pidfile (and the STOP
# sentinels) in the `.agent-harness/` that holds the `logs/` dir it is tailing,
# so that it can report daemon liveness without re-reading the harness config.
# Writing the pidfile anywhere else makes a running daemon look stopped.
#
# PROJECT_PATH comes from the environment, else from the same env file the daemon
# itself reads, looked up in the same order (config.EnvFileCandidates):
#
#   1. $HARNESS_ENV_FILE                                      explicit
#   2. $PWD/.env                                              a harness checkout
#   3. $PWD/.agent-harness/.env                                in the project
#   4. $PROJECT_PATH/.agent-harness/.env                       PROJECT_PATH exported
#   5. ${XDG_CONFIG_HOME:-$HOME/.config}/agent-harness/.env   operator config dir
#
# The two lookups must agree (#118). The daemon inherits this shell's cwd and
# resolves from it; it never looks beside this script. A file only the script
# read could name the project while the daemon loaded another file's credentials.
# The cwd .env is how a from-source operator runs, from the checkout root. The
# in-repo file is what an operator gets when they keep credentials beside the
# config and prompts their project already commits; the harness masks that exact
# path in every container, so the sandbox cannot read it. The config dir is for an
# operator driving several projects from one install.
#
# Without a lookup that covers all of them, PROJECT_PATH stays unset, the pidfile
# and log default beside the binary instead of under the Consumer's
# .agent-harness/, and the viewer reports a running daemon as stopped (it derives
# both from the logs dir it tails, ADR-0006).
#
# If nothing resolves it, fall back to the harness dir so a bare `loop-start.sh`
# still launches something inspectable rather than failing on an unset variable.
harness_config_home="${XDG_CONFIG_HOME:-${HOME:-}/.config}"

# env_file_value KEY prints KEY's value from the first env file above that assigns
# it, or nothing. A single grepped assignment, not `source`: the env file holds
# live credentials and must not be evaluated by this shell.
env_file_value() {
    for env_candidate in \
        "${HARNESS_ENV_FILE:-}" \
        "$PWD/.env" \
        "$PWD/.agent-harness/.env" \
        "${PROJECT_PATH:+$PROJECT_PATH/.agent-harness/.env}" \
        "${harness_config_home}/agent-harness/.env"; do
        [ -n "$env_candidate" ] || continue
        [ -f "$env_candidate" ] || continue
        env_value="$(grep -E "^$1=" "$env_candidate" | tail -n 1 | cut -d= -f2- | sed -e 's/^["'"'"']//' -e 's/["'"'"']$//' || true)"
        if [ -n "$env_value" ]; then
            printf '%s\n' "$env_value"
            return 0
        fi
    done
}

PROJECT_PATH="${PROJECT_PATH:-$(env_file_value PROJECT_PATH)}"

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

# A named instance's files carry its id, and another loop's carry its name, so
# no two daemons share a pidfile, a log or a sentinel (config.Loop.PidFile is the
# daemon's side of this naming). An id may not name a loop, so the two never
# collide. WATCH_HINT is the `agent-harness watch` that follows this daemon.
WATCH_HINT=""
if [ "$LOOP_NAME" = "tickets" ]; then
    LOOP_NAME=""
fi
if [ -n "$LOOP_NAME" ]; then
    set -- "$LOOP_NAME"
    WATCH_HINT="--loop $LOOP_NAME"
fi
if [ -n "$LOOP_INSTANCE" ]; then
    LOOP_PIDFILE="${LOOP_PIDFILE:-$RUNTIME_DIR/loop-$LOOP_INSTANCE.pid}"
    LOOP_LOGFILE="${LOOP_LOGFILE:-$RUNTIME_DIR/loop-$LOOP_INSTANCE.log}"
    STOP_HINT="${RUNTIME_DIR}/STOP-$LOOP_INSTANCE"
    set -- ${1+"$@"} --instance "$LOOP_INSTANCE"
    WATCH_HINT="${WATCH_HINT:+$WATCH_HINT }--instance $LOOP_INSTANCE"
elif [ -n "$LOOP_NAME" ]; then
    LOOP_PIDFILE="${LOOP_PIDFILE:-$RUNTIME_DIR/loop-$LOOP_NAME.pid}"
    LOOP_LOGFILE="${LOOP_LOGFILE:-$RUNTIME_DIR/loop-$LOOP_NAME.log}"
    STOP_HINT="${RUNTIME_DIR}/STOP-$LOOP_NAME"
else
    LOOP_PIDFILE="${LOOP_PIDFILE:-$RUNTIME_DIR/loop.pid}"
    LOOP_LOGFILE="${LOOP_LOGFILE:-$RUNTIME_DIR/loop.log}"
    STOP_HINT="${RUNTIME_DIR}/STOP"
fi

# The runtime dir must exist before nohup redirects into it.
mkdir -p "$(dirname "$LOOP_LOGFILE")" "$(dirname "$LOOP_PIDFILE")"

# Refuse to start a second daemon over a live one of the same loop or instance: a
# running pid in the pidfile means it is already up. Another instance is
# welcome — the claim lock stops two instances working one ticket (#89) — but two
# daemons behind one pidfile could not be stopped or watched apart.
# A STALE pidfile (its process is gone — a crashed or killed daemon) is cleared
# and ignored so it can't wedge restarts.
if [ -f "$LOOP_PIDFILE" ]; then
    existing="$(cat "$LOOP_PIDFILE" 2>/dev/null || true)"
    if [ -n "$existing" ] && kill -0 "$existing" 2>/dev/null; then
        echo "loop already running (pid $existing, pidfile $LOOP_PIDFILE); touch $STOP_HINT to stop it first, or start another with --instance <id>" >&2
        exit 1
    fi
    rm -f "$LOOP_PIDFILE"
fi

# Fail loud if the binary isn't built, rather than nohup'ing a non-existent path
# (which would write a pidfile for an instantly-dead process).
if [ ! -x "$LOOP_BIN" ]; then
    {
        echo "loop binary not found or not executable at $LOOP_BIN."
        echo "  installed with install.sh: run 'agent-harness start' instead of this script"
        echo "  from a source checkout:    run 'make build'"
        echo "  from a release archive:    run it from the unpacked directory"
        echo "  or set LOOP_BIN to the binary explicitly"
    } >&2
    exit 1
fi

# Detached background launch: nohup divorces the daemon from this shell's session
# so it survives the operator logging out; output appends to the log; $! captures
# the PID for the pidfile.
# ${1+"$@"}: macOS bash 3.2 treats a bare "$@" with no arguments as unbound
# under set -u.
nohup "$LOOP_BIN" ${1+"$@"} >> "$LOOP_LOGFILE" 2>&1 &
loop_pid=$!
echo "$loop_pid" > "$LOOP_PIDFILE"

echo "loop started (pid $loop_pid); logging to $LOOP_LOGFILE"
echo "  stop gracefully:  touch $STOP_HINT"
if [ -n "$WATCH_HINT" ]; then
    echo "  stop every loop:  touch ${RUNTIME_DIR}/STOP"
    echo "  watch it:         agent-harness watch $WATCH_HINT"
fi
echo "  hard kill:        kill $loop_pid"

# Optional: hold a sleep assertion for as long as the daemon lives (#49). Idle
# sleep freezes the loop mid-run and a sandbox container can hit its timeouts;
# locking the screen does not, so nothing else in an unattended run prevents it.
# Opt-in, resolved like PROJECT_PATH (environment first, then the env file).
#
# `-w` ties caffeinate to the daemon's pid, so a STOP, a crash or a breaker trip
# releases the assertion with no cleanup step here. `-i` blocks idle sleep; `-s`
# blocks system sleep, but only on AC power. Closing the lid still sleeps the Mac
# unless it is in clamshell mode. Where caffeinate does not exist (Linux) the
# launch goes ahead with a warning — keeping awake is a convenience, not a
# precondition for running.
LOOP_KEEP_AWAKE="${LOOP_KEEP_AWAKE:-$(env_file_value LOOP_KEEP_AWAKE)}"
case "$LOOP_KEEP_AWAKE" in
    1|true|TRUE|True|yes|YES|Yes)
        if command -v caffeinate >/dev/null 2>&1; then
            nohup caffeinate -is -w "$loop_pid" >/dev/null 2>&1 &
            echo "  keeping system awake while loop runs (caffeinate pid $!)"
        else
            echo "warning: LOOP_KEEP_AWAKE is set but caffeinate is not on PATH; the system may sleep while the loop runs" >&2
        fi
        ;;
esac
