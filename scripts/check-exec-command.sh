#!/bin/bash
# check-exec-command.sh — guard against unbounded exec.Command for daemon/remote calls (BEH-388).
#
# Flags raw exec.Command/exec.CommandContext outside internal/proc for external daemon/remote
# tools (docker, gh, git remote ops), with an explicit allowlist for deliberately-deferred
# local git reads (per BEH-386 commit rationale) and session-managed docker calls.
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

# Track violations
VIOLATIONS=()

# Explicit allowlist for legitimate unbounded calls:
# - internal/session/session.go:55 - main docker run with timer-based kill
# - internal/session/session.go:75 - docker kill called by timeout handler
# - internal/sandbox/sandbox.go:192 - docker build (can take minutes, user-visible progress)
# Format: "path-suffix:line" where path-suffix matches the end of the file path
declare -A ALLOWLIST=(
    ["internal/session/session.go:55"]="main docker run with timer-based kill"
    ["internal/session/session.go:75"]="docker kill called by timeout handler"
    ["internal/sandbox/sandbox.go:192"]="docker build (can take minutes, user-visible progress)"
)

# Find all Go files, excluding vendor and internal/proc
GO_FILES=$(find "$BASE_DIR" -name "*.go" -not -path "*/vendor/*" -not -path "*/internal/proc/*" | sort)

# Pattern to match exec.Command or exec.CommandContext
EXEC_PATTERN='exec\.(Command|CommandContext)\('

# Check each file
for file in $GO_FILES; do
    # Skip if no matches (use -E for extended regex)
    if ! grep -qE "$EXEC_PATTERN" "$file" 2>/dev/null; then
        continue
    fi

    # Get all matches with line numbers
    while IFS='' read -r match; do
        line_num=$(echo "$match" | cut -d: -f1)
        line_content=$(echo "$match" | cut -d: -f2-)

        # Try different patterns to extract the command
        cmd=""

        # Pattern 1: exec.Command("cmd", ...)
        if [[ "$line_content" =~ exec\.(Command|CommandContext)\([[:space:]]*\"([^\"]+)\" ]]; then
            cmd="${BASH_REMATCH[2]}"
        # Pattern 2: exec.CommandContext(ctx, "cmd", ...)
        elif [[ "$line_content" =~ exec\.(Command|CommandContext)\([^,]+,[[:space:]]*\"([^\"]+)\" ]]; then
            cmd="${BASH_REMATCH[2]}"
        fi

        # Check if it's a daemon/remote command that needs a deadline
        if [ -n "$cmd" ]; then
            # Check if this location matches an allowlist entry
            is_allowed=0
            for allowed_key in "${!ALLOWLIST[@]}"; do
                # Check if file ends with the path pattern and line matches
                if [[ "$file" == *"${allowed_key%:*}" ]] && [[ "${allowed_key##*:}" == "$line_num" ]]; then
                    is_allowed=1
                    break
                fi
            done

            # Skip if this location is explicitly allowlisted
            if [ "$is_allowed" -eq 1 ]; then
                continue
            fi

            case "$cmd" in
                docker|gh)
                    # These always need deadlines
                    VIOLATIONS+=("$file:$line_num: unbounded $cmd call - use internal/proc with timeout")
                    ;;
                git)
                    # Check if it's a remote operation by looking at the whole call
                    # We need to see more context - get the full statement
                    full_stmt=$(sed -n "${line_num}p" "$file")

                    # Remote operations need deadlines
                    if echo "$full_stmt" | grep -qE '"(fetch|push|clone|ls-remote|pull)"'; then
                        VIOLATIONS+=("$file:$line_num: unbounded git remote operation - use internal/proc with timeout")
                    fi
                    # Local operations are deliberately allowed (BEH-386 deferral)
                    # These include: log, status, rev-list, rev-parse, worktree, etc.
                    ;;
            esac
        fi
    done < <(grep -nE "$EXEC_PATTERN" "$file" 2>/dev/null || true)
done

# Report results
if [ ${#VIOLATIONS[@]} -eq 0 ]; then
    echo -e "${GREEN}✓${NC} No unbounded daemon/remote exec.Command calls found"
    exit 0
else
    echo -e "${RED}✗${NC} Found ${#VIOLATIONS[@]} unbounded daemon/remote exec.Command call(s):"
    echo
    for violation in "${VIOLATIONS[@]}"; do
        echo -e "  ${YELLOW}$violation${NC}"
    done
    echo
    echo "Fix: Use internal/proc with appropriate timeout for daemon/remote operations"
    echo "     (docker, gh, git fetch/push/clone/ls-remote)"
    exit 1
fi