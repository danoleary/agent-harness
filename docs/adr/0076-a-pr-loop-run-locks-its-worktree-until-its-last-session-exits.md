# ADR-0076: A PR-loop run locks its worktree until its last session exits

- Status: Accepted
- Date: 2026-11-28
- Extends: ADR-0066, ADR-0068

## Context

ADR-0068 made a PR-loop run hold the run lock for its item's key from checkout
to cleanup, its retrospective included. Two removers read that lock: the PR
loop's reclaim of a stale worktree (ADR-0064) and the merged-worktree prune's
shield (ADR-0066). The shield `git worktree lock`s each live run's worktree only
while the harness itself runs the prune, and it reads liveness once, when that
prune starts.

In herd it still happened (#478). A pr-doctor run pushed a conflict resolution,
CI went green, and the retrospective started in `.claude/worktrees/pr-<n>`.
About a minute in, the worktree was deleted. The run log had not yet recorded
`retrospective session exited`. The run's own cleanup is deferred until after
the retrospective returns, and a test pins that order. So the remover was
outside the run, and it did not read the run lock. Examples are a Consumer's
prune script run outside the shield (by a human, a cron job or a hook), a loop on
an older binary, or a prune whose liveness snapshot predates the run.

## Decision

1. **A PR-loop run occupies its worktree from checkout until its last session
   exits.** Right after the checkout, `prloop.Fix` runs `git worktree lock` on
   the worktree with the reason `agent-harness: a run's sessions are in it, …`
   (`git.Worktree.Occupy`, `git.OccupiedReason`). It lifts the lock in its
   deferred cleanup, after the retrospective and every other session have
   exited and before the checkpoint and the worktree removal. A remover that
   calls `git worktree remove`, with or without one `--force`, is refused for as
   long as any session is in the worktree, whether or not it reads the run lock.
   One lock spans every session of the run, so no per-session count is needed.
   A lock that cannot be taken is narrated with a `⚠`, and the run goes on
   protected by the run lock alone.
2. **A lock a killed run left is lifted by the next one that can tell.** Before
   the checkout, the next run on the worktree holds its run lock, so no other run
   can be in it. It lifts a lock with that reason and says so
   (`git.Worktree.LiftStaleOccupancy`). Otherwise `CheckoutRemote` would be
   refused, and the PR handed to a human. The prune's shield lifts the same lock
   on a worktree whose run lock is free. It leaves the lock on a live run's
   worktree for the run to lift. A lock with any other reason stays where it is.

## Alternatives considered

- **Lock only for the retrospective.** Every session of the run is exposed in
  the same way, and the gates run in the worktree too. One lock from checkout
  to cleanup is simpler, and the run's own steps (rebase, commit, push) are not
  affected by a worktree lock.
- **Count sessions per worktree in a file under `locks/`.** Only a remover that
  reads the file would honour it, which is the run lock's gap again. git's own
  lock is the one every `git worktree remove` already reads.
- **Re-create the worktree when it is found gone.** The retrospective already
  falls back to origin's copy of the branch (#467), but the session still loses
  its working directory mid-run. Preventing the removal is the fix.

## Consequences

- A remover that forces twice, or deletes the directory with `rm -rf`, still
  gets past the lock. ADR-0066's `⚠` line and #467's fallback to
  `origin/<branch>` still cover that case.
- `git worktree list` shows the run's worktree as `locked` while the run is in
  it. A human who tries to remove it is told why.
- The security-alerts loop and the ticket pipeline do not lock their worktrees
  yet. They keep the run lock and the shield.
