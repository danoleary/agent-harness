#!/bin/bash
# check-ci-runs-check.sh — keep the harness CI gate from drifting out of the
# Makefile `check` target (BEH-462).
#
# The harness CI workflow used to run each `make check` sub-target as its own
# step (`make fmt-check`, `make vet`, `make check-exec`, …) instead of running
# `make check` itself. That step list is a hand-maintained copy of the `check`
# target's prerequisites, and the two silently diverge: a guard wired into
# `check:` but never added to the YAML never runs in CI — exactly the
# CI-invisible-guard bug behind BEH-457/BEH-409.
#
# This guard fails if the workflow re-enumerates any `check` prerequisite as its
# own `make <target>` step, or if it stops invoking `make check` at all. It
# reads the prerequisite list straight from the Makefile, so a sub-target added
# to `check:` is covered with no extra wiring. Wired into `make check` itself, it
# runs in CI for free. It must stay bash-3.2-safe.
#
# The workflow is DISCOVERED, not hardcoded: it is whichever workflow invokes a
# `make` target, so the guard works both while the harness is a subtree of a
# Consumer (the workflow sits two levels up) and once it is extracted to its own
# repo (one level up), with no per-repo edit. A hardcoded filename silently
# vanished on extraction and took the guard with it.
#
# Usage:  check-ci-runs-check.sh [WORKFLOW_YAML] [MAKEFILE]
# Exit 0 = clean, exit 1 = drift found.

set -euo pipefail

if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    NC='\033[0m'
else
    RED=''
    GREEN=''
    YELLOW=''
    NC=''
fi

SELF_DIR="$(cd "$(dirname "$0")" && pwd)"
MAKEFILE="${2:-$SELF_DIR/../Makefile}"

# discover_workflow prints the GATE workflow: the one running `make check`, or —
# when the drift this guard exists for has happened — the one running a `check`
# prerequisite as its own step. Other workflows that merely invoke some `make`
# target (an image build, a smoke check) are not the gate and must not be judged
# as one.
#
# It looks in the GIT ROOT's .github/workflows, because that is the only one
# GitHub actually reads. Walking up to the first `.github` found would, inside a
# Consumer subtree, settle on the harness's own inert copy — the standalone
# repo's workflows, which GitHub never runs there — and quietly stop guarding the
# Consumer's real gate. Anchoring on the git root is correct in both layouts: the
# subtree resolves to the Consumer, the extracted repo to itself.
discover_workflow() {
    prereq_pattern="$1"

    root="$(git -C "$SELF_DIR" rev-parse --show-toplevel 2>/dev/null || true)"
    if [ -z "$root" ]; then
        # Not a git checkout (a test fixture, an exported tree): fall back to the
        # nearest ancestor that has a .github/workflows, starting at the harness
        # root rather than assuming a subtree depth.
        probe="$(cd "$SELF_DIR/.." && pwd)"
        while :; do
            [ -d "$probe/.github/workflows" ] && break
            parent="$(dirname "$probe")"
            [ "$parent" = "$probe" ] && return 1
            probe="$parent"
        done
        root="$probe"
    fi

    dir="$root/.github/workflows"
    [ -d "$dir" ] || return 1

    fallback=""
    for wf in "$dir"/*.yml "$dir"/*.yaml; do
        [ -f "$wf" ] || continue
        if grep -qE '(^|[^A-Za-z0-9_-])make[[:space:]]+check([[:space:]]|$)' "$wf"; then
            printf '%s\n' "$wf"
            return 0
        fi
        if [ -z "$fallback" ] && grep -qE "$prereq_pattern" "$wf"; then
            fallback="$wf"
        fi
    done

    [ -n "$fallback" ] || return 1
    printf '%s\n' "$fallback"
}

if [ ! -f "$MAKEFILE" ]; then
    echo -e "${RED}✗${NC} Makefile not found: $MAKEFILE" >&2
    exit 1
fi

# Prerequisites of the `check` target: the words after `check:` on its rule line.
# The `[[:space:]]` after `check` keeps `check-exec:` etc. from matching.
prereq_line="$(grep -E '^check:[[:space:]]' "$MAKEFILE" | head -n 1 || true)"
if [ -z "$prereq_line" ]; then
    echo -e "${RED}✗${NC} no 'check:' target found in $MAKEFILE" >&2
    exit 1
fi
PREREQS="${prereq_line#check:}"

# A pattern matching any `make <prereq>` step, for discovery's drift-case fallback.
prereq_alt="$(printf '%s' "$PREREQS" | tr -s '[:space:]' '|' | sed -e 's/^|//' -e 's/|$//')"
PREREQ_PATTERN="(^|[^A-Za-z0-9_-])make[[:space:]]+($prereq_alt)([[:space:]]|$)"

WORKFLOW="${1:-$(discover_workflow "$PREREQ_PATTERN" || true)}"

if [ -z "$WORKFLOW" ]; then
    echo -e "${RED}✗${NC} no workflow runs 'make check' or any of its prerequisites — the harness gate would be CI-invisible" >&2
    exit 1
fi


if [ ! -f "$WORKFLOW" ]; then
    echo -e "${RED}✗${NC} workflow not found: $WORKFLOW" >&2
    exit 1
fi
if [ ! -f "$MAKEFILE" ]; then
    echo -e "${RED}✗${NC} Makefile not found: $MAKEFILE" >&2
    exit 1
fi


# Every `make <target>` invocation in the workflow, skipping comment-only lines
# so prose mentioning a target is never flagged. The target token stops at
# whitespace, so `make check-exec` and `make check` are distinct.
RUN_TARGETS=()
while IFS='' read -r line; do
    trimmed="${line#"${line%%[![:space:]]*}"}"
    case "$trimmed" in
        '#'*) continue ;;
    esac
    while IFS='' read -r tgt; do
        [ -n "$tgt" ] && RUN_TARGETS+=("$tgt")
    done < <(printf '%s\n' "$line" | grep -oE 'make[[:space:]]+[A-Za-z0-9_-]+' | sed -E 's/^make[[:space:]]+//')
done < "$WORKFLOW"

ran_check=0
i=0
while [ "$i" -lt "${#RUN_TARGETS[@]}" ]; do
    [ "${RUN_TARGETS[$i]}" = "check" ] && ran_check=1
    i=$((i + 1))
done

VIOLATIONS=()

# A sub-target run as its own step is drift — it belongs to `make check`.
for prereq in $PREREQS; do
    j=0
    while [ "$j" -lt "${#RUN_TARGETS[@]}" ]; do
        if [ "${RUN_TARGETS[$j]}" = "$prereq" ]; then
            VIOLATIONS+=("re-enumerates 'make $prereq' as its own step — it is already a prerequisite of 'make check'")
            break
        fi
        j=$((j + 1))
    done
done

if [ "$ran_check" -eq 0 ]; then
    VIOLATIONS+=("does not run 'make check' — CI must invoke the 'check' target directly so its prerequisites can't drift")
fi

if [ ${#VIOLATIONS[@]} -eq 0 ]; then
    echo -e "${GREEN}✓${NC} CI runs 'make check' directly (no drift from the Makefile)"
    exit 0
fi

echo -e "${RED}✗${NC} $WORKFLOW has drifted from the 'check' target:"
echo
for v in "${VIOLATIONS[@]}"; do
    echo -e "  ${YELLOW}$v${NC}"
done
echo
echo "Run 'make check' (then 'make build') in CI instead of listing its"
echo "sub-targets. The 'check' target is the single source of truth for the gate;"
echo "a sub-target added there must run in CI without editing the workflow."
exit 1
