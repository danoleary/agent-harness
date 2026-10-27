#!/bin/bash
# Guard ADR numbering against parallel tickets (#92).
#
# Two tickets in flight at once each take the next free ADR number they can see
# on their base. Their slugs differ, so the two files never conflict in git, a
# clean rebase lands both, and every ADR-NNNN cross-reference the second ticket
# wrote now points at the wrong decision. This guard fails the gate when:
#
#   - two files in docs/adr/ share a numeric prefix;
#   - an ADR-NNNN reference in a Go comment or a Markdown file names a number
#     with no file in docs/adr/;
#   - a Markdown link to adr/NNNN-<slug>.md names a file that doesn't exist;
#   - the index, docs/adr/README.md, is not exactly the ADR files, one
#     `- [ADR-NNNN](NNNN-<slug>.md)` line each, in ascending order (#146).
#
# It runs under `make check`, so both the host-side Gate re-run and CI catch a
# collision even after a clean rebase. But each sees one branch: two open PRs
# that took the same number both pass, and the second to merge leaves main red.
# The index closes that gap (ADR-0034): a new ADR appends its line after the
# last one, so two branches taking the same number edit the same spot of one
# file, and the second PR conflicts with main instead of merging cleanly. Go string literals are not scanned: they
# are test data, not cross-references. Linked worktrees under .claude/worktrees
# are other branches' trees and are skipped.
#
# It is itself bash-3.2-safe (runs under macOS's stock /bin/bash).

set -euo pipefail

ROOT="${1:-.}"
ADR_DIR="$ROOT/docs/adr"

violations=0

# Every scanned source file, NUL-free paths one per line.
sources() {
    find "$ROOT" \( -path "$ROOT/.git" -o -path "$ROOT/.claude/worktrees" -o -name node_modules \) -prune \
        -o -type f \( -name '*.go' -o -name '*.md' \) -print
}

# adr_exists NNNN — is there a docs/adr/NNNN-*.md?
adr_exists() {
    local f
    for f in "$ADR_DIR/$1"-*.md; do
        [ -e "$f" ] && return 0
    done
    return 1
}

# 1. Duplicate numbers.
if [ -d "$ADR_DIR" ]; then
    dups=$(find "$ADR_DIR" -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]-*.md' -exec basename {} \; |
        cut -c1-4 | sort | uniq -d)
    for n in $dups; do
        files=$(find "$ADR_DIR" -maxdepth 1 -type f -name "$n-*.md" -exec basename {} \; | sort | tr '\n' ' ')
        echo "docs/adr: ADR-$n is taken by more than one file: ${files% }"
        violations=$((violations + 1))
    done
fi

# 2 + 3. Dangling references and links.
while IFS= read -r file; do
    rel="${file#"$ROOT"/}"
    case "$file" in
        # Keep everything after the FIRST `//`: a greedy match would cut at a URL's
        # `://` and drop the reference before it.
        *.go) text=$(grep -n '//' "$file" | sed -E 's#^([0-9]+:)([^/]|/[^/])*//#\1#' || true) ;;
        *) text=$(grep -n '' "$file" || true) ;;
    esac
    [ -n "$text" ] || continue

    while IFS=: read -r line_no n; do
        if ! adr_exists "$n"; then
            echo "${rel}:${line_no}: ADR-$n names no file in docs/adr/"
            violations=$((violations + 1))
        fi
    done < <(printf '%s\n' "$text" | grep -oE '^[0-9]+:|ADR-[0-9]{4}' |
        awk '/^[0-9]+:$/ { line = $0; next } { sub(/^ADR-/, ""); print line $0 }')

    case "$file" in
        *.md) ;;
        *) continue ;;
    esac
    while IFS=: read -r line_no link; do
        if [ ! -e "$ADR_DIR/${link#adr/}" ]; then
            echo "${rel}:${line_no}: link to ${link} names no file in docs/adr/"
            violations=$((violations + 1))
        fi
    done < <(printf '%s\n' "$text" | grep -oE '^[0-9]+:|adr/[0-9]{4}-[A-Za-z0-9._-]*\.md' |
        awk '/^[0-9]+:$/ { line = $0; next } { print line $0 }')
done < <(sources)

# 4. The index.
INDEX="$ADR_DIR/README.md"
adrs=""
if [ -d "$ADR_DIR" ]; then
    adrs=$(find "$ADR_DIR" -maxdepth 1 -type f -name '[0-9][0-9][0-9][0-9]-*.md' -exec basename {} \; | sort)
fi
if [ -n "$adrs" ] && [ ! -f "$INDEX" ]; then
    echo "docs/adr/README.md: missing; it must list every ADR, one '- [ADR-NNNN](NNNN-<slug>.md)' line each, in ascending order"
    violations=$((violations + 1))
elif [ -n "$adrs" ]; then
    indexed=""
    prev=0
    while read -r line_no n file; do
        if [ ! -e "$ADR_DIR/$file" ]; then
            echo "docs/adr/README.md:${line_no}: index entry ${file} names no file in docs/adr/"
            violations=$((violations + 1))
        fi
        if [ "${file%%-*}" != "$n" ]; then
            echo "docs/adr/README.md:${line_no}: ADR-$n is linked to ${file}"
            violations=$((violations + 1))
        fi
        if [ $((10#${file%%-*})) -le "$prev" ]; then
            echo "docs/adr/README.md:${line_no}: ${file} is out of order; the index lists ADRs in ascending order, a new one last"
            violations=$((violations + 1))
        fi
        prev=$((10#${file%%-*}))
        indexed="$indexed$file
"
    done < <(grep -nE '^- \[ADR-[0-9]{4}\]\([0-9]{4}-[^)]*\.md\)' "$INDEX" |
        sed -E 's/^([0-9]+):- \[ADR-([0-9]{4})\]\(([^)]*)\).*/\1 \2 \3/')
    for f in $adrs; do
        if ! printf '%s' "$indexed" | grep -qxF "$f"; then
            echo "docs/adr/README.md: ${f} is not listed; append a '- [ADR-${f%%-*}](${f})' line for it"
            violations=$((violations + 1))
        fi
    done
fi

if [ "$violations" -gt 0 ]; then
    echo
    echo "Found ${violations} ADR numbering problem(s). Number a new ADR against origin/main's"
    echo "docs/adr/, not local main: a sibling branch may have taken the same number."
    exit 1
fi

echo "✓ ADR numbers are unique, indexed in order, and every ADR reference resolves"
