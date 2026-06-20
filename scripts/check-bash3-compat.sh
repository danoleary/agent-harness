#!/bin/bash
# check-bash3-compat.sh — guard against bash-4-only syntax in bash scripts (BEH-457).
#
# Harness scripts run on developers' default shell, which on macOS is the stock
# bash 3.2. A bash-4-only construct (e.g. `declare -A`) under a `#!/bin/bash`
# shebang dies opaquely there ("unbound variable") and the script silently never
# runs — exactly how check-exec-command.sh became a no-op for an unknown number
# of sessions before BEH-409. This guard fails on those constructs so they can't
# ship again. It must itself stay bash-3.2-safe.
#
# Exit 0 = clean, exit 1 = violations found.

set -euo pipefail

# Directory to scan (defaults to current directory)
BASE_DIR="${1:-.}"

# Color codes for output (disabled if not a TTY)
if [ -t 1 ]; then
    RED='\033[0;31m'
    GREEN='\033[0;32m'
    YELLOW='\033[1;33m'
    NC='\033[0m' # No Color
else
    RED=''
    GREEN=''
    YELLOW=''
    NC=''
fi

VIOLATIONS=()

# Bash-4-only constructs to flag, as parallel indexed arrays (NOT an associative
# array — that would make this guard itself break under bash 3.2). PATTERNS holds
# the extended-regex; LABELS holds the human-readable reason at the same index.
PATTERNS=()
LABELS=()
PATTERNS+=('(declare|local|typeset)[[:space:]]+-[a-zA-Z]*A')
LABELS+=('declare -A (associative array) — bash 4.0+')
PATTERNS+=('(declare|local|typeset)[[:space:]]+-[a-zA-Z]*n')
LABELS+=('declare -n (nameref) — bash 4.3+')
PATTERNS+=('\$\{[A-Za-z_][A-Za-z0-9_]*(\[[^]]*\])?(,,?|\^\^?)')
LABELS+=('${var,,}/${var^^} (case modification) — bash 4.0+')
PATTERNS+=('(^|[^[:alnum:]_])(mapfile|readarray)[[:space:]]')
LABELS+=('mapfile/readarray — bash 4.0+')

# Find every bash script under BASE_DIR: a regular file whose first line is a
# bash shebang (`#!/bin/bash` or `#!/usr/bin/env bash`). Skip vendored / build /
# VCS dirs, and skip this guard + its own test (they name the forbidden
# constructs in patterns and fixtures and would self-flag).
while IFS='' read -r file; do
    case "$file" in
        *check-bash3-compat.sh) continue ;;
    esac
    first_line=$(head -n 1 "$file" 2>/dev/null || true)
    # Match a bash shebang with OR without trailing flags — `#!/bin/bash -eu`
    # is common, and an exact-only match would silently skip such scripts, the
    # very "guard never runs over this script" failure this guard exists to stop.
    # The space (or end-of-line) before any flags prevents matching `#!/bin/bashx`.
    case "$first_line" in
        '#!/bin/bash'|'#!/bin/bash '*|'#! /bin/bash'|'#! /bin/bash '*) ;;
        '#!/usr/bin/env bash'|'#!/usr/bin/env bash '*) ;;
        *) continue ;;
    esac

    # Scan for each forbidden construct, skipping full-line comments (incl. the
    # shebang) so prose mentioning a construct — e.g. a comment explaining why a
    # script avoids `declare -A` — is never flagged.
    i=0
    while [ "$i" -lt "${#PATTERNS[@]}" ]; do
        while IFS='' read -r match; do
            line_num="${match%%:*}"
            line_content="${match#*:}"
            trimmed="${line_content#"${line_content%%[![:space:]]*}"}"
            case "$trimmed" in
                '#'*) continue ;;
            esac
            VIOLATIONS+=("$file:$line_num: ${LABELS[$i]}")
        done < <(grep -nE "${PATTERNS[$i]}" "$file" 2>/dev/null || true)
        i=$((i + 1))
    done
done < <(find "$BASE_DIR" -type f \
    -not -path '*/.git/*' \
    -not -path '*/vendor/*' \
    -not -path '*/node_modules/*' \
    -not -path '*/bin/*' \
    | sort)

# Report results
if [ ${#VIOLATIONS[@]} -eq 0 ]; then
    echo -e "${GREEN}✓${NC} No bash-4-only constructs found (scripts are bash 3.2 safe)"
    exit 0
else
    echo -e "${RED}✗${NC} Found ${#VIOLATIONS[@]} bash-4-only construct(s) under a bash shebang:"
    echo
    for violation in "${VIOLATIONS[@]}"; do
        echo -e "  ${YELLOW}$violation${NC}"
    done
    echo
    echo "Harness scripts must run under macOS's stock bash 3.2. Rewrite using"
    echo "bash-3.2-safe equivalents (indexed arrays, tr/awk for case folding)."
    exit 1
fi
