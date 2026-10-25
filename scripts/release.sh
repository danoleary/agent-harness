#!/usr/bin/env bash
# Cut a harness release in one command: bump the base-image pins (and optionally
# the sandbox claude CLI), gate, commit, tag, push, and wait for the release
# workflow to publish the binaries and the base image.
#
# The tag is what publishes — .github/workflows/release.yml builds the binaries
# and Dockerfile.base on a `v*` push. This script only gets main into the right
# state first and tags it, so the commit carrying `FROM …-base:X.Y.Z` IS the one
# released as vX.Y.Z (the harness and its base are a matched pair, ADR-0007).
# The pins are read at session time, not by the release build, so tagging the
# bump is safe; the only gap is the few minutes before the new base lands on
# GHCR, and a failed release run leaves main pinned to an unpublished tag — the
# script says so and how to recover. Must stay bash-3.2-safe (macOS stock).
#
# Usage:
#   release.sh <vX.Y.Z | X.Y.Z | patch | minor | major> [--claude-version V] [--dry-run] [--no-wait]
#
# Env overrides (used by the test): RELEASE_POLL_INTERVAL, RELEASE_POLL_TRIES.
set -euo pipefail

usage() {
	sed -n 's/^#   //p' "$0" | head -1 | sed 's/^/usage: /' >&2
	exit 2
}

die() {
	printf 'release: %s\n' "$*" >&2
	exit 1
}

# Files that pin the published base image. Missing ones are skipped, so a
# Consumer subtree without example/ still works. The scaffold template is the
# Dockerfile `agent-harness init` writes, so a release's init pins its own base.
PIN_FILES=".agent-harness/Dockerfile example/.agent-harness/Dockerfile docs/CONSUMER.md internal/scaffold/templates/Dockerfile.tmpl"
BASE_REF='ghcr\.io/[A-Za-z0-9._-]+/agent-harness-base'

want=""
claude_version=""
dry_run=0
wait_run=1
while [ $# -gt 0 ]; do
	case "$1" in
	--claude-version)
		[ $# -ge 2 ] || usage
		claude_version="$2"
		shift 2
		;;
	--dry-run) dry_run=1; shift ;;
	--no-wait) wait_run=0; shift ;;
	-h | --help) usage ;;
	-*) die "unknown flag: $1" ;;
	*)
		[ -z "$want" ] || usage
		want="$1"
		shift
		;;
	esac
done
[ -n "$want" ] || usage

root="$(git rev-parse --show-toplevel)"
cd "$root"

# --- Preflight: a clean main that matches origin, and a gh that can see it.
branch="$(git rev-parse --abbrev-ref HEAD)"
[ "$branch" = "main" ] || die "on '$branch' — releases are cut from main"
[ -z "$(git status --porcelain)" ] || die "working tree is dirty — commit or stash first"
git fetch --quiet --tags origin main
[ "$(git rev-parse HEAD)" = "$(git rev-parse origin/main)" ] ||
	die "main is not at origin/main — pull (or push) first"
gh auth status >/dev/null 2>&1 || die "gh is not authenticated — run: gh auth login"

# --- Resolve the version against the latest vX.Y.Z tag.
latest="$(git tag --list 'v[0-9]*.[0-9]*.[0-9]*' --sort=-v:refname | head -1)"
latest="${latest:-v0.0.0}"
IFS=. read -r lmaj lmin lpat <<EOF
${latest#v}
EOF
case "$want" in
patch) version="v$lmaj.$lmin.$((lpat + 1))" ;;
minor) version="v$lmaj.$((lmin + 1)).0" ;;
major) version="v$((lmaj + 1)).0.0" ;;
v[0-9]* | [0-9]*) version="v${want#v}" ;;
*) die "version must be vX.Y.Z, patch, minor or major — got '$want'" ;;
esac
printf '%s\n' "$version" | grep -qE '^v[0-9]+\.[0-9]+\.[0-9]+$' || die "not a vX.Y.Z version: $version"
git rev-parse -q --verify "refs/tags/$version" >/dev/null && die "tag $version already exists"

IFS=. read -r nmaj nmin npat <<EOF
${version#v}
EOF
if [ "$nmaj" -lt "$lmaj" ] ||
	{ [ "$nmaj" -eq "$lmaj" ] && [ "$nmin" -lt "$lmin" ]; } ||
	{ [ "$nmaj" -eq "$lmaj" ] && [ "$nmin" -eq "$lmin" ] && [ "$npat" -le "$lpat" ]; }; then
	die "$version is not newer than the latest release $latest"
fi
semver="${version#v}"

printf 'Releasing %s (latest is %s)\n' "$version" "$latest"

# --- Edit: base-image pins, and the claude CLI if asked. Rewrite via temp + mv
# rather than `sed -i`, whose flags differ between BSD and GNU sed.
rewrite() { # file sed-expr
	sed -E "$2" "$1" >"$1.tmp" && mv -f "$1.tmp" "$1"
}
for f in $PIN_FILES; do
	[ -f "$f" ] || continue
	rewrite "$f" "s#(${BASE_REF}):[0-9]+\.[0-9]+\.[0-9]+#\1:${semver}#g"
done
if [ -n "$claude_version" ]; then
	grep -q '^ARG CLAUDE_VERSION=' Dockerfile.base || die "no ARG CLAUDE_VERSION in Dockerfile.base"
	rewrite Dockerfile.base "s#^ARG CLAUDE_VERSION=.*#ARG CLAUDE_VERSION=${claude_version}#"
fi

if [ "$dry_run" -eq 1 ]; then
	git --no-pager diff
	git checkout -- .
	printf '\ndry run: would gate, commit, tag %s and push. Nothing changed.\n' "$version"
	exit 0
fi

# --- Gate the exact tree that will be tagged.
make check || {
	git checkout -- .
	die "make check failed — nothing committed or tagged"
}

if [ -n "$(git status --porcelain)" ]; then
	msg="Release $version"
	[ -z "$claude_version" ] || msg="$msg

Bump the sandbox claude CLI to $claude_version."
	git commit --quiet -am "$msg"
fi
git tag -a "$version" -m "$version"
# --atomic: main and the tag land together or not at all, so a rejected push
# never leaves a tag on a commit main doesn't have.
git push --atomic origin main "refs/tags/$version"
printf 'pushed main and %s\n' "$version"

if [ "$wait_run" -eq 0 ]; then
	printf 'not waiting — follow it with: gh run list --workflow release.yml --branch %s\n' "$version"
	exit 0
fi

# --- Wait for the release workflow the tag push triggered.
interval="${RELEASE_POLL_INTERVAL:-5}"
tries="${RELEASE_POLL_TRIES:-24}"
run_id=""
i=0
while [ "$i" -lt "$tries" ]; do
	run_id="$(gh run list --workflow release.yml --branch "$version" --limit 1 \
		--json databaseId --jq '.[0].databaseId // empty' 2>/dev/null || true)"
	[ -z "$run_id" ] || break
	sleep "$interval"
	i=$((i + 1))
done
[ -n "$run_id" ] || die "no release run appeared for $version — check the Actions tab"

if ! gh run watch "$run_id" --exit-status; then
	printf '\nrelease: the release run failed. main now pins base %s, which may not be on GHCR.\n' "$semver" >&2
	printf 'Fix the cause, then re-run it:  gh workflow run release.yml -f tag=%s\n' "$version" >&2
	exit 1
fi

printf '\nReleased %s\n' "$version"
gh release view "$version" --json url --jq .url
