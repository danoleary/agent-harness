#!/bin/bash
# check-pipefail-grep.sh — guard against `producer | grep -q` in scripts that
# set pipefail (#273).
#
# grep -q exits on its first match. If the producer is still writing, it takes
# SIGPIPE, pipefail makes the pipeline exit 141, and the `if` reads a match as
# a miss. Whether it fires depends on timing, so it is a flake, not a failure:
# #177 hit a guard this way, and #273 hit the script tests' expect_fail. Match a
# here-string instead (`grep -q x <<< "$var"`, bash 3.2-safe), or drop -q and
# redirect to /dev/null so grep reads all its input.
#
# Scope: scripts/*.sh, scripts/lib/*.sh and the repo-root *.sh that set
# pipefail. A per-line text scan: full-line comments are skipped, and this guard
# and its own test are skipped (they name the pattern in prose and fixtures).
# It must itself stay bash-3.2-safe.
#
# Exit 0 = clean, exit 1 = violations found.

set -euo pipefail

BASE_DIR="${1:-.}"

# A single `|` (not `||`), then grep with -q/--quiet/--silent among the options
# that directly follow it.
PATTERN='(^|[^|])\|[[:space:]]*grep([[:space:]]+-[^[:space:]]*)*[[:space:]]+(-[A-Za-z]*q[A-Za-z]*|--quiet|--silent)([[:space:]]|$)'

violations=0

for file in "$BASE_DIR"/scripts/*.sh "$BASE_DIR"/scripts/lib/*.sh "$BASE_DIR"/*.sh; do
    [ -f "$file" ] || continue
    case "$file" in
        *check-pipefail-grep.sh) continue ;;
    esac
    grep -qE '^[[:space:]]*set[[:space:]].*pipefail' "$file" || continue

    while IFS='' read -r match; do
        line_num="${match%%:*}"
        line_content="${match#*:}"
        trimmed="${line_content#"${line_content%%[![:space:]]*}"}"
        case "$trimmed" in
            '#'*) continue ;;
        esac
        echo "${file#"$BASE_DIR"/}:${line_num}: pipe into grep -q under pipefail (grep quits on a match, the writer takes SIGPIPE, and the match reads as a miss): use a here-string, grep -q PATTERN <<< \"\$var\""
        violations=$((violations + 1))
    done < <(grep -nE "$PATTERN" "$file" || true)
done

if [ "$violations" -gt 0 ]; then
    echo
    echo "Found $violations pipe(s) into grep -q in pipefail scripts (#273)."
    exit 1
fi
echo "No pipes into grep -q in pipefail scripts."
