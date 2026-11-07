# ADR-0043: A branch is held by a live run, not by a leftover worktree

- Status: Accepted
- Date: 2026-11-07
- Amends: ADR-0019 (its #192 amendment), ADR-0031 (decision 3, the hold)

## Context

The PR loop stays off a branch another loop holds. Two rules decided that, and
both keyed on a worktree:

- ADR-0031 made the ticket loop's hold "an agent-claimed In Progress ticket whose
  worktree still exists". The hold was meant to end when the retrospective, or
  the committed-fix recovery (#184), removed the worktree.
- ADR-0019's #192 amendment skipped a PR whose head branch any worktree other
  than the loop's own had checked out, and said the loop never removes a
  worktree it did not create.

Both rules treat a worktree as a sign that work is going on. Worktrees are left
behind in many ways: a binary from before #184, a loop restart that cuts the
review stage's CI watch short so no retrospective runs, a removal that fails, a
human's worktree for their own branch. Each one kept its PR skipped forever.
The only symptom was a `skip:` line in `--dry-run`, and the stream said
`queue empty`. In herd on 2026-11-07 all four open PRs were skipped this way
(BEH-1305, BEH-1198, BEH-1301 / herd#1386, and a human's
`chore/trim-radix-packages` / herd#1382). Three of them were red or
conflicting (#231).

## Decision

**A branch is held only while a run that works it is live. The PR loop removes
a leftover worktree that loses nothing when removed, and reports any other
leftover worktree in the stream.**

1. **The run lock is the liveness signal.** Every run of a ticket holds a flock
   on `.agent-harness/locks/runs/<slug>.lock` for as long as it runs
   (`loophost.HoldRun`). That covers the loop's Pipeline, its committed-fix
   recovery, and a single-shot `pipeline`. The kernel drops the lock when the
   run dies, so there is no stale lock to reap (as with the claim lock,
   ADR-0024). The lock is only a signal: if another run already holds it, or it
   cannot be taken, the run says so and goes ahead.
2. **The hold** (`TicketSource.HeldBranches`) is an agent-claimed In Progress
   ticket whose run lock is held. A worktree no longer counts. A run lock that
   cannot be read fails the poll, as the claim list does.
3. **A leftover worktree is removed when nothing would be lost.** Once a PR has
   passed every other check, the Source asks about any worktree that has its
   head branch checked out and is not the loop's own `pr-<n>`. The worktree is
   safe to remove (`git.Checkout.WorktreeSafeToRemove`) when all of these hold:
   - it is not the main checkout;
   - its tree is clean, ignoring the readiness sentinel;
   - the PR's head on origin contains its HEAD (at the tip or behind it).

   A worktree whose directory is gone only needs `git worktree prune`. On every
   poll, which covers start-up and every gap between items, `Next` removes each
   safe worktree before it selects anything, whether or not that PR will be
   worked now. It narrates each removal. The removal is not forced, so git
   still refuses a worktree with untracked work.
4. **Any other worktree is a skip, and the stream says so.** A worktree that is
   dirty, has commits origin lacks, is the main checkout, or that git refused to
   remove is reported once per reason as an `item-skipped` record, a new
   loopstream Kind. The record names the PR, the worktree and the reason. The
   skip still costs no attempt (#192). `--dry-run` names the worktree it would
   remove and changes nothing.

## Alternatives considered

- **Keep the worktree as the signal and add more removal paths.** #184 did this
  for one path. Each new way to leave a worktree behind would need its own fix,
  and a human's worktree would never be covered.
- **Use the claim's age against the claim TTL.** That is a guess about
  liveness. A long review stage would look dead, and a dead run would look
  alive until the TTL ran out.
- **A pidfile per run.** It would need a stale-file check, which flock makes
  unnecessary, and the claim lock and gate slots already use flock.
- **Only remove worktrees under `.claude/worktrees/`.** That would leave the
  human-worktree case from herd#1382 skipped forever. A clean worktree at
  origin's tip holds no tracked work. Its ignored files (build output, a local
  `.env`) are the exception, as the Consequences say.

## Consequences

- The #192 amendment's "the loop never removes or reuses a worktree it did not
  create" no longer holds for a clean worktree at origin's tip with no live run.
  The PR loop can remove a human's clean worktree for their own open PR, and
  with it any ignored files in it (`git worktree remove` deletes them). To keep a
  worktree, leave something uncommitted in it, or use `scope = "opted_in"` and
  leave the PR un-labelled.
- A run started by a binary from before this ADR holds no run lock, so the PR
  loop reads it as dead. If its worktree is clean at origin's tip, the PR loop
  may remove it mid-run. This only matters while an old and a new binary run side
  by side.
- A Source that adds a run over a branch (ADR-0040's security loop) must hold a
  run lock the same way, or the PR loop will not see that run as live.
- The viewer maps `item-skipped` like `idle`.
