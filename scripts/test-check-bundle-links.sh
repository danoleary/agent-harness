#!/bin/bash
# Unit tests for check-bundle-links.sh (#96, #127).
#
# The installed README lives in <prefix>/share/agent-harness/, beside only the
# docs install.sh ships (its DOCS list). A relative link to anything else is
# broken in every install, and nothing on our side noticed. The guard resolves
# each relative Markdown link in every shipped .md against DOCS, from that doc's
# own directory, and fails on a link to a file the bundle doesn't carry. Fixture trees stand in for the repo. Like the
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

# fixture DIR README_BODY [GUIDE_BODY] — a repo whose install.sh ships
# README.md, docs/GUIDE.md and docs/HOWTO.md, with docs/EXTRA.md and
# docs/adr/0001-a.md in the tree but not in the bundle.
fixture() {
    mkdir -p "$1/docs/adr"
    printf 'DOCS="README.md LICENSE\n\tdocs/GUIDE.md docs/HOWTO.md"\n' > "$1/install.sh"
    printf 'license\n' > "$1/LICENSE"
    printf '%b' "${3:-# Guide\n}" > "$1/docs/GUIDE.md"
    printf '# How to\n' > "$1/docs/HOWTO.md"
    printf '# Extra\n' > "$1/docs/EXTRA.md"
    printf '# ADR-0001\n' > "$1/docs/adr/0001-a.md"
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
    elif grep -qE -- "$3" <<< "$report"; then
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

# The #127 bug: the installed docs/DESIGN.md linked adr/NNNN-*.md, which the
# bundle never shipped. A shipped doc's links resolve from its own directory.
echo -n "Test 9: a shipped doc's link to an unshipped file is flagged... "
fixture "$TEST_DIR/t9" '# Readme\n' '# Guide\n\nSee [ADR-0001](adr/0001-a.md).\n'
expect_fail "unshipped link in a shipped doc" "$TEST_DIR/t9" 'docs/GUIDE\.md:3:.*docs/adr/0001-a\.md'

echo -n "Test 10: a shipped doc's links resolve from its own directory... "
fixture "$TEST_DIR/t10" '# Readme\n' '# Guide\n\n[Up](../README.md), [sibling](HOWTO.md#x), [self](./GUIDE.md),\n[licence](../LICENSE) and [ADR-0001](https://github.com/o/r/blob/main/docs/adr/0001-a.md).\n'
expect_pass "doc-relative links" "$TEST_DIR/t10"

echo -n "Test 11: a shipped doc's link that climbs out of the bundle is flagged... "
fixture "$TEST_DIR/t11" '# Readme\n' '# Guide\n\n[Releases](../../releases).\n'
expect_fail "escaping link in a shipped doc" "$TEST_DIR/t11" 'docs/GUIDE\.md:3:.*\.\./\.\./releases'

echo -n "Test 12: this repo's shipped docs link only to shipped docs... "
expect_pass "the real bundle" "$REPO_DIR"

# #177: shipped() piped DOCS into `grep -q`, which exits on its first match;
# under pipefail the writer's SIGPIPE read as "not shipped". A DOCS list past
# the pipe buffer makes that fire for a doc early on the list.
echo -n "Test 13: a link to an early doc on a large DOCS list passes (#177)... "
fixture "$TEST_DIR/t13" '# Readme\n\nSee [the guide](docs/GUIDE.md).\n'
pad=$(printf '%0240d' 0 | tr 0 x)
list="README.md docs/GUIDE.md"
i=1
while [ "$i" -le 600 ]; do
    printf 'x\n' > "$TEST_DIR/t13/docs/$i-$pad.txt"
    list="$list docs/$i-$pad.txt"
    i=$((i + 1))
done
printf 'DOCS="%s"\n' "$list" > "$TEST_DIR/t13/install.sh"
expect_pass "large DOCS list" "$TEST_DIR/t13"

# expect_fail itself once matched its report with `echo | grep -q`: grep quit on
# a first-line match, echo took SIGPIPE writing the rest, and under pipefail
# Test 8's plainly matching report read as a FAIL (#273). A stub guard replays
# that report, padded well past a pipe buffer so the race always fires.
echo -n "Test 14: a long report that matches on its first line passes (#273)... "
cat > "$TEST_DIR/long-report.sh" <<'EOF'
#!/bin/bash
echo "README.md:3: link to docs/EXTRA.md, which the bundle does not ship"
i=0
while [ "$i" -lt 4000 ]; do
    echo "docs/GUIDE.md:$i: link to docs/EXTRA.md, which the bundle does not ship"
    i=$((i + 1))
done
exit 1
EOF
chmod +x "$TEST_DIR/long-report.sh"
(SCRIPT_PATH="$TEST_DIR/long-report.sh" && expect_fail "long report" "$TEST_DIR/t14" 'README\.md:3:.*docs/EXTRA\.md')

echo
echo -e "${GREEN}All tests passed!${NC}"
