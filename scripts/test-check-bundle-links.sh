#!/bin/bash
# Unit tests for check-bundle-links.sh (#96).
#
# The installed README lives in <prefix>/share/agent-harness/, beside only the
# docs install.sh ships (its DOCS list). A relative link to anything else is
# broken in every install, and nothing on our side noticed. The guard resolves
# each relative Markdown link in README.md against DOCS and fails on a link to a
# file the bundle doesn't carry. Fixture trees stand in for the repo. Like the
# other harness guards, this must run under macOS's stock bash 3.2.

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

SCRIPT_DIR="$(cd "$(dirname "$0")" && pwd)"
SCRIPT_PATH="$SCRIPT_DIR/check-bundle-links.sh"
REPO_DIR="$(dirname "$SCRIPT_DIR")"

# fixture DIR README_BODY — a repo whose install.sh ships README.md and
# docs/GUIDE.md, with docs/EXTRA.md in the tree but not in the bundle.
fixture() {
    mkdir -p "$1/docs"
    printf 'DOCS="README.md LICENSE docs/GUIDE.md"\n' > "$1/install.sh"
    printf 'license\n' > "$1/LICENSE"
    printf '# Guide\n' > "$1/docs/GUIDE.md"
    printf '# Extra\n' > "$1/docs/EXTRA.md"
    printf '%b' "$2" > "$1/README.md"
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

echo "Testing check-bundle-links.sh..."
echo

echo -n "Test 1: links to shipped docs, anchors and URLs pass... "
fixture "$TEST_DIR/t1" '# Readme\n\nSee [the guide](docs/GUIDE.md), [its setup](docs/GUIDE.md#setup),\n[again](./docs/GUIDE.md "title"), [the licence](LICENSE), [below](#usage),\n[the site](https://example.com/x.md) and [mail](mailto:a@b.c).\n'
expect_pass "shipped links" "$TEST_DIR/t1"

# The #96 bug: the README linked docs/ files the installer never copied.
echo -n "Test 2: a link to a doc the bundle does not ship is flagged... "
fixture "$TEST_DIR/t2" '# Readme\n\nSee [extra](docs/EXTRA.md).\n'
expect_fail "unshipped doc" "$TEST_DIR/t2" 'README\.md:3:.*docs/EXTRA\.md'

echo -n "Test 3: a link to a directory is flagged... "
fixture "$TEST_DIR/t3" '# Readme\n\nDecisions are in [adr](docs/adr/).\n'
expect_fail "directory link" "$TEST_DIR/t3" 'README\.md:3:.*docs/adr/'

# A GitHub-relative link like ../../releases works on github.com and nowhere else.
echo -n "Test 4: a link that climbs out of the bundle is flagged... "
fixture "$TEST_DIR/t4" '# Readme\n\nDownload from [Releases](../../releases).\n'
expect_fail "escaping link" "$TEST_DIR/t4" 'README\.md:3:.*\.\./\.\./releases'

echo -n "Test 5: a reference-style link definition is checked too... "
fixture "$TEST_DIR/t5" '# Readme\n\nSee [extra][x].\n\n[x]: docs/EXTRA.md\n'
expect_fail "reference definition" "$TEST_DIR/t5" 'README\.md:5:.*docs/EXTRA\.md'

echo -n "Test 6: a link inside a fenced code block is not a link... "
fixture "$TEST_DIR/t6" '# Readme\n\n```md\n[not a link](docs/EXTRA.md)\n```\n'
expect_pass "fenced code" "$TEST_DIR/t6"

# Listed in DOCS but absent from the tree: the release's cp would fail, so say
# so here, where the fix is obvious.
echo -n "Test 7: a shipped doc that does not exist is flagged... "
fixture "$TEST_DIR/t7" '# Readme\n\nSee [the guide](docs/GUIDE.md).\n'
rm "$TEST_DIR/t7/docs/GUIDE.md"
expect_fail "missing shipped doc" "$TEST_DIR/t7" 'docs/GUIDE\.md'

echo -n "Test 8: several links on one line are each checked... "
fixture "$TEST_DIR/t8" '# Readme\n\n[ok](docs/GUIDE.md) and [bad](docs/EXTRA.md).\n'
expect_fail "second link on a line" "$TEST_DIR/t8" 'README\.md:3:.*docs/EXTRA\.md'

echo -n "Test 9: this repo's README links only to shipped docs... "
expect_pass "the real README" "$REPO_DIR"

echo
echo -e "${GREEN}All tests passed!${NC}"
