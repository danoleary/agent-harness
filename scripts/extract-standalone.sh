#!/bin/bash
# Produce the standalone agent-harness repo from this herd subtree (ADR-0007).
#
# The extraction is a script, not a one-off session, so it is reviewable, and so
# it can be re-run after another round of generalize-in-place work without anyone
# having to remember what was done by hand the first time.
#
# Two phases:
#   1. `git filter-repo --subdirectory-filter agent-harness` — lifts this subtree
#      to the repo root with its history intact, so the ADRs, DESIGN.md and the
#      per-commit rationale survive.
#   2. Delete the Consumer-coupled files. Everything left in herd that reads
#      herd's checkout — its Dockerfile, its committed-config smoke tests, its
#      prompt-body assertions, its image pins — lives in a file of its OWN for
#      exactly this reason, so this phase is pure deletion and never text surgery.
#
# Usage:
#   scripts/extract-standalone.sh <output-dir> [source-ref]
#
# It never pushes and never touches the source checkout: it clones, so a failed
# run costs nothing but the output directory.
set -euo pipefail

usage() {
	echo "usage: $0 <output-dir> [source-ref]" >&2
	exit 2
}

[ $# -ge 1 ] || usage
OUT="$1"
REF="${2:-HEAD}"

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
HARNESS_DIR="$(dirname "$SCRIPT_DIR")"
SRC="$(cd "$HARNESS_DIR/.." && pwd)"
SUBTREE="$(basename "$HARNESS_DIR")"

command -v git-filter-repo >/dev/null 2>&1 || {
	echo "git-filter-repo not found — install it (brew install git-filter-repo)" >&2
	exit 1
}

[ -e "$OUT" ] && {
	echo "refusing to overwrite existing path: $OUT" >&2
	exit 1
}

# Consumer-coupled files, deleted after the filter. Each is herd's, not the
# harness's: it reads a path outside this module (herd's web/, .claude/skills/,
# .github/workflows/) and cannot run in a repo where that path does not exist.
# herd re-expresses these as its own guards when it drops the subtree.
CONSUMER_COUPLED="
Dockerfile
internal/config/herd_committed_test.go
internal/prompt/herd_body_test.go
internal/git/docsonly_crosscheck_test.go
internal/sandbox/playwright_version_test.go
internal/sandbox/pnpm_store_test.go
internal/sandbox/pnpm_store_smoke.go
internal/sandbox/pnpm_store_smoke_test.go
internal/sandbox/go_toolchain_test.go
internal/skills
"

echo "==> cloning $SRC@$REF"
git clone --no-local --quiet "$SRC" "$OUT"
cd "$OUT"
git checkout --quiet "$REF"

echo "==> filtering to the $SUBTREE subtree"
git filter-repo --subdirectory-filter "$SUBTREE" --force >/dev/null

# The clone carries every herd branch, each rewritten by the filter. Only the
# extracted line is wanted, under the name the new repo's default branch will use.
echo "==> reducing to a single main branch"
current="$(git branch --show-current)"
git branch | sed 's/^[* ]*//' | while IFS= read -r b; do
	[ "$b" = "$current" ] || git branch -D "$b" >/dev/null
done
[ "$current" = "main" ] || git branch -m main

echo "==> removing Consumer-coupled files"
for f in $CONSUMER_COUPLED; do
	if [ -e "$f" ]; then
		git rm -r --quiet "$f"
		echo "    - $f"
	else
		echo "    ! missing (already gone?): $f" >&2
	fi
done

# This script describes the Consumer's subtree; it is meaningless once extracted.
git rm --quiet "scripts/$(basename "$0")" "scripts/test-$(basename "$0")"

# The Makefile is SHARED, so its Consumer-only targets cannot be removed by
# deleting a file. They are fenced by sentinels instead, so this stays a
# delete-between-two-markers edit rather than a pattern match on target names.
echo "==> stripping the Makefile's consumer-only block"
grep -q '^# >>> consumer-only' Makefile || {
	echo "Makefile has no '# >>> consumer-only' sentinel — refusing to guess what to strip" >&2
	exit 1
}
grep -q '^# <<< consumer-only' Makefile || {
	echo "Makefile has no '# <<< consumer-only' sentinel — refusing to guess what to strip" >&2
	exit 1
}
sed -i.bak '/^# >>> consumer-only/,/^# <<< consumer-only/d' Makefile
rm -f Makefile.bak
sed -i.bak 's/^\.PHONY: build base-image image smoke test/.PHONY: build base-image test/' Makefile
rm -f Makefile.bak

echo "==> verifying the extracted repo"
# The FULL gate, not just build+test: the guards are where a Consumer coupling
# hides (a hardcoded workflow path, a Makefile target pointing at a deleted
# Dockerfile), and those pass a bare `go test` while being broken here.
make check
make build >/dev/null
rm -rf bin

echo "==> committing the de-herd"
git add -A
git -c user.name="Agent Harness" -c user.email="agent-harness@users.noreply.github.com" \
	commit --quiet -m "Extract from the herd subtree into a standalone repo

Drops the files that read a Consumer's checkout and so cannot run here: herd's
sandbox Dockerfile (its toolchain layer now FROMs the published base), its
committed-config and prompt-body smoke tests, its image version pins, and the
skill-contract tests over its .claude/skills tree. herd re-expresses each as its
own guard.

What stays is the harness and its contract: the base image, the envelope, the
tracker port, the gates, and the worked example in example/."

echo
echo "extracted $(git rev-list --count HEAD) commits to $OUT"
echo "next: add .github/workflows, then create the public repo and push."
