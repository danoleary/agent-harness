# ADR-0034: A conflict resolution's edits beyond the conflict are reviewed before push

- Status: Accepted
- Date: 2026-10-27
- Extends: ADR-0017

## Context

The review stage clears a branch against the base it was cut from. If the base
moves while the branch is in review, the pre-push replay in `ship.Land` can hit a
content conflict, and the review's conflict hook (BEH-581) launches a sandboxed
resolution session and re-runs the gates. Until now that was all that stood
between the session's edits and the push.

The resolution session is the only stage that writes code after review. In #114
it went well past the one conflicted hunk. It ported a test the base had just
added to the branch's changed API, renumbered a design record whose number the
base had taken and rewrote its cross-references, and reworded the commit. Git
flagged none of that. The gates were green and the result happened to be
correct. But the PR shipped as if it had passed review, and part of it never
had. A green gate cannot show that a hand-ported test still asserts what its
author meant.

## Decision

**What the resolution changed beyond the files git flagged is reviewed before the
push, and the PR says what was written after review.**

- **The conflicted files come from git.** `Worktree.RebaseConflicts` reads the
  unmerged paths before it aborts the replay, and `ship.Land` hands them to the
  `Resolve` hook: `func(conflicted []string) ship.Resolution`.
- **What was rewritten comes from git too.** `Worktree.RewrittenSince(base, head)`
  compares the reviewed range with the resolved one, file by file, using only the
  lines the branch adds and removes. A clean cherry-pick moves the branch's
  changes but not their lines, so a clean replay rewrites nothing. A ported test
  or a renumbered file (seen as a delete and an add) does show up. It also reports
  reworded commit messages and carries `git range-diff` over the two ranges.
- **A scoped review runs over the rest.** If any rewritten file is outside the
  conflicted set, the stage launches one more review session (`review-resolution`)
  whose envelope names the conflicted files, the files beyond them and the
  range-diff, and limits the review to those. The push waits on `verify.Review`
  over its verdict. A fix it commits gets fresh gates. A blocked or missing
  verdict keeps the worktree and leaves a tracker breadcrumb naming the files.
- **The PR body says so.** `ship.Resolution.Note` goes into the PR body ahead of
  the trailer. It names the conflicted files and any reworded messages, and when
  there were extra edits it names them and says a scoped review cleared them.

## Alternatives considered

- **Rebase onto the latest base before the review session as well.** This shrinks
  the window but does not close it, because the base can still move during review.
  It also costs a fetch and a replay on every run, not only on the rare conflict.
  It can be added on top of this later.
- **Only note the files in the PR, with no review.** That is honest, but it still
  ships unreviewed code on an autonomous loop. The note is kept as well as the
  review, not in place of it.
- **Review every resolution, conflicted hunks included.** That doubles the
  sessions for the common case, where the resolution touched only what git
  flagged, and the conflicted hunk is the part the resolution prompt already
  scopes the session to.

## Consequences

- The conflicted list is what git flagged at the first conflicting commit. A
  later commit's conflicts are not in it, so they count as "beyond" and are
  reviewed. That errs toward more review, not less.
- The CI watch's reactive rebase after a prerequisite merges (#82) and the PR
  loop's conflict fix still push resolutions with only a gate behind them. Both
  act on a PR a human is already reading. They can reuse `RewrittenSince` if
  that turns out not to be enough.
- `pr.BuildBody` takes a note, and `hostio.Fake` records PR bodies.
