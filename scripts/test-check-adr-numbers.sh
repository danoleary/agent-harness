#!/bin/bash
# Unit tests for check-adr-numbers.sh (#92, #146).
#
# The guard fails when two ADRs share a number (two parallel tickets each took
# the next free one on their base), when an ADR-NNNN reference in a Go comment or
# Markdown names a number with no file, when a Markdown link to
# adr/NNNN-<slug>.md points at a file that doesn't exist, and when the ADR index
# (docs/adr/README.md) is not exactly the ADR files in ascending order — the
# index is what turns two parallel branches' same number into a git conflict
# (ADR-0034). Fixture trees stand in
# for a repo so the cases run without touching the real docs. Like the other
# harness guards, this must run under macOS's stock bash 3.2.

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

TEST_DIR=$(mktemp -d)
trap "rm -rf $TEST_DIR" EXIT

SCRIPT_PATH="$(dirname "$0")/check-adr-numbers.sh"

# fixture DIR — a repo with two distinct ADRs, their index, and references to both.
fixture() {
    mkdir -p "$1/docs/adr" "$1/internal/foo"
    printf '# ADR-0001: One\n' > "$1/docs/adr/0001-one.md"
    printf '# ADR-0002: Two\n\nBuilds on ADR-0001.\n' > "$1/docs/adr/0002-two.md"
    printf '# Decisions\n\n- [ADR-0001](0001-one.md): One\n- [ADR-0002](0002-two.md): Two\n' > "$1/docs/adr/README.md"
    printf 'See [ADR-0002](adr/0002-two.md).\n' > "$1/docs/DESIGN.md"
    printf 'package foo\n\n// Foo follows ADR-0001.\nfunc Foo() {}\n' > "$1/internal/foo/foo.go"
}

# expect_fail NAME DIR PATTERN — the guard must exit non-zero AND report PATTERN,
# so a missing or broken guard can't masquerade as a passing RED.
expect_fail() {
    local report
    report=$("$SCRIPT_PATH" "$2" 2>&1 || true)
    if "$SCRIPT_PATH" "$2" > /dev/null 2>&1; then
        echo -e "${RED}FAIL${NC} - $1: expected failure but got success"
        exit 1
    elif echo "$report" | grep -qE -- "$3"; then
        echo -e "${GREEN}PASS${NC}"
    else
        echo -e "${RED}FAIL${NC} - $1: expected a report matching '$3', got: $report"
        exit 1
    fi
}

expect_pass() {
    local report
    if report=$("$SCRIPT_PATH" "$2" 2>&1); then
        echo -e "${GREEN}PASS${NC}"
    else
        echo -e "${RED}FAIL${NC} - $1: expected success, got: $report"
        exit 1
    fi
}

echo "Testing check-adr-numbers.sh..."
echo

echo -n "Test 1: distinct numbers with resolving references pass... "
fixture "$TEST_DIR/t1"
expect_pass "clean tree" "$TEST_DIR/t1"

# The #92 bug: two tickets each took 0002 under different slugs, so git never
# saw a conflict. Both files must be named in the report.
echo -n "Test 2: two ADRs sharing a number are flagged... "
fixture "$TEST_DIR/t2"
printf '# ADR-0002: Also two\n' > "$TEST_DIR/t2/docs/adr/0002-also-two.md"
expect_fail "duplicate number" "$TEST_DIR/t2" '0002.*0002-also-two\.md.*0002-two\.md|0002.*0002-two\.md.*0002-also-two\.md'

echo -n "Test 3: ADR-NNNN in a Go comment with no file is flagged... "
fixture "$TEST_DIR/t3"
printf 'package foo\n\n// Bar follows ADR-0009.\nfunc Bar() {}\n' > "$TEST_DIR/t3/internal/foo/bar.go"
expect_fail "dangling Go reference" "$TEST_DIR/t3" 'internal/foo/bar\.go:3:.*ADR-0009'

# A URL later in the comment carries its own `//`; the reference before it must
# still be scanned.
echo -n "Test 3b: ADR-NNNN before a URL in a Go comment is flagged... "
fixture "$TEST_DIR/t3b"
printf 'package foo\n\n// Bar follows ADR-0009, see https://example.com/x.\nfunc Bar() {}\n' > "$TEST_DIR/t3b/internal/foo/bar.go"
expect_fail "dangling Go reference before a URL" "$TEST_DIR/t3b" 'internal/foo/bar\.go:3:.*ADR-0009'

echo -n "Test 4: ADR-NNNN in Markdown with no file is flagged... "
fixture "$TEST_DIR/t4"
printf 'Intro.\n\nPer ADR-0007, we do it.\n' > "$TEST_DIR/t4/README.md"
expect_fail "dangling Markdown reference" "$TEST_DIR/t4" 'README\.md:3:.*ADR-0007'

# The number exists but the slug is wrong — e.g. a renumber that kept the old
# slug, or a link written against the sibling branch's file.
echo -n "Test 5: a link to an adr/NNNN-<slug>.md that doesn't exist is flagged... "
fixture "$TEST_DIR/t5"
printf 'See [ADR-0002](adr/0002-wrong-slug.md).\n' > "$TEST_DIR/t5/docs/DESIGN.md"
expect_fail "dangling link" "$TEST_DIR/t5" 'docs/DESIGN\.md:1:.*adr/0002-wrong-slug\.md'

# A Go string literal is test data, not a cross-reference (internal/git's
# DocsOnly fixtures name docs/adr/0026-comments.md, which need not exist).
echo -n "Test 6: ADR paths and numbers in Go string literals are not flagged... "
fixture "$TEST_DIR/t6"
printf 'package foo\n\nvar paths = []string{"docs/adr/0026-comments.md", "ADR-0026"}\n' > "$TEST_DIR/t6/internal/foo/data_test.go"
expect_pass "Go string literal" "$TEST_DIR/t6"

# Linked worktrees under .claude/worktrees are other branches' trees, not this one.
echo -n "Test 7: files under .claude/worktrees and .git are ignored... "
fixture "$TEST_DIR/t7"
mkdir -p "$TEST_DIR/t7/.claude/worktrees/x/docs/adr" "$TEST_DIR/t7/.git"
printf '# ADR-0001: Other\n' > "$TEST_DIR/t7/.claude/worktrees/x/docs/adr/0001-other.md"
printf 'Per ADR-0042.\n' > "$TEST_DIR/t7/.claude/worktrees/x/NOTES.md"
printf 'Per ADR-0042.\n' > "$TEST_DIR/t7/.git/NOTES.md"
expect_pass "ignored dirs" "$TEST_DIR/t7"

echo -n "Test 8: an ADR the index does not list is flagged... "
fixture "$TEST_DIR/t8"
printf '# ADR-0003: Three\n' > "$TEST_DIR/t8/docs/adr/0003-three.md"
expect_fail "unindexed ADR" "$TEST_DIR/t8" 'docs/adr/README\.md.*0003-three\.md'

echo -n "Test 9: an index entry naming no file is flagged... "
fixture "$TEST_DIR/t9"
printf -- '- [ADR-0003](0003-three.md): Three\n' >> "$TEST_DIR/t9/docs/adr/README.md"
expect_fail "dangling index entry" "$TEST_DIR/t9" 'docs/adr/README\.md:5:.*0003-three\.md'

# A new ADR inserted mid-list rather than appended could merge cleanly beside a
# sibling's, so the index must ascend and its label must match the file.
echo -n "Test 10: an index out of ascending order is flagged... "
fixture "$TEST_DIR/t10"
printf '# Decisions\n\n- [ADR-0002](0002-two.md): Two\n- [ADR-0001](0001-one.md): One\n' > "$TEST_DIR/t10/docs/adr/README.md"
expect_fail "unordered index" "$TEST_DIR/t10" 'docs/adr/README\.md:4:.*0001-one\.md'

echo -n "Test 11: an index label that disagrees with its file is flagged... "
fixture "$TEST_DIR/t11"
printf '# Decisions\n\n- [ADR-0001](0001-one.md): One\n- [ADR-0003](0002-two.md): Two\n' > "$TEST_DIR/t11/docs/adr/README.md"
expect_fail "mislabelled index entry" "$TEST_DIR/t11" 'docs/adr/README\.md:4:.*ADR-0003.*0002-two\.md'

echo -n "Test 12: a tree with no ADRs needs no index... "
mkdir -p "$TEST_DIR/t12"
printf 'No decisions yet.\n' > "$TEST_DIR/t12/README.md"
expect_pass "no ADRs" "$TEST_DIR/t12"

# The #146 incident, replayed: #99 and #110 each branched from a main whose last
# ADR was 0029, each took 0030 under its own slug, and each passed the guard on
# its own branch, so both went green in CI. Once #99 merged, #110 merged cleanly
# too and main's check went red. With the index, #110 must now conflict with
# main — GitHub refuses the merge and the PR loop picks the PR up — and the tree
# a resolver gets by keeping both index lines must still fail the guard.
echo -n "Test 13: two branches taking the same number conflict once one merges (#146)... "
R="$TEST_DIR/t13"
git init -q -b main "$R"
git -C "$R" config user.email t@example.com
git -C "$R" config user.name t
mkdir -p "$R/docs/adr"
printf '# ADR-0028: Open questions\n' > "$R/docs/adr/0028-a-ready-ticket-with-open-questions-is-dispatched-with-a-warning.md"
printf '# ADR-0029: Diataxis\n' > "$R/docs/adr/0029-docs-are-split-by-diataxis-and-live-in-the-repo.md"
printf '%s\n' '# Decisions' '' \
    '- [ADR-0028](0028-a-ready-ticket-with-open-questions-is-dispatched-with-a-warning.md): Open questions' \
    '- [ADR-0029](0029-docs-are-split-by-diataxis-and-live-in-the-repo.md): Diataxis' > "$R/docs/adr/README.md"
git -C "$R" add -A
git -C "$R" commit -q -m base
# take_0030 BRANCH SLUG TITLE — the branch adds ADR-0030 and its index line.
take_0030() {
    git -C "$R" checkout -q -b "$1" main
    printf '# ADR-0030: %s\n' "$3" > "$R/docs/adr/0030-$2.md"
    printf -- '- [ADR-0030](0030-%s.md): %s\n' "$2" "$3" >> "$R/docs/adr/README.md"
    git -C "$R" add -A
    git -C "$R" commit -q -m "$1"
    if ! "$SCRIPT_PATH" "$R" > /dev/null 2>&1; then
        echo -e "${RED}FAIL${NC} - branch $1 should pass the guard on its own"
        exit 1
    fi
}
take_0030 gh-99 a-signal-killed-session-is-interrupted-not-failed 'Interrupted, not failed'
take_0030 gh-110 the-ticket-loop-can-leave-ci-to-the-pr-loop 'Leave CI to the PR loop'
git -C "$R" checkout -q main
git -C "$R" merge -q --no-edit gh-99
if git -C "$R" merge -q --no-edit gh-110 > /dev/null 2>&1; then
    echo -e "${RED}FAIL${NC} - #110 merged cleanly onto a main that already took ADR-0030"
    exit 1
fi
if ! git -C "$R" diff --name-only --diff-filter=U | grep -qx 'docs/adr/README.md'; then
    echo -e "${RED}FAIL${NC} - the merge conflicted, but not on docs/adr/README.md"
    exit 1
fi
# Resolve by keeping both sides, as a careless conflict hook would.
grep -v '^[<=>][<=>]*' "$R/docs/adr/README.md" > "$R/resolved" || true
mv "$R/resolved" "$R/docs/adr/README.md"
expect_fail "both-sides resolution" "$R" 'ADR-0030 is taken by more than one file'

echo -n "Test 14: the real harness tree passes... "
HARNESS_DIR="$(dirname "$(dirname "$0")")"
expect_pass "real tree" "$HARNESS_DIR"

echo
echo -e "${GREEN}All tests passed!${NC}"
