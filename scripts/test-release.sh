#!/bin/bash
# Unit tests for release.sh.
#
# Drives the release script against a throwaway clone of a bare "origin", with
# FAKE make and gh on PATH, so nothing is gated, pushed to GitHub, or watched for
# real. Asserts the preflight refusals, version resolution, the pin + claude CLI
# rewrites, that a failed gate commits and tags nothing, and that a good run
# lands one release commit and its tag on origin together. Like the other
# harness scripts it must run under macOS's stock bash 3.2.

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

SCRIPT_PATH="$(cd "$(dirname "$0")" && pwd)/release.sh"
TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TEST_DIR"' EXIT

# Fakes: make passes unless FAKE_MAKE_FAIL=1; gh answers just the calls release.sh
# makes, and `run watch` exits FAKE_WATCH_RC.
FAKE_BIN="$TEST_DIR/bin"
mkdir -p "$FAKE_BIN"
cat >"$FAKE_BIN/make" <<'EOF'
#!/bin/sh
[ "${FAKE_MAKE_FAIL:-0}" = 1 ] && exit 1
exit 0
EOF
cat >"$FAKE_BIN/gh" <<'EOF'
#!/bin/sh
case "$1 $2" in
"auth status") exit 0 ;;
"run list") echo 4242 ;;
"run watch") exit "${FAKE_WATCH_RC:-0}" ;;
"release view") echo "https://example.invalid/release" ;;
esac
EOF
chmod +x "$FAKE_BIN/make" "$FAKE_BIN/gh"
export PATH="$FAKE_BIN:$PATH"
export RELEASE_POLL_INTERVAL=0 RELEASE_POLL_TRIES=1

# A fresh origin + clone at v0.5.0 with every file release.sh rewrites.
fixture() { # name -> prints the clone path
    local origin="$TEST_DIR/$1.git" clone="$TEST_DIR/$1"
    git init --quiet --bare -b main "$origin"
    git init --quiet -b main "$clone"
    (
        cd "$clone"
        git config user.name test
        git config user.email test@example.invalid
        git config commit.gpgsign false
        git config tag.gpgsign false
        mkdir -p .agent-harness docs/how-to internal/scaffold/templates
        echo 'FROM ghcr.io/danoleary/agent-harness-base:0.5.0' >.agent-harness/Dockerfile
        printf '# << .Label >>\nFROM ghcr.io/danoleary/agent-harness-base:0.5.0\n' >internal/scaffold/templates/Dockerfile.tmpl
        printf 'text\nFROM ghcr.io/danoleary/agent-harness-base:0.1.0\n' >docs/how-to/sandbox-image.md
        printf 'FROM node:24\nARG CLAUDE_VERSION=2.1.283\n' >Dockerfile.base
        git add -A
        git commit --quiet -m init
        git tag -a v0.5.0 -m v0.5.0
        git remote add origin "$origin"
        git push --quiet origin main v0.5.0
    )
    echo "$clone"
}

pass() { echo -e "${GREEN}PASS${NC}"; }
fail() { echo -e "${RED}FAIL${NC} - $*"; exit 1; }

echo "Testing release.sh..."
echo

echo -n "Test 1: refuses a dirty working tree... "
c=$(fixture t1)
echo junk >"$c/junk"
out=$(cd "$c" && "$SCRIPT_PATH" minor 2>&1) && fail "expected failure"
echo "$out" | grep -q 'dirty' && pass || fail "got: $out"

echo -n "Test 2: refuses a branch other than main... "
c=$(fixture t2)
git -C "$c" checkout --quiet -b feature
out=$(cd "$c" && "$SCRIPT_PATH" minor 2>&1) && fail "expected failure"
echo "$out" | grep -q 'cut from main' && pass || fail "got: $out"

echo -n "Test 3: refuses a version not newer than the latest tag... "
c=$(fixture t3)
out=$(cd "$c" && "$SCRIPT_PATH" 0.4.9 2>&1) && fail "expected failure"
echo "$out" | grep -q 'not newer' && pass || fail "got: $out"

echo -n "Test 4: --dry-run resolves 'minor' and changes nothing... "
c=$(fixture t4)
out=$(cd "$c" && "$SCRIPT_PATH" minor --dry-run 2>&1) || fail "dry run failed: $out"
echo "$out" | grep -q 'v0.6.0' || fail "expected v0.6.0 in: $out"
[ -z "$(git -C "$c" status --porcelain)" ] || fail "dry run left changes"
git -C "$c" rev-parse -q --verify refs/tags/v0.6.0 >/dev/null && fail "dry run tagged"
pass

echo -n "Test 5: a failed gate commits and tags nothing... "
c=$(fixture t5)
head_before=$(git -C "$c" rev-parse HEAD)
out=$(cd "$c" && FAKE_MAKE_FAIL=1 "$SCRIPT_PATH" patch 2>&1) && fail "expected failure"
[ "$(git -C "$c" rev-parse HEAD)" = "$head_before" ] || fail "committed despite failed gate"
[ -z "$(git -C "$c" status --porcelain)" ] || fail "left edits behind"
git -C "$c" rev-parse -q --verify refs/tags/v0.5.1 >/dev/null && fail "tagged despite failed gate"
pass

echo -n "Test 6: a release bumps pins + CLI and pushes commit and tag together... "
c=$(fixture t6)
out=$(cd "$c" && "$SCRIPT_PATH" minor --claude-version 2.2.0 2>&1) || fail "release failed: $out"
o="$TEST_DIR/t6.git"
git --git-dir="$o" rev-parse -q --verify refs/tags/v0.6.0 >/dev/null || fail "tag not on origin"
[ "$(git --git-dir="$o" rev-parse 'v0.6.0^{commit}')" = "$(git --git-dir="$o" rev-parse main)" ] ||
    fail "tag is not on origin's main tip"
show() { git --git-dir="$o" show "main:$1"; }
show .agent-harness/Dockerfile | grep -q 'agent-harness-base:0.6.0' || fail "dogfood pin not bumped"
show docs/how-to/sandbox-image.md | grep -q 'agent-harness-base:0.6.0' || fail "sandbox-image how-to pin not bumped"
show internal/scaffold/templates/Dockerfile.tmpl | grep -q 'agent-harness-base:0.6.0' || fail "init's scaffold pin not bumped"
show Dockerfile.base | grep -q '^ARG CLAUDE_VERSION=2.2.0$' || fail "claude CLI not bumped"
git --git-dir="$o" log -1 --format=%s main | grep -q '^Release v0.6.0$' || fail "unexpected commit subject"
pass

echo -n "Test 7: a failed release run is reported with the recovery command... "
c=$(fixture t7)
out=$(cd "$c" && FAKE_WATCH_RC=1 "$SCRIPT_PATH" patch 2>&1) && fail "expected failure"
echo "$out" | grep -q 'gh workflow run release.yml -f tag=v0.5.1' && pass || fail "got: $out"

echo
echo -e "${GREEN}All release.sh tests passed${NC}"
