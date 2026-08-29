#!/bin/bash
# Unit tests for extract-standalone.sh.
#
# The full extraction is expensive (a clone, a filter, a build and the whole test
# suite) and is exercised by actually running the script. What is tested here is
# the part that ROTS: the hardcoded list of Consumer-coupled files. Every entry
# must still exist, or the extraction silently stops deleting something and ships
# a herd-coupled file into the public repo; and nothing that SURVIVES may
# reference a file the list deletes, or the extracted repo will not compile.
#
# Also covers the guards that stop a careless invocation clobbering something.
# Must run under macOS's stock bash 3.2.

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

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/extract-standalone.sh"
HARNESS_DIR="$(dirname "$SCRIPT_DIR")"
TEST_DIR=$(mktemp -d)
trap 'rm -rf "$TEST_DIR"' EXIT

echo "Testing extract-standalone.sh..."
echo

# The list is parsed out of the script itself rather than duplicated here: a copy
# would drift and the test would pass while the real list rotted.
coupled="$(sed -n '/^CONSUMER_COUPLED="$/,/^"$/p' "$SCRIPT_PATH" | sed '1d;$d')"

# --- every listed path exists -------------------------------------------------
echo -n "Test 1: every Consumer-coupled path still exists... "
missing=""
for f in $coupled; do
    [ -e "$HARNESS_DIR/$f" ] || missing="$missing $f"
done
if [ -n "$missing" ]; then
    echo -e "${RED}FAIL${NC} - listed but absent:$missing"
    echo "  A renamed or deleted file must be updated in CONSUMER_COUPLED, or the"
    echo "  extraction stops deleting it and ships herd coupling into the public repo."
    exit 1
fi
echo -e "${GREEN}PASS${NC}"

# --- the list is not empty ----------------------------------------------------
echo -n "Test 2: the list is non-empty... "
count="$(printf '%s\n' "$coupled" | grep -c . || true)"
if [ "$count" -lt 5 ]; then
    echo -e "${RED}FAIL${NC} - only $count entries; the list looks truncated"
    exit 1
fi
echo -e "${GREEN}PASS${NC} ($count entries)"

# --- nothing that survives references something the list deletes --------------
#
# The binding case is a Go identifier: a helper defined in a deleted _test.go and
# used by one that stays compiles fine here and fails only in the extracted repo,
# where the deleted file is gone. Check the identifiers those files define.
echo -n "Test 3: surviving Go files define their own helpers... "
deleted_go=""
for f in $coupled; do
    case "$f" in
        *.go) deleted_go="$deleted_go $f" ;;
    esac
done

leaked=""
for f in $deleted_go; do
    # Top-level func names defined in a file the extraction deletes.
    names="$(grep -oE '^func [a-zA-Z_][a-zA-Z0-9_]*' "$HARNESS_DIR/$f" 2>/dev/null | sed 's/^func //' || true)"
    for n in $names; do
        # Ignore test entry points: nothing calls a TestXxx by name.
        case "$n" in
            Test*) continue ;;
        esac
        # Any OTHER file in the same package that calls it would not compile.
        pkgdir="$(dirname "$HARNESS_DIR/$f")"
        for sibling in "$pkgdir"/*.go; do
            [ "$sibling" = "$HARNESS_DIR/$f" ] && continue
            skip=""
            for d in $deleted_go; do
                [ "$sibling" = "$HARNESS_DIR/$d" ] && skip=1
            done
            [ -n "$skip" ] && continue
            if grep -qE "(^|[^a-zA-Z0-9_])$n\(" "$sibling" 2>/dev/null; then
                leaked="$leaked $n(in $(basename "$sibling"))"
            fi
        done
    done
done
if [ -n "$leaked" ]; then
    echo -e "${RED}FAIL${NC} - surviving files call helpers defined in deleted ones:$leaked"
    echo "  Move the helper into a file that survives, or the extracted repo will not build."
    exit 1
fi
echo -e "${GREEN}PASS${NC}"

# --- refuses to overwrite an existing output path ----------------------------
echo -n "Test 4: refuses to overwrite an existing output dir... "
EXISTING="$TEST_DIR/already-here"
mkdir -p "$EXISTING"
if "$SCRIPT_PATH" "$EXISTING" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - overwrote an existing path instead of refusing"
    exit 1
fi
echo -e "${GREEN}PASS${NC}"

# --- fails loud with no arguments --------------------------------------------
echo -n "Test 5: usage error with no output dir... "
if "$SCRIPT_PATH" > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - exited 0 with no arguments"
    exit 1
fi
echo -e "${GREEN}PASS${NC}"

echo
echo -e "${GREEN}All tests passed!${NC}"
