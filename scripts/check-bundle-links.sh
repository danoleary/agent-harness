#!/bin/bash
# Guard the installed docs' links against files the install doesn't ship (#96, #127).
#
# install.sh copies the README into <prefix>/share/agent-harness/ beside exactly
# the files in its DOCS list, keeping their repo-relative paths. A relative link
# in a shipped doc to anything else — a doc left off the list, an ADR, a
# directory, a GitHub-relative path like ../../releases — works on github.com and
# is broken in every install, which no one on our side ever sees. This guard
# reads DOCS from install.sh (the one list the installer, `make install` and the
# release workflow all copy) and fails when a relative link in any shipped .md,
# resolved from that doc's own directory:
#
#   - resolves to a file that isn't in DOCS;
#   - names a directory (the bundle ships files, not trees);
#   - climbs out of the bundle root.
#
# It also fails when a doc in DOCS is missing from the tree, since the release
# would fail to copy it. URLs (any scheme), in-page #anchors and links inside
# fenced code blocks are not checked. It runs under `make check`, and is itself
# bash-3.2-safe (runs under macOS's stock /bin/bash).
#
# Usage:
#   check-bundle-links.sh [ROOT]   # ROOT defaults to the current directory

set -euo pipefail

ROOT="$(cd "${1:-.}" && pwd)"

# The shipped docs, one per line, straight from install.sh — sourced, not
# grepped, so the list this checks is the list the installer acts on.
docs=$(AGENT_HARNESS_SOURCE_ONLY=1 sh -c '. "$1" && printf "%s\n" $DOCS' sh "$ROOT/install.sh")
[ -n "$docs" ] || { echo "install.sh: DOCS is empty or unset — cannot tell what the bundle ships"; exit 1; }

violations=0

# A here-string, not `printf | grep -q`: grep -q exits on its first match, and
# under pipefail the writer's SIGPIPE read as "not shipped" (#177).
shipped() {
    grep -qxF -- "$1" <<< "$docs"
}

# 1. Every shipped doc exists.
while IFS= read -r doc; do
    if [ ! -f "$ROOT/$doc" ]; then
        echo "install.sh: DOCS lists ${doc}, which is not in the tree"
        violations=$((violations + 1))
    fi
done <<EOF
$docs
EOF

# links FILE — "<line>\t<target>" for every inline link and reference-style link
# definition outside fenced code blocks.
links() {
    awk '
        /^[ \t]*(```|~~~)/ { fenced = !fenced; next }
        fenced { next }
        {
            rest = $0
            while (match(rest, /\]\([^)]*\)/)) {
                print NR "\t" substr(rest, RSTART + 2, RLENGTH - 3)
                rest = substr(rest, RSTART + RLENGTH)
            }
            if (match($0, /^ ? ? ?\[[^]]+\]:[ \t]*[^ \t]+/)) {
                def = substr($0, RSTART, RLENGTH)
                sub(/^ ? ? ?\[[^]]+\]:[ \t]*/, "", def)
                print NR "\t" def
            }
        }
    ' "$1"
}

# normalize PATH — fold ./ and ../ segments; prints nothing when PATH climbs out
# of the bundle root. A trailing slash is kept, so a directory stays visible.
normalize() {
    printf '%s\n' "$1" | awk -F/ '
        {
            n = 0
            for (i = 1; i <= NF; i++) {
                if ($i == "" || $i == ".") continue
                if ($i == "..") { if (n == 0) exit; n--; continue }
                seg[++n] = $i
            }
            out = ""
            for (i = 1; i <= n; i++) out = out (i > 1 ? "/" : "") seg[i]
            if ($0 ~ /\/$/ && out != "") out = out "/"
            print out
        }'
}

# 2. Every relative link in a shipped .md resolves to a shipped doc.
tab=$(printf '\t')
while IFS= read -r doc; do
    case "$doc" in
        *.md) ;;
        *) continue ;;
    esac
    [ -f "$ROOT/$doc" ] || continue
    # A link resolves from the doc's own directory: "" for a root doc.
    dir=""
    case "$doc" in
        */*) dir="${doc%/*}/" ;;
    esac
    while IFS="$tab" read -r line_no target; do
        # <dest> and [text](dest "title") forms: the destination is the first token.
        target="${target#<}"
        target="${target%%>*}"
        target="${target%%[ $tab]*}"
        case "$target" in
            "" | "#"*) continue ;;
        esac
        # Any URL scheme (https:, mailto:, …) points outside the bundle on purpose.
        if grep -qE '^[A-Za-z][A-Za-z0-9+.-]*:' <<< "$target"; then
            continue
        fi

        path="${target%%#*}"
        path="${path%%\?*}"
        resolved=$(normalize "$dir$path")
        if [ -z "$resolved" ]; then
            echo "${doc}:${line_no}: link to ${target} climbs out of the installed bundle"
        elif [ "${resolved%/}" != "$resolved" ] || [ -d "$ROOT/$resolved" ]; then
            echo "${doc}:${line_no}: link to ${target} names a directory; the bundle ships files only"
        elif ! shipped "$resolved"; then
            echo "${doc}:${line_no}: link to ${target} names ${resolved}, which install.sh does not ship"
        else
            continue
        fi
        violations=$((violations + 1))
    done < <(links "$ROOT/$doc")
done <<EOF
$docs
EOF

if [ "$violations" -gt 0 ]; then
    echo
    echo "Found ${violations} bundle link problem(s). Add the doc to DOCS in install.sh, or"
    echo "link it by its https://github.com/... URL if it is not meant to be installed."
    exit 1
fi

echo "✓ every relative link in a shipped doc resolves to a doc install.sh ships"
