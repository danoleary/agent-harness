#!/bin/bash
# Tests for install.sh — the one-command install.
#
# Everything runs against a FIXTURE release served over file://, so the suite is
# offline (make check must stay runnable on a plane) and never touches the real
# GitHub release, the operator's ~/.local, or their PATH. The fixture is laid out
# exactly as the release workflow lays out a real archive; a drift between the two
# layouts is the bug this file exists to catch, since it is invisible until an
# operator runs the one-liner.
#
# Where it can, it drives the REAL dispatcher binary (built on demand when a Go
# toolchain is present), because "the installed layout is one the dispatcher can
# actually find its stage binaries in" is the single claim the whole design rests
# on. Must run under macOS's stock bash 3.2.

set -euo pipefail

if [ -t 1 ]; then
    GREEN='\033[0;32m'
    RED='\033[0;31m'
    YELLOW='\033[0;33m'
    NC='\033[0m'
else
    GREEN=''
    RED=''
    YELLOW=''
    NC=''
fi

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
REPO_DIR="$(dirname "$SCRIPT_DIR")"
INSTALL_SH="$REPO_DIR/install.sh"
TEST_DIR="$(mktemp -d)"
trap 'rm -rf "$TEST_DIR"' EXIT

FAKE_VERSION="v9.9.9"
TOOLS="implementation review retrospective pipeline loop watch"

fail() {
    echo -e "${RED}FAIL${NC} - $1"
    exit 1
}

# The platform half of the asset name, derived the same way install.sh derives it
# — the fixture has to be named for the host actually running the test.
platform() {
    local os arch
    case "$(uname -s)" in
    Darwin) os=darwin ;;
    Linux) os=linux ;;
    *) fail "unsupported test host: $(uname -s)" ;;
    esac
    case "$(uname -m)" in
    x86_64 | amd64) arch=amd64 ;;
    arm64 | aarch64) arch=arm64 ;;
    *) fail "unsupported test host arch: $(uname -m)" ;;
    esac
    printf '%s_%s\n' "$os" "$arch"
}
PLATFORM="$(platform)"
ASSET="agent-harness_${FAKE_VERSION}_${PLATFORM}.tar.gz"

sha256_of() {
    if command -v sha256sum >/dev/null 2>&1; then
        sha256sum "$1" | awk '{print $1}'
    else
        shasum -a 256 "$1" | awk '{print $1}'
    fi
}

# make_release builds a fixture release directory ($1) whose layout mirrors the
# release workflow's: the dispatcher at the archive root, the stage binaries under
# libexec/agent-harness/, the launcher under scripts/, docs beside them. Each fake
# stage binary echoes its own name and arguments, so a dispatch can be asserted on.
# $2, when set, is a tool to OMIT — the partial-archive case.
make_release() {
    local root="$1" omit="${2:-}"
    local stage="$root/stage/agent-harness_${FAKE_VERSION}_${PLATFORM}"
    mkdir -p "$stage/libexec/agent-harness" "$stage/scripts"

    # The dispatcher. A real one when there is a toolchain to build it with,
    # because the installed layout must be one the real search path resolves;
    # a stub otherwise, so the suite still runs without Go.
    if command -v go >/dev/null 2>&1 &&
        go build -buildvcs=false -o "$stage/agent-harness" "$REPO_DIR/cmd/agent-harness" 2>/dev/null; then
        REAL_DISPATCHER=1
    else
        REAL_DISPATCHER=""
        cat >"$stage/agent-harness" <<'EOF'
#!/bin/sh
[ "$1" = "--version" ] && echo "stub" && exit 0
exit 0
EOF
        chmod +x "$stage/agent-harness"
    fi

    local tool
    for tool in $TOOLS; do
        [ "$tool" = "$omit" ] && continue
        cat >"$stage/libexec/agent-harness/$tool" <<EOF
#!/bin/sh
echo "fake-$tool \$*"
EOF
        chmod +x "$stage/libexec/agent-harness/$tool"
    done

    cp "$REPO_DIR/scripts/loop-start.sh" "$stage/scripts/loop-start.sh"
    cp "$REPO_DIR/README.md" "$REPO_DIR/LICENSE" "$REPO_DIR/.env.example" "$stage/"
    cp "$REPO_DIR/docs/CONSUMER.md" "$stage/CONSUMER.md"

    # Pack it and publish checksums the way the release workflow does — including
    # the `./` prefix `sha256sum ./*.tar.gz` writes, which the verifier must strip.
    mkdir -p "$root/releases/download/$FAKE_VERSION"
    tar -czf "$root/releases/download/$FAKE_VERSION/$ASSET" \
        -C "$root/stage" "agent-harness_${FAKE_VERSION}_${PLATFORM}"
    printf '%s  ./%s\n' \
        "$(sha256_of "$root/releases/download/$FAKE_VERSION/$ASSET")" "$ASSET" \
        >"$root/releases/download/$FAKE_VERSION/checksums.txt"
}

# run_install runs the installer against a fixture release. $1 is the release root,
# $2 the prefix to install into; the rest are extra arguments.
run_install() {
    local release="$1" prefix="$2"
    shift 2
    AGENT_HARNESS_BASE_URL="file://$release/releases/download" \
        AGENT_HARNESS_VERSION="$FAKE_VERSION" \
        sh "$INSTALL_SH" --prefix "$prefix" "$@" 2>&1
}

echo "Testing install.sh"
echo "=================="
echo

# --- the happy path lays out every file the design depends on ---------------
echo -n "install.sh installs the dispatcher, the stage binaries and the docs ... "
REL="$TEST_DIR/rel"
PREFIX="$TEST_DIR/prefix"
make_release "$REL"
INSTALL_OUT="$(run_install "$REL" "$PREFIX")" || fail "install failed: $INSTALL_OUT"

[ -x "$PREFIX/bin/agent-harness" ] || fail "no executable at $PREFIX/bin/agent-harness"
for t in $TOOLS; do
    [ -x "$PREFIX/libexec/agent-harness/$t" ] || fail "stage binary $t is not in libexec"
done
[ -x "$PREFIX/libexec/agent-harness/loop-start.sh" ] || fail "the loop launcher is not in libexec"
[ -f "$PREFIX/share/agent-harness/.env.example" ] || fail ".env.example was not installed"
[ -f "$PREFIX/share/agent-harness/CONSUMER.md" ] || fail "CONSUMER.md was not installed"
echo -e "${GREEN}PASS${NC}"

# --- the generic names stay OFF the PATH ------------------------------------
#
# The reason the dispatcher exists. If a stage binary ever lands in bin/, the
# install has silently shadowed watch(1) (and five other common words) on the
# operator's machine.
echo -n "install.sh puts exactly one name in bin/ ... "
bin_entries="$(ls "$PREFIX/bin")"
if [ "$bin_entries" != "agent-harness" ]; then
    fail "bin/ holds more than the dispatcher: $(echo "$bin_entries" | tr '\n' ' ')"
fi
echo -e "${GREEN}PASS${NC}"

# --- the installed layout is one the dispatcher can resolve -----------------
echo -n "the installed dispatcher finds its stage binaries ... "
if [ -n "$REAL_DISPATCHER" ]; then
    dispatched="$("$PREFIX/bin/agent-harness" pipeline BEH-1 2>&1)" ||
        fail "dispatch failed: $dispatched"
    if [ "$dispatched" != "fake-pipeline BEH-1" ]; then
        fail "expected the libexec pipeline to run with its arguments, got: $dispatched"
    fi
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${YELLOW}SKIP${NC} (no Go toolchain to build the real dispatcher)"
fi

# --- a symlinked dispatcher still resolves ----------------------------------
#
# Homebrew, /usr/local/bin and dotfile managers all link the binary rather than
# copying it; resolving siblings against the LINK's directory would break that.
echo -n "a symlinked dispatcher resolves through the link ... "
if [ -n "$REAL_DISPATCHER" ]; then
    mkdir -p "$TEST_DIR/linkbin"
    ln -sf "$PREFIX/bin/agent-harness" "$TEST_DIR/linkbin/agent-harness"
    linked="$("$TEST_DIR/linkbin/agent-harness" review BEH-2 2>&1)" || fail "dispatch through a symlink failed: $linked"
    if [ "$linked" != "fake-review BEH-2" ]; then
        fail "expected the real install's review binary, got: $linked"
    fi
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${YELLOW}SKIP${NC} (no Go toolchain)"
fi

# --- `agent-harness start` reaches the launcher AND the daemon --------------
#
# The one subcommand that is a shell script rather than a Go binary, and the one
# that needs a second resolution: the dispatcher hands loop-start.sh the daemon
# binary belonging to THIS install (LOOP_BIN), because the launcher's own search
# has no way to know which of several installs it was invoked from.
echo -n "agent-harness start launches the installed daemon ... "
if [ -n "$REAL_DISPATCHER" ]; then
    START_PROJ="$TEST_DIR/consumer-start"
    mkdir -p "$START_PROJ"
    start_out="$(PROJECT_PATH="$START_PROJ" PATH="/usr/bin:/bin" "$PREFIX/bin/agent-harness" start 2>&1)" ||
        fail "agent-harness start failed: $start_out"
    start_pid="$(cat "$START_PROJ/.agent-harness/loop.pid" 2>/dev/null || true)"
    [ -n "$start_pid" ] || fail "no pidfile under the project: $start_out"
    # The fake daemon writes its marker and exits, so poll briefly for the log.
    i=0
    while [ "$i" -lt 20 ]; do
        grep -q "fake-loop" "$START_PROJ/.agent-harness/loop.log" 2>/dev/null && break
        sleep 0.1
        i=$((i + 1))
    done
    grep -q "fake-loop" "$START_PROJ/.agent-harness/loop.log" 2>/dev/null ||
        fail "the launcher never started the installed daemon (log: $(cat "$START_PROJ/.agent-harness/loop.log" 2>/dev/null))"
    echo -e "${GREEN}PASS${NC}"
else
    echo -e "${YELLOW}SKIP${NC} (no Go toolchain)"
fi

# --- the operator is told how to reach it -----------------------------------
echo -n "install.sh prints a PATH line when its bin dir is not on PATH ... "
if ! printf '%s' "$INSTALL_OUT" | grep -q "$PREFIX/bin"; then
    fail "output never mentions the install dir: $INSTALL_OUT"
elif ! printf '%s' "$INSTALL_OUT" | grep -q "PATH"; then
    fail "output gives no PATH guidance: $INSTALL_OUT"
elif ! printf '%s' "$INSTALL_OUT" | grep -q "agent-harness --help"; then
    fail "output does not tell the operator what to run next: $INSTALL_OUT"
fi
echo -e "${GREEN}PASS${NC}"

# --- installing twice is a plain upgrade ------------------------------------
echo -n "install.sh is idempotent (re-running upgrades in place) ... "
run_install "$REL" "$PREFIX" >/dev/null || fail "the second install failed"
[ -x "$PREFIX/bin/agent-harness" ] || fail "the dispatcher went missing on re-install"
echo -e "${GREEN}PASS${NC}"

# --- a tampered archive is refused ------------------------------------------
#
# This script's output is executed, so a download that is not the one the release
# published must never be unpacked — failing closed is the whole point.
echo -n "install.sh refuses an archive whose checksum does not match ... "
BAD_REL="$TEST_DIR/bad"
BAD_PREFIX="$TEST_DIR/bad-prefix"
make_release "$BAD_REL"
printf 'tampered\n' >>"$BAD_REL/releases/download/$FAKE_VERSION/$ASSET"
if bad_out="$(run_install "$BAD_REL" "$BAD_PREFIX")"; then
    fail "install succeeded on a tampered archive"
elif ! printf '%s' "$bad_out" | grep -qi "checksum mismatch"; then
    fail "the failure does not name the cause: $bad_out"
elif [ -e "$BAD_PREFIX/bin/agent-harness" ]; then
    fail "a tampered archive still installed a binary"
fi
echo -e "${GREEN}PASS${NC}"

# --- a missing checksums.txt is refused too ---------------------------------
echo -n "install.sh refuses to install when checksums.txt is absent ... "
NOSUM_REL="$TEST_DIR/nosum"
NOSUM_PREFIX="$TEST_DIR/nosum-prefix"
make_release "$NOSUM_REL"
rm -f "$NOSUM_REL/releases/download/$FAKE_VERSION/checksums.txt"
if nosum_out="$(run_install "$NOSUM_REL" "$NOSUM_PREFIX")"; then
    fail "install succeeded with no checksums to verify against"
elif [ -e "$NOSUM_PREFIX/bin/agent-harness" ]; then
    fail "an unverified archive still installed a binary"
fi
echo -e "${GREEN}PASS${NC}"

# --- a partial archive is refused before it half-installs -------------------
#
# A release that dropped a tool must fail at install, not later as an opaque
# "cannot find the loop executable" the first time the operator starts a daemon.
echo -n "install.sh refuses an archive that is missing a stage binary ... "
PARTIAL_REL="$TEST_DIR/partial"
PARTIAL_PREFIX="$TEST_DIR/partial-prefix"
make_release "$PARTIAL_REL" "loop"
if partial_out="$(run_install "$PARTIAL_REL" "$PARTIAL_PREFIX")"; then
    fail "install succeeded on an archive with no loop binary"
elif ! printf '%s' "$partial_out" | grep -q "loop"; then
    fail "the failure does not name the missing tool: $partial_out"
fi
echo -e "${GREEN}PASS${NC}"

# --- a download that 404s explains itself -----------------------------------
echo -n "install.sh fails clearly when the release does not exist ... "
if missing_out="$(AGENT_HARNESS_BASE_URL="file://$TEST_DIR/no-such-release" \
    AGENT_HARNESS_VERSION="$FAKE_VERSION" \
    sh "$INSTALL_SH" --prefix "$TEST_DIR/missing-prefix" 2>&1)"; then
    fail "install succeeded against a release that does not exist"
elif ! printf '%s' "$missing_out" | grep -qi "download"; then
    fail "the failure does not say the download is what failed: $missing_out"
fi
echo -e "${GREEN}PASS${NC}"

# --- uninstall is the exact inverse -----------------------------------------
echo -n "install.sh --uninstall removes everything it installed ... "
sh "$INSTALL_SH" --prefix "$PREFIX" --uninstall >/dev/null || fail "uninstall failed"
[ ! -e "$PREFIX/bin/agent-harness" ] || fail "the dispatcher survived uninstall"
for t in $TOOLS; do
    [ ! -e "$PREFIX/libexec/agent-harness/$t" ] || fail "stage binary $t survived uninstall"
done
[ ! -e "$PREFIX/share/agent-harness/.env.example" ] || fail ".env.example survived uninstall"
echo -e "${GREEN}PASS${NC}"

# --- uninstall leaves the operator's own files alone ------------------------
#
# The prefix is a shared directory (~/.local), so uninstall names its own files
# one by one and never rm -rf's a directory derived from an environment variable.
echo -n "install.sh --uninstall leaves unrelated files in the prefix alone ... "
KEEP_PREFIX="$TEST_DIR/keep-prefix"
make_release "$TEST_DIR/keep-rel"
run_install "$TEST_DIR/keep-rel" "$KEEP_PREFIX" >/dev/null || fail "install failed"
mkdir -p "$KEEP_PREFIX/bin"
echo "mine" >"$KEEP_PREFIX/bin/my-own-tool"
echo "mine" >"$KEEP_PREFIX/share/agent-harness/my-notes.md"
sh "$INSTALL_SH" --prefix "$KEEP_PREFIX" --uninstall >/dev/null || fail "uninstall failed"
[ -f "$KEEP_PREFIX/bin/my-own-tool" ] || fail "uninstall deleted an unrelated binary"
[ -f "$KEEP_PREFIX/share/agent-harness/my-notes.md" ] || fail "uninstall deleted an unrelated file"
echo -e "${GREEN}PASS${NC}"

echo -n "install.sh --uninstall says so plainly when there is nothing installed ... "
empty_out="$(sh "$INSTALL_SH" --prefix "$TEST_DIR/never-installed" --uninstall 2>&1)" ||
    fail "uninstall of a non-install exited non-zero: $empty_out"
printf '%s' "$empty_out" | grep -qi "nothing to remove" ||
    fail "expected a plain 'nothing to remove', got: $empty_out"
echo -e "${GREEN}PASS${NC}"

# --- unit-level: platform detection and checksum parsing --------------------
echo -n "detect_platform names a platform the release workflow builds ... "
detected="$(AGENT_HARNESS_SOURCE_ONLY=1 sh -c ". '$INSTALL_SH'; detect_platform")"
case "$detected" in
darwin_amd64 | darwin_arm64 | linux_amd64 | linux_arm64) echo -e "${GREEN}PASS${NC}" ;;
*) fail "detect_platform produced $detected, which no release asset is named for" ;;
esac

echo -n "verify_checksum matches names with and without the ./ prefix ... "
SUMDIR="$TEST_DIR/sums"
mkdir -p "$SUMDIR"
echo "payload" >"$SUMDIR/thing.tar.gz"
digest="$(sha256_of "$SUMDIR/thing.tar.gz")"
printf '%s  ./thing.tar.gz\n' "$digest" >"$SUMDIR/dotslash.txt"
printf '%s  thing.tar.gz\n' "$digest" >"$SUMDIR/plain.txt"
printf '%s  other.tar.gz\n' "$digest" >"$SUMDIR/absent.txt"
for f in dotslash plain; do
    AGENT_HARNESS_SOURCE_ONLY=1 sh -c \
        ". '$INSTALL_SH'; verify_checksum '$SUMDIR/thing.tar.gz' '$SUMDIR/$f.txt' thing.tar.gz" >/dev/null 2>&1 ||
        fail "verify_checksum rejected a matching $f entry"
done
if AGENT_HARNESS_SOURCE_ONLY=1 sh -c \
    ". '$INSTALL_SH'; verify_checksum '$SUMDIR/thing.tar.gz' '$SUMDIR/absent.txt' thing.tar.gz" >/dev/null 2>&1; then
    fail "verify_checksum passed a file that checksums.txt has no entry for"
fi
echo -e "${GREEN}PASS${NC}"

# --- unsupported hosts fail at the top, not mid-unpack ----------------------
echo -n "install.sh rejects an unsupported platform with a reason ... "
bad_platform_out="$(AGENT_HARNESS_SOURCE_ONLY=1 sh -c "
    . '$INSTALL_SH'
    uname() { case \"\$1\" in -s) echo MINGW64_NT-10.0 ;; -m) echo x86_64 ;; esac; }
    detect_platform" 2>&1 || true)"
printf '%s' "$bad_platform_out" | grep -qi "unsupported OS" ||
    fail "expected an unsupported-OS error, got: $bad_platform_out"
echo -e "${GREEN}PASS${NC}"

# --- bad flags do not silently install somewhere unexpected -----------------
echo -n "install.sh rejects an unknown flag ... "
if flag_out="$(sh "$INSTALL_SH" --into /tmp/whatever 2>&1)"; then
    fail "install accepted an unknown flag"
elif ! printf '%s' "$flag_out" | grep -q "unknown option"; then
    fail "the failure does not name the bad flag: $flag_out"
fi
echo -e "${GREEN}PASS${NC}"

echo
echo -e "${GREEN}All tests passed!${NC}"
