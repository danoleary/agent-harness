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

# Legitimate unbounded calls opt out with an inline marker comment on the call line:
#
#   cmd := exec.Command("docker", "build", ...) // allow-unbounded-exec: <reason>
#
# Keying the allowlist on a code marker (not a file:line tuple) means it travels
# WITH the call — an edit that shifts the line, or a refactor that moves the call to
# another file, can't silently break the guard or strip the exemption (BEH-490 retro:
# a line-keyed allowlist failed on every insertion above an allowlisted call).
ALLOW_MARKER='allow-unbounded-exec'

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
            # Skip if the call line carries the inline allow marker (per-line, so an
            # unmarked call elsewhere in the same file is still flagged).
            if [[ "$line_content" == *"$ALLOW_MARKER"* ]]; then
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