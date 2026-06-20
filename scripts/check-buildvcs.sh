#!/bin/bash
# Guard against VCS-stamping Go invocations in Makefiles that omit
# -buildvcs=false (BEH-459).
#
# The Go toolchain stamps VCS metadata into `main` packages and shells out to
# git to do it. In a linked git worktree (the `.claude/worktrees/<slug>` path
# the tdd/review skills use) that git call returns 128, so `go build`/`go test`
# abort before running anything. Appending -buildvcs=false disables the stamping
# and is a no-op in a normal checkout (CI), so it unblocks worktree work without
# changing CI behaviour. This guard fails the gate if a stamping subcommand
# (`go build`/`go install`/`go run`/`go test`) appears in a Makefile recipe
# without it, so the fix can't silently regress.
#
# It is itself bash-3.2-safe (runs under macOS's stock /bin/bash).
#
# Scope: this is a deliberately simple per-line text scanner. It does NOT join
# `\`-continued recipe lines, so a `go build` split across lines with the flag on
# the continuation line, or a tab-indented `# go build …` comment inside a
# recipe, will trip a false positive. Both fail loudly (never a silent miss), so
# the guard stays safe-by-default; keep stamping go commands on a single line.

set -euo pipefail

ROOT="${1:-.}"

violations=0

while IFS= read -r makefile; do
    line_no=0
    while IFS= read -r line || [ -n "$line" ]; do
        line_no=$((line_no + 1))

        # Skip full-line comments.
        case "$line" in
            \#*) continue ;;
            *) ;;
        esac

        # Only recipe lines that invoke a VCS-stamping go subcommand.
        if echo "$line" | grep -qE '(^|[^[:alnum:]_])go[[:space:]]+(build|install|run|test)([^[:alnum:]_]|$)'; then
            if ! echo "$line" | grep -qE -- '-buildvcs=false'; then
                echo "${makefile}:${line_no}: go build/test/run/install without -buildvcs=false (breaks builds in a linked worktree; see BEH-459)"
                violations=$((violations + 1))
            fi
        fi
    done < "$makefile"
done < <(find "$ROOT" -type f \( -name Makefile -o -name '*.mk' -o -name '*.make' \) ! -path '*/.git/*')

if [ "$violations" -gt 0 ]; then
    echo
    echo "Found ${violations} VCS-stamping go command(s) missing -buildvcs=false."
    exit 1
fi

echo "✓ All VCS-stamping go commands in Makefiles set -buildvcs=false"
