#!/bin/bash
# check-awk-portability.sh — guard against awk that only some awks run (#129).
#
# The harness scripts run under three awks: mawk (the sandbox's awk, where the
# Gate runs), BWK awk (macOS) and gawk (CI). mawk 1.3.4 does not support regex
# interval expressions ({n}, {n,}, {n,m}) and raises no error on one: the regex
# just never matches. A guard script only reports failures, so a regex that never
# matches makes it pass vacuously in the sandbox while it works on the host and
# in CI (#96). gawk-only functions fail the other way, loudly but only off-CI.
#
# This guard reads every awk program written inline as a single-quoted argument
# in scripts/*.sh and install.sh, and fails on an interval inside a regex literal
# (or a string matched with ~), or a call to a gawk-only function. A program
# passed with -f or in double quotes is not read. It must itself stay
# bash-3.2-safe, and its awk must run under all three awks.
#
# Exit 0 = clean, exit 1 = violations found.

set -euo pipefail

# Repo root to scan (defaults to current directory)
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

# scan FILE — "<line>: <reason>" for every non-portable construct in FILE's
# inline awk programs. It walks the shell text for an `awk` command word, skips
# its options, then lexes the single-quoted program: strings, regex literals
# (a `/` where an operand can't end, so not division), comments, identifiers.
scan() {
    awk '
        { src = src $0 "\n" }

        function line_at(p,    t) { t = substr(src, 1, p - 1); return gsub(/\n/, "", t) + 1 }
        function report(p, msg) { print line_at(p) ": " msg }
        function blank(c) { return c == " " || c == "\t" }
        function ident(c) { return c ~ /^[A-Za-z0-9_]$/ }

        # word_end(k) — index just past the shell word starting at k.
        function word_end(k,    c) {
            while (k <= n) {
                c = substr(src, k, 1)
                if (blank(c) || c == "\n") break
                if (c == "\047" || c == "\"") {
                    k++
                    while (k <= n && substr(src, k, 1) != c) k++
                }
                k++
            }
            return k
        }

        # program_start(k) — index of the quote opening the program of the awk
        # command word ending at k, or 0 when there is no inline program.
        function program_start(k,    c, e, tok) {
            while (k <= n) {
                c = substr(src, k, 1)
                if (blank(c)) { k++; continue }
                if (c == "\\" && substr(src, k + 1, 1) == "\n") { k += 2; continue }
                if (c == "\047") return k
                if (c != "-") return 0
                e = word_end(k)
                tok = substr(src, k, e - k)
                k = e
                if (tok ~ /^-f/) return 0
                if (tok == "-F" || tok == "-v") {
                    while (k <= n && blank(substr(src, k, 1))) k++
                    k = word_end(k)
                }
            }
            return 0
        }

        # interval(p) — the interval expression opening at p, or "" if the brace
        # there is not one.
        function interval(p,    e) {
            if (substr(src, p + 1, 1) !~ /^[0-9]$/) return ""
            e = p + 1
            while (e <= n && substr(src, e, 1) ~ /^[0-9,]$/) e++
            if (substr(src, e, 1) != "}") return ""
            return substr(src, p, e - p + 1)
        }

        function flag_interval(p, iv) {
            report(p, "awk regex interval " iv " — mawk (the sandbox awk) silently never matches it; repeat the atom (a{2} -> aa) or use ? (x{0,3} -> x?x?x?)")
        }

        # scan_program(k) — lex the program starting at k; returns the index just
        # past its closing quote.
        function scan_program(k,    c, last, j, inbr, first, iv, name) {
            last = "("
            while (k <= n) {
                c = substr(src, k, 1)
                if (c == "\047") return k + 1
                if (c == "\n") { last = c; k++; continue }
                if (blank(c)) { k++; continue }
                if (c == "\\" && substr(src, k + 1, 1) == "\n") { k += 2; continue }
                if (c == "#") {
                    while (k <= n && substr(src, k, 1) != "\n" && substr(src, k, 1) != "\047") k++
                    continue
                }
                if (c == "\"") {
                    j = k + 1
                    while (j <= n) {
                        c = substr(src, j, 1)
                        if (c == "\\") { j += 2; continue }
                        if (c == "\"" || c == "\047" || c == "\n") break
                        if (c == "{" && last == "~" && (iv = interval(j)) != "") flag_interval(j, iv)
                        j++
                    }
                    k = (substr(src, j, 1) == "\"") ? j + 1 : j
                    last = "a"
                    continue
                }
                if (c == "/" && last != "a" && last != ")" && last != "]") {
                    j = k + 1; inbr = 0
                    while (j <= n) {
                        c = substr(src, j, 1)
                        if (c == "\\") { j += 2; continue }
                        if (c == "\047" || c == "\n") break
                        if (inbr) {
                            if (c == "]" && j > first) inbr = 0
                        } else if (c == "[") {
                            inbr = 1
                            first = j + 1
                            if (substr(src, first, 1) == "^") first++
                        } else if (c == "/") {
                            break
                        } else if (c == "{" && (iv = interval(j)) != "") {
                            flag_interval(j, iv)
                        }
                        j++
                    }
                    k = (substr(src, j, 1) == "/") ? j + 1 : j
                    last = "a"
                    continue
                }
                if (ident(c)) {
                    j = k
                    while (j <= n && ident(substr(src, j, 1))) j++
                    name = substr(src, k, j - k)
                    k = j
                    if (name ~ /^(gensub|asort|asorti|strftime)$/) {
                        while (k <= n && blank(substr(src, k, 1))) k++
                        if (substr(src, k, 1) == "(")
                            report(j - length(name), name "() is gawk-only — mawk and BWK awk (macOS) lack it")
                    }
                    last = "a"
                    continue
                }
                last = c
                k++
            }
            return k
        }

        END {
            n = length(src)
            i = 1
            bol = 1
            while (i <= n) {
                c = substr(src, i, 1)
                if (c == "\n") { bol = 1; i++; continue }
                if (blank(c)) { i++; continue }
                if (bol && c == "#") {
                    while (i <= n && substr(src, i, 1) != "\n") i++
                    continue
                }
                bol = 0
                if (c == "a" && substr(src, i, 3) == "awk" && blank(substr(src, i + 3, 1)) &&
                    (i == 1 || substr(src, i - 1, 1) !~ /^[A-Za-z0-9_.-]$/)) {
                    start = program_start(i + 3)
                    if (start) { i = scan_program(start + 1); continue }
                }
                i++
            }
        }
    ' "$1"
}

VIOLATIONS=()
for file in "$BASE_DIR"/scripts/*.sh "$BASE_DIR"/install.sh; do
    [ -f "$file" ] || continue
    # This guard and its test name the forbidden constructs and would self-flag.
    case "$file" in
        *check-awk-portability.sh) continue ;;
    esac
    while IFS='' read -r match; do
        VIOLATIONS+=("$file:$match")
    done < <(scan "$file")
done

# Report results
if [ ${#VIOLATIONS[@]} -eq 0 ]; then
    echo -e "${GREEN}✓${NC} No awk interval expressions or gawk-only functions found (awk is mawk/BWK/gawk safe)"
    exit 0
else
    echo -e "${RED}✗${NC} Found ${#VIOLATIONS[@]} non-portable awk construct(s):"
    echo
    for violation in "${VIOLATIONS[@]}"; do
        echo -e "  ${YELLOW}$violation${NC}"
    done
    echo
    echo "Harness scripts must run under mawk (the sandbox), BWK awk (macOS) and gawk (CI)."
    echo "Spell intervals out: repeat the atom, or use ? for an optional one."
    exit 1
fi
