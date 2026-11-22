# ADR-0068: A PR or security-alerts run holds a run lock, as a ticket's run does

- Status: Accepted
- Date: 2026-11-22
- Extends: ADR-0061, ADR-0064, ADR-0066

## Context

ADR-0066 keeps the merged-worktree prune off any worktree a live run is in. It
does this by reading each worktree's run lock, `locks/runs/<slug>.lock`. ADR-0064's
PR-loop reclaim reads the same lock. Only a ticket's run ever took that lock
(`loophost.HoldRun`).

The PR loop and the security-alerts loop also work a worktree of their own,
`.claude/worktrees/<slug>` for the item's key (`PR-<n>` is `pr-<n>`). A PR-loop run
checks the PR's branch out there, rebases it on a conflict, pushes it, watches
CI, and then runs its retrospective in that worktree, with its cleanup last.
Once the pushed PR merges, by a human or by auto-merge, the worktree is merged
and clean, which is all the Consumer's prune script checks for. The run held no
lock, so the shield did not lock the worktree, and another loop's disk reclaim
removed it while the retrospective was still reading it (#416). The session's
next Bash call failed with `Working directory ... was deleted`, and it could not
read the branch's diff.

## Decision

1. **A PR-loop or security-alerts run holds the run lock for its item's key
   from checkout to cleanup, its retrospective included.** `cmd/loop` hands
   each of those Pipelines `loophost.RunLock`, the same `underRunLock` a ticket's
   run takes (`loop.RunLock`). The lock covers the whole `Fix`, so the prune's
   shield locks the worktree while the run is in it, and the run's own cleanup
   still removes it at the end.
2. **A run whose lock another live run holds runs nothing.** As ADR-0061 says
   for a ticket, it narrates `⏸ another live run holds <KEY>'s run lock` and
   comes back RunLive. The loop spends no attempt and counts no failure. A PR
   or security-alerts run gives its lease back on the way out, as it does after
   every run.

## Alternatives considered

- **Point the retrospective at the pushed branch in the primary checkout** (the
  ticket's second option). The session would load the host checkout's skills
  and rules, not the branch's (ADR-0046). ADR-0066 turned this down for the same
  reason.
- **Run the retrospective before the push.** The retrospective mines the whole
  run, the CI watch and its fixes included, so it has to come last.
- **Have the shield lock every worktree under `.claude/worktrees`.** The prune
  could then never reclaim a merged worktree a dead run left, which is what it
  exists for.

## Consequences

- A merged PR whose PR-loop retrospective is still running keeps its worktree
  until the run's cleanup removes it, or until the next prune after the run ends.
- A run that finishes during a prune meets a locked worktree in its cleanup,
  logs `worktree <path> kept`, and leaves it to the next prune (ADR-0066).
- A remover that ignores git's worktree lock still gets past it. The
  retrospective's `⚠` line from ADR-0066 still names that removal in the run log.
