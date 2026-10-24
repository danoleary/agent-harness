#!/bin/bash
# Probe a built sandbox image for its toolchain the way a Gate sees it (#40).
#
# The harness runs post_create and every Gate as `bash -lc <command>` through the
# base image's entrypoint. A login shell sources /etc/profile, which RESETS PATH,
# so a toolchain put on PATH only by a Dockerfile `ENV PATH=...` is visible to the
# in-session shell and invisible to the Gate: `make check` exits 127 host-side on
# a branch that passed it in-session. This probe runs the image exactly that way
# and fails, naming each tool, if one does not resolve — so the regression is
# caught when the image changes, not at a ticket's push gate.
#
# Needs a docker daemon and a built image, so it stays OUT of `make check` (which
# is offline-runnable); CI runs it after building the image. bash-3.2-safe.
#
# Usage:
#   check-sandbox-image.sh IMAGE [TOOL...]   # TOOL defaults to: go gofmt
# DOCKER overrides the docker binary (the tests pass a fake).

set -euo pipefail

DOCKER="${DOCKER:-docker}"

if [ $# -lt 1 ]; then
    echo "usage: $0 IMAGE [TOOL...]" >&2
    exit 2
fi
image="$1"
shift
if [ $# -eq 0 ]; then
    set -- go gofmt
fi

# The tool names are spliced into the in-container script, so keep them to plain
# command names.
for t in "$@"; do
    case "$t" in
    "" | *[!A-Za-z0-9._+-]*)
        echo "not a command name: '$t'" >&2
        exit 2
        ;;
    esac
done

probe=""
for t in "$@"; do
    probe="${probe}command -v $t >/dev/null || echo \"missing $t\"; "
done

# PROJECT_PATH is what the entrypoint stats to pick the runtime uid; a root-owned
# /tmp makes it drop to the image's node user, so the probe runs unprivileged
# like a real session does.
if ! out="$("$DOCKER" run --rm -e PROJECT_PATH=/tmp "$image" bash -lc "$probe")"; then
    echo "could not run $image to probe it" >&2
    exit 1
fi

missing=""
while IFS= read -r line; do
    case "$line" in
    "missing "*) missing="$missing ${line#missing }" ;;
    esac
done <<EOF
$out
EOF

if [ -n "$missing" ]; then
    for t in $missing; do
        echo "missing from a login shell's PATH in $image: $t"
    done
    echo
    echo "A Gate runs \`bash -lc\`, whose /etc/profile resets PATH: put the toolchain"
    echo "on PATH in /etc/profile.d/ (or symlink it into /usr/local/bin), not only via ENV."
    exit 1
fi

echo "✓ $image: $* on a login shell's PATH"
