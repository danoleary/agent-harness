# ADR-0034: ADR numbers are reserved in an append-only index

- Status: Accepted
- Date: 2026-10-27
- Extends: the `check-adr` repo guard (#92)

## Context

A new ADR takes the next free number on the branch's base. Two tickets in
flight at once both see the same base and both take the same number, under
different slugs. The files never conflict in git. `check-adr`
(`scripts/check-adr-numbers.sh`, #92) catches the duplicate, but only in a tree
that has both files. Each PR's tree has only its own, so both PRs go green.

That is what happened in #146. #99 and #110 each added an ADR-0030, each passed
CI against the main it branched from, and GitHub merged both cleanly. After the
second merge every branch's `make check` failed at `check-adr`, whatever it
changed. The #116 session renumbered #110's ADR to 0031 inside an unrelated
commit, and every other in-flight branch would have had to do the same.

The harness does not merge PRs; a human does. No Stage, Gate or Landing runs at
merge time, so a gate that re-checks "just before merging" has nothing to
attach to. A merge queue or "require branches to be up to date" would work, but
both are repository settings that each Consumer and fork would have to turn on,
not something the repo can enforce.

## Decision

`docs/adr/README.md` is the ADR index: one `- [ADR-NNNN]` line per ADR, linking
its `NNNN-<slug>.md` file and giving its title, in ascending order. `check-adr` fails when the index is missing,
leaves an ADR out, names a file that doesn't exist, labels an entry with a
number that isn't its file's, or is out of order.

A new ADR is always the highest number, so its line always goes after the last
line of the list. Two branches that take the same number therefore edit the
same spot of the same file. When the first merges, the second PR conflicts with
main (GitHub shows it as CONFLICTING and refuses the merge). The PR loop picks
up conflicting PRs on the harness's branch prefix, and Landing's rebase hits the
same conflict. A resolver that keeps both lines still fails `check-adr` on the
duplicate number, so the collision has to be fixed by renumbering on that
branch, not on main.

## Consequences

- The collision moves from main to the second branch. Main's `make check` no
  longer goes red because of two branches that were each green.
- A new ADR now touches two files, and the guard says which line to add if one
  is forgotten.
- Two branches that each add a *different* number at the same time (say 0034 and
  0035, by someone who numbered against a sibling branch) also conflict on the
  index. That conflict is trivial: keep both lines in order.
- The guard catches only ADR numbers. Other values that parallel branches can
  each take independently are not covered by this.
