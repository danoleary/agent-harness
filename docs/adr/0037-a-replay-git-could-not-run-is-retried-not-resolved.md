# ADR-0037: A replay git could not run is retried, not resolved

- Status: Accepted
- Date: 2026-11-01
- Extends: ADR-0017, ADR-0035

## Context

Before a push, `ship.Land` replays the branch onto origin/main: `reset --hard
origin/main`, then `cherry-pick` of the branch's commits (BEH-618).
`Worktree.Rebase` reported two different stops as `RebaseConflict`:

- a halt with unmerged paths in the index, which is a real content conflict;
- its fail-safe branch, where the cherry-pick errored and no pick was left in
  progress. That is git failing, not a conflict: an `index.lock` another process
  held, a stat or permission blip on the bind-mounted worktree, a file in the way.

git's output was thrown away, so the run log could not say which one happened.
In #164 the replay picked the first of two commits and stopped on the second. The
branch had been cut from that same origin/main, so the replay should have been a
no-op apart from the SHAs. The harness logged "pre-push rebase hit a content
conflict" and launched a conflict-resolution session. The session ran the same
replay, and both commits applied with the tree unchanged. The harness then logged
"conflict resolved", re-ran the gates and pushed. That cost a sandbox session and
two gate runs, and it left a conflict resolution on record that never happened,
which ADR-0035's scoped review acts on.

Building that failure in a test repo turned up a worse case on the same path. A
pick that halts with its change staged, no unmerged paths and the pick still in
progress (a refused ref update, for example) looked like an empty commit to the
auto-skip loop (BEH-678). The loop skipped it, and the branch was pushed without
that commit.

## Decision

**Only a halt with unmerged paths is a content conflict. git failing with no
conflict is recorded, retried once, and then left for a human. It is never handed
to a resolution session.**

- **git classifies the stop.** `Worktree.RebaseDetail` returns a `RebaseReport`:
  `RebaseClean`, `RebaseConflict` (unmerged paths, named in `Conflicted`) or
  `RebaseFailed`. Every non-clean report carries git's `Output` from the failing
  step. `RebaseFailed` covers a cherry-pick error with no pick in progress, a
  failed setup step, the skip loop's safety bound, and a halted pick whose index
  differs from HEAD. That last one is no longer skipped as empty.
- **ship retries a failure once.** `ship.replay` logs git's output for every stop.
  On anything but a conflict with named paths, it replays once more, but only if
  git put the branch back on the tip it started from and the worktree is clean.
  Re-pinning any other tip would replay the wrong commits. A second failure is the
  new `ReplayFailed`, which `Land` returns as the verdict `RebaseFailed` without
  calling `Resolve`. The review stage keeps the worktree and leaves a breadcrumb.
  The committed-fix recovery narrates it, and the CI watch hands it to a human
  without offering its resolver. `Replay` takes the caller's `EventSink` so the CI
  watch logs git's output too.
- **A resolution that found nothing says so.** If the branch already sat on
  origin/main when the resolution session started, and the session left its tree
  unchanged, `fix.ResolveConflict` logs "replay was clean in the sandbox — no
  conflict to resolve" rather than "conflict resolved". The gates are still re-run
  before the push, as for any resolution.

## Alternatives considered

- **Skip the replay when origin/main is already an ancestor of the tip.** That
  would have avoided #164 outright. But it also changes what every landing pushes:
  the replay re-stamps the committer (BEH-579) and normalises SHAs. It is a
  separate decision, and this one does not depend on it.
- **Retry with a backoff, or more than once.** A held lock is usually released
  within the second it takes to restore the branch. More retries would mostly delay
  the breadcrumb for failures that are not transient, such as a merge commit in the
  range or a permission problem.
- **Judge "no conflict to resolve" by the tree alone.** A session that resolved
  every hunk in the branch's favour also leaves the tree unchanged. "Already on
  origin/main" is the condition under which no conflict is possible.

## Consequences

- A `RebaseFailed` costs one extra local replay and no session. The run log
  carries git's output for both attempts.
- `RebaseConflicts` is replaced by `RebaseDetail`. `git.runners` gains a
  `combined` runner, bounded like `run`, for commands whose output the harness
  keeps.
- The Landing verdicts gain **RebaseFailed** (CONTEXT.md).
- `gittest` can make a replay fail on cue (`InTheWayOfReplay`,
  `RefuseReplayedCommit`) through hooks under a `core.hooksPath` of its own.
